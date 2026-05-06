// Package inventory wraps S3 bucket-inventory configuration and reads
// the most recent generated manifest so the operator can drive a
// restore-status reconciliation off a pre-built listing instead of
// paying for a full ListObjectsV2 sweep.
//
// Inventory is configured on the same backup bucket with destination
// prefix `_inventory/` so the operator does not have to provision a
// separate bucket. The configuration ID is fixed
// ("aws-backup-restore-status") so Get / Put / Delete always address
// the same record. AWS does not expose restore status as an inventory
// optional field, so the manifest is used only to enumerate keys; the
// scanner still HEADs each one to read x-amz-restore.
package inventory

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ConfigID is the fixed inventory configuration name we read/write.
// Hardcoded so Get/Put/Delete always address the same record without an
// extra config knob.
const ConfigID = "aws-backup-restore-status"

// DestinationPrefix is the S3 prefix on the source bucket where AWS
// writes inventory manifests + data files.
const DestinationPrefix = "_inventory/"

// Frequency is how often AWS regenerates the inventory.
type Frequency string

const (
	FrequencyDaily  Frequency = "Daily"
	FrequencyWeekly Frequency = "Weekly"
)

// ParseFrequency normalises a UI-supplied frequency to one of the two
// supported values.
func ParseFrequency(s string) (Frequency, error) {
	switch strings.ToLower(s) {
	case "daily":
		return FrequencyDaily, nil
	case "weekly":
		return FrequencyWeekly, nil
	}
	return "", fmt.Errorf("inventory: unsupported frequency %q", s)
}

// Status describes the bucket's current inventory configuration.
type Status struct {
	Enabled     bool      `json:"enabled"`
	ID          string    `json:"id,omitempty"`
	Frequency   Frequency `json:"frequency,omitempty"`
	Destination string    `json:"destination,omitempty"` // s3://bucket/prefix
	Format      string    `json:"format,omitempty"`
}

// API is the slice of *s3.Client this package uses. Exported so tests
// can stub it and so callers can build a Manager from a hand-rolled
// fake without depending on the AWS SDK type.
type API interface {
	GetBucketInventoryConfiguration(ctx context.Context, in *s3.GetBucketInventoryConfigurationInput, opts ...func(*s3.Options)) (*s3.GetBucketInventoryConfigurationOutput, error)
	PutBucketInventoryConfiguration(ctx context.Context, in *s3.PutBucketInventoryConfigurationInput, opts ...func(*s3.Options)) (*s3.PutBucketInventoryConfigurationOutput, error)
	DeleteBucketInventoryConfiguration(ctx context.Context, in *s3.DeleteBucketInventoryConfigurationInput, opts ...func(*s3.Options)) (*s3.DeleteBucketInventoryConfigurationOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Manager owns the inventory lifecycle for a single bucket. The
// resolver returns the live S3 client + bucket pair so a settings
// hot-swap (PUT /api/settings) is observed on the next call without
// rebuilding the manager.
type Manager struct {
	resolve   func() (API, string, bool)
	accountID string // used only when AWS rejects PUT without an explicit account hint; optional.
}

// New builds a Manager. resolver returns the live (s3 client, bucket)
// pair plus a "ready" flag — return false to make every method respond
// with ErrNotReady. accountID may be empty; AWS infers it from
// credentials in most cases.
func New(resolver func() (API, string, bool), accountID string) *Manager {
	return &Manager{resolve: resolver, accountID: accountID}
}

// ErrNotReady is returned when no live S3 client is available (e.g. a
// settings change is mid-swap).
var ErrNotReady = errors.New("inventory: storage not configured")

// snapshot returns the live (client, bucket) pair or ErrNotReady.
func (m *Manager) snapshot() (API, string, error) {
	c, b, ok := m.resolve()
	if !ok || c == nil || b == "" {
		return nil, "", ErrNotReady
	}
	return c, b, nil
}

// Get returns the current inventory configuration on the bucket, or
// {Enabled: false} when no record exists. A NoSuchConfiguration error
// is treated as "not configured" so callers can render a clean
// "Disabled" badge without inspecting AWS error codes themselves.
func (m *Manager) Get(ctx context.Context) (Status, error) {
	client, bucket, err := m.snapshot()
	if err != nil {
		return Status{}, err
	}
	out, err := client.GetBucketInventoryConfiguration(ctx, &s3.GetBucketInventoryConfigurationInput{
		Bucket: aws.String(bucket),
		Id:     aws.String(ConfigID),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NoSuchConfiguration", "NoSuchBucket":
				return Status{Enabled: false}, nil
			}
		}
		return Status{}, fmt.Errorf("get inventory configuration: %w", err)
	}
	cfg := out.InventoryConfiguration
	if cfg == nil {
		return Status{Enabled: false}, nil
	}
	st := Status{
		Enabled: aws.ToBool(cfg.IsEnabled),
		ID:      aws.ToString(cfg.Id),
	}
	if cfg.Schedule != nil {
		st.Frequency = Frequency(cfg.Schedule.Frequency)
	}
	if cfg.Destination != nil && cfg.Destination.S3BucketDestination != nil {
		dest := cfg.Destination.S3BucketDestination
		bkt := strings.TrimPrefix(aws.ToString(dest.Bucket), "arn:aws:s3:::")
		prefix := aws.ToString(dest.Prefix)
		st.Destination = fmt.Sprintf("s3://%s/%s", bkt, prefix)
		st.Format = string(dest.Format)
	}
	return st, nil
}

// Put installs (or replaces) the inventory configuration with the given
// frequency. Destination is `_inventory/` on the same bucket so the
// operator never has to provision a second bucket.
func (m *Manager) Put(ctx context.Context, freq Frequency) error {
	client, bucket, err := m.snapshot()
	if err != nil {
		return err
	}
	bucketARN := "arn:aws:s3:::" + bucket
	cfg := &s3types.InventoryConfiguration{
		Id:        aws.String(ConfigID),
		IsEnabled: aws.Bool(true),
		IncludedObjectVersions: s3types.InventoryIncludedObjectVersionsCurrent,
		Schedule: &s3types.InventorySchedule{
			Frequency: s3types.InventoryFrequency(freq),
		},
		Destination: &s3types.InventoryDestination{
			S3BucketDestination: &s3types.InventoryS3BucketDestination{
				Bucket: aws.String(bucketARN),
				Format: s3types.InventoryFormatCsv,
				Prefix: aws.String(strings.TrimSuffix(DestinationPrefix, "/")),
			},
		},
		OptionalFields: []s3types.InventoryOptionalField{
			s3types.InventoryOptionalFieldSize,
			s3types.InventoryOptionalFieldLastModifiedDate,
			s3types.InventoryOptionalFieldStorageClass,
		},
	}
	if m.accountID != "" {
		cfg.Destination.S3BucketDestination.AccountId = aws.String(m.accountID)
	}
	_, err = client.PutBucketInventoryConfiguration(ctx, &s3.PutBucketInventoryConfigurationInput{
		Bucket:                 aws.String(bucket),
		Id:                     aws.String(ConfigID),
		InventoryConfiguration: cfg,
	})
	if err != nil {
		return fmt.Errorf("put inventory configuration: %w", err)
	}
	return nil
}

// Delete removes the inventory configuration. Idempotent: a missing
// configuration is reported as success so the UI's "Disable" button is
// safe to click twice.
func (m *Manager) Delete(ctx context.Context) error {
	client, bucket, err := m.snapshot()
	if err != nil {
		return err
	}
	_, err = client.DeleteBucketInventoryConfiguration(ctx, &s3.DeleteBucketInventoryConfigurationInput{
		Bucket: aws.String(bucket),
		Id:     aws.String(ConfigID),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			if apiErr.ErrorCode() == "NoSuchConfiguration" {
				return nil
			}
		}
		return fmt.Errorf("delete inventory configuration: %w", err)
	}
	return nil
}

// ListLatestKeys locates the most recent inventory manifest under
// `_inventory/<bucket>/<id>/`, parses it, and streams every CSV data
// file to assemble the list of source object keys. The returned slice
// excludes any inventory artefacts under `_inventory/` itself so the
// caller does not waste HEADs on its own metadata.
func (m *Manager) ListLatestKeys(ctx context.Context) ([]string, error) {
	client, bucket, err := m.snapshot()
	if err != nil {
		return nil, err
	}
	manifestKey, err := findLatestManifest(ctx, client, bucket)
	if err != nil {
		return nil, err
	}
	if manifestKey == "" {
		return nil, errors.New("no inventory manifest found yet — first report can take up to 48h after enabling")
	}

	mf, err := fetchManifest(ctx, client, bucket, manifestKey)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", manifestKey, err)
	}

	schema := parseSchema(mf.FileSchema)
	keyCol, ok := schema["Key"]
	if !ok {
		return nil, fmt.Errorf("manifest schema missing Key column: %q", mf.FileSchema)
	}

	var keys []string
	for _, f := range mf.Files {
		dataKeys, err := fetchDataKeys(ctx, client, bucket, f.Key, keyCol)
		if err != nil {
			return nil, fmt.Errorf("fetch data %s: %w", f.Key, err)
		}
		for _, k := range dataKeys {
			if strings.HasPrefix(k, DestinationPrefix) {
				// Skip inventory's own output to avoid HEADing every
				// manifest + data file we just wrote.
				continue
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// findLatestManifest returns the most recent manifest.json under
// `_inventory/<bucket>/<id>/`. Returns "" with no error when no
// manifests have been generated yet.
func findLatestManifest(ctx context.Context, client API, bucket string) (string, error) {
	prefix := DestinationPrefix + bucket + "/" + ConfigID + "/"
	var manifests []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return "", fmt.Errorf("list inventory output: %w", err)
		}
		for _, obj := range out.Contents {
			k := aws.ToString(obj.Key)
			if strings.HasSuffix(k, "/manifest.json") {
				manifests = append(manifests, k)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	if len(manifests) == 0 {
		return "", nil
	}
	// Inventory writes manifests under `<prefix>/<YYYY-MM-DDTHH-MMZ>/`,
	// so lexicographic order is chronological — last entry wins.
	sort.Strings(manifests)
	return manifests[len(manifests)-1], nil
}

type manifest struct {
	SourceBucket string         `json:"sourceBucket"`
	FileFormat   string         `json:"fileFormat"`
	FileSchema   string         `json:"fileSchema"`
	Files        []manifestFile `json:"files"`
}

type manifestFile struct {
	Key string `json:"key"`
}

func fetchManifest(ctx context.Context, client API, bucket, key string) (*manifest, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	var mf manifest
	if err := json.Unmarshal(body, &mf); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &mf, nil
}

// parseSchema turns "Bucket, Key, Size" into a column-name → index map.
// S3 Inventory's manifest schema field sometimes wraps individual column
// names in double quotes (e.g. `"Bucket", "Key", "Size"`); strip them so
// downstream lookups by bare name still match. (#190)
func parseSchema(s string) map[string]int {
	out := map[string]int{}
	for i, col := range strings.Split(s, ",") {
		name := strings.TrimSpace(col)
		name = strings.Trim(name, `"`)
		name = strings.TrimSpace(name)
		out[name] = i
	}
	return out
}

// fetchDataKeys downloads one inventory data file (gzip CSV) and pulls
// the value at keyCol from each row.
func fetchDataKeys(ctx context.Context, client API, bucket, key string, keyCol int) ([]string, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	gz, err := gzip.NewReader(out.Body)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	r := csv.NewReader(gz)
	r.FieldsPerRecord = -1 // some inventory rows are jagged across schema versions
	var keys []string
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: %w", err)
		}
		if keyCol >= len(row) {
			continue
		}
		keys = append(keys, row[keyCol])
	}
	return keys, nil
}
