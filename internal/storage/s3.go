package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	tmtypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// defaultMultipartThreshold is the SDK's hard ceiling on a single
// PutObject body. Above it (or when size is unknown) we go through
// the multipart uploader. Operators can lower the threshold via
// S3Config.MultipartThreshold to get parallel-part throughput and
// finer-grained retry on medium-sized objects.
const defaultMultipartThreshold = 5 * 1024 * 1024 * 1024 // 5 GiB

// DefaultResumeThreshold is the byte size at or above which uploads
// go through the hand-rolled resumable multipart path that persists
// the UploadId across runs. (#162)
const DefaultResumeThreshold int64 = 100 * 1024 * 1024 // 100 MiB

// DefaultPartSize is the byte size of one multipart part on the
// resumable path. 16 MiB hits the S3 sweet spot — large enough that
// 10k parts × 16 MiB ≈ 160 GiB covers the realistic upper end, small
// enough that one stalled part isn't a multi-minute setback. (#162)
const DefaultPartSize int64 = 16 * 1024 * 1024 // 16 MiB

// S3Config holds everything S3Storage needs. The Endpoint field is what
// points the client at MinIO (or another S3-compatible service); set to
// "" to talk to real AWS.
type S3Config struct {
	Endpoint        string
	UsePathStyle    bool
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	StorageClass    string // e.g. DEEP_ARCHIVE, STANDARD
	// MultipartThreshold is the byte size at or above which Put bodies
	// route through the multipart uploader instead of single PutObject.
	// 0 (or any non-positive value) selects defaultMultipartThreshold.
	MultipartThreshold int64
	// ResumeThreshold is the byte size at or above which the engine
	// should pick the resumable multipart path (PutResumable) instead
	// of the single-shot Put. 0 → DefaultResumeThreshold (100 MiB).
	ResumeThreshold int64
	// PartSize is the byte size of one part on the resumable path.
	// 0 → DefaultPartSize (16 MiB).
	PartSize int64
}

// S3Storage is the real S3 / MinIO backend.
type S3Storage struct {
	client             *s3.Client
	uploader           *transfermanager.Client
	bucket             string
	storageClass       s3types.StorageClass
	multipartThreshold int64
	resumeThreshold    int64
	partSize           int64
}

// NewS3Storage builds an S3 client from cfg. When Endpoint is set the
// client talks to that host (MinIO); otherwise it uses AWS resolution.
func NewS3Storage(ctx context.Context, cfg S3Config) (*S3Storage, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: s3 bucket is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("storage: s3 region is required")
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	// Custom HTTP client whose RoundTripper wraps each request body in a
	// counting reader iff the request's context carries a ProgressFunc.
	// This is what gives us upload-progress events at HTTP-frame
	// granularity (16-32 KiB per Read) instead of the SDK's per-part
	// granularity (which can be 64 MiB+ for multipart) — i.e. a smooth
	// bar instead of one update per part. (#)
	httpClient := &http.Client{
		Transport: &progressTransport{inner: http.DefaultTransport},
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
		o.HTTPClient = httpClient
	})

	mt := cfg.MultipartThreshold
	if mt <= 0 {
		mt = defaultMultipartThreshold
	}
	rt := cfg.ResumeThreshold
	if rt <= 0 {
		rt = DefaultResumeThreshold
	}
	ps := cfg.PartSize
	if ps <= 0 {
		ps = DefaultPartSize
	}
	return &S3Storage{
		client:             client,
		uploader:           transfermanager.New(client),
		bucket:             cfg.Bucket,
		storageClass:       s3types.StorageClass(cfg.StorageClass),
		multipartThreshold: mt,
		resumeThreshold:    rt,
		partSize:           ps,
	}, nil
}

// ResumeThreshold returns the byte size at or above which uploads
// should use the resumable multipart path. The engine reads this so
// it can branch without knowing about S3Config internals.
func (s *S3Storage) ResumeThreshold() int64 { return s.resumeThreshold }

// PartSize returns the configured multipart part size.
func (s *S3Storage) PartSize() int64 { return s.partSize }

// Bucket returns the configured bucket name.
func (s *S3Storage) Bucket() string { return s.bucket }

// Client returns the underlying *s3.Client. Exposed so adjacent
// packages can issue bucket-level operations (inventory, lifecycle,
// etc.) without each loading their own AWS credentials.
func (s *S3Storage) Client() *s3.Client { return s.client }

// Put uploads body under key. Bodies <= 5 GiB go through PutObject with a
// server-verified SHA256 checksum. Bodies above that limit (or of unknown
// size, size = -1) are routed through the SDK's multipart uploader so
// arbitrarily large zips don't hit S3's single-PutObject EntityTooLarge
// ceiling. Returns the ETag, size, and (single-shot path) checksum.
func (s *S3Storage) Put(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error) {
	return s.putWithClass(ctx, key, body, size, s.storageClass)
}

// PutStandard uploads body under key with STANDARD storage class, bypassing
// the configured default so zip index sidecars remain instantly readable.
func (s *S3Storage) PutStandard(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error) {
	return s.putWithClass(ctx, key, body, size, s3types.StorageClassStandard)
}

// PutIfAbsent uploads body under key with the configured storage class,
// failing with ErrAlreadyExists if the key is already occupied. Implemented
// via S3's IfNoneMatch="*" precondition; AWS responds with 412 which we
// translate to ErrAlreadyExists. Bodies large enough to need multipart
// (> 5 GiB or unknown size) are not supported here because the SDK's
// multipart manager doesn't surface preconditions cleanly — for those we
// fall back to a HEAD probe + regular put, which is racy but matches
// what bucket-versioning would catch operationally. (#116)
func (s *S3Storage) PutIfAbsent(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error) {
	if size < 0 || size > s.multipartThreshold {
		// Best-effort race-y path for huge / unknown-size bodies.
		if _, err := s.Head(ctx, key); err == nil {
			return PutResult{}, ErrAlreadyExists
		} else if !errors.Is(err, ErrNotFound) {
			return PutResult{}, err
		}
		return s.uploadMultipart(ctx, key, body, size, s.storageClass)
	}

	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              body,
		ContentLength:     aws.Int64(size),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
		StorageClass:      s.storageClass,
		IfNoneMatch:       aws.String("*"),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "PreconditionFailed", "ConditionalRequestConflict":
				return PutResult{}, ErrAlreadyExists
			}
		}
		return PutResult{}, err
	}

	res := PutResult{Key: key, Size: size}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.ChecksumSHA256 != nil {
		if hx, err := base64ToHex(*out.ChecksumSHA256); err == nil {
			res.ChecksumSHA256 = hx
		} else {
			res.ChecksumSHA256 = *out.ChecksumSHA256
		}
	}
	return res, nil
}

func (s *S3Storage) putWithClass(ctx context.Context, key string, body io.Reader, size int64, class s3types.StorageClass) (PutResult, error) {
	// Bodies larger than the 5 GiB single-PutObject limit (or of unknown
	// size) must go through the multipart uploader; PutObject would reject
	// them with EntityTooLarge.
	if size < 0 || size > s.multipartThreshold {
		return s.uploadMultipart(ctx, key, body, size, class)
	}

	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              body,
		ContentLength:     aws.Int64(size),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
		StorageClass:      class,
	})
	if err != nil {
		return PutResult{}, err
	}

	res := PutResult{Key: key, Size: size}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.ChecksumSHA256 != nil {
		// S3 returns the checksum base64-encoded; convert to hex for our
		// DB column so we can compare against a local sha256 hex digest.
		if hx, err := base64ToHex(*out.ChecksumSHA256); err == nil {
			res.ChecksumSHA256 = hx
		} else {
			res.ChecksumSHA256 = *out.ChecksumSHA256
		}
	}
	return res, nil
}

// uploadMultipart uses the SDK's transfer manager to split body into part
// uploads. Setting ChecksumAlgorithm here makes the SDK request a
// composed full-object SHA256 from S3, which AWS verifies on reassembly
// — without it, only per-part MD5s are checked and a silent reassembly
// bug or off-by-one in DEEP_ARCHIVE storage stays undetected for years
// until the operator tries to restore. (#109)
func (s *S3Storage) uploadMultipart(ctx context.Context, key string, body io.Reader, size int64, class s3types.StorageClass) (PutResult, error) {
	out, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              body,
		StorageClass:      tmtypes.StorageClass(class),
		ChecksumAlgorithm: tmtypes.ChecksumAlgorithmSha256,
	})
	if err != nil {
		return PutResult{}, err
	}
	res := PutResult{Key: key, Size: size}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.ChecksumSHA256 != nil {
		if hx, err := base64ToHex(*out.ChecksumSHA256); err == nil {
			res.ChecksumSHA256 = hx
		} else {
			res.ChecksumSHA256 = *out.ChecksumSHA256
		}
	}
	return res, nil
}

// Head returns object metadata or ErrNotFound. ChecksumMode=ENABLED so
// AWS includes the stored SHA256 in the response when present — callers
// use it to dedup byte-identical re-uploads. (#133)
func (s *S3Storage) Head(ctx context.Context, key string) (HeadResult, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		ChecksumMode: s3types.ChecksumModeEnabled,
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NotFound", "NoSuchKey":
				return HeadResult{}, ErrNotFound
			}
		}
		return HeadResult{}, err
	}
	r := HeadResult{
		Key:          key,
		ETag:         aws.ToString(out.ETag),
		StorageClass: string(out.StorageClass),
		Restore:      aws.ToString(out.Restore),
	}
	if out.ContentLength != nil {
		r.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		r.LastModified = *out.LastModified
	}
	if out.ChecksumSHA256 != nil {
		if hx, err := base64ToHex(*out.ChecksumSHA256); err == nil {
			r.ChecksumSHA256 = hx
		} else {
			r.ChecksumSHA256 = *out.ChecksumSHA256
		}
	}
	return r, nil
}

// HeadBucket verifies the bucket is reachable with the configured
// credentials and returns the underlying SDK error otherwise. Used by
// the API connectivity-test endpoint.
func (s *S3Storage) HeadBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	return err
}

// Restore triggers a Glacier restore for the given key. This is the only
// code path that will talk to real AWS in production; it remains unused
// until the Restore feature (19) is enabled.
func (s *S3Storage) Restore(ctx context.Context, key string, days int) error {
	_, err := s.client.RestoreObject(ctx, &s3.RestoreObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		RestoreRequest: &s3types.RestoreRequest{
			Days: aws.Int32(int32(days)),
			GlacierJobParameters: &s3types.GlacierJobParameters{
				Tier: s3types.TierStandard,
			},
		},
	})
	return err
}

// List returns every object key in the bucket with the given prefix.
// Pass "" to list all keys. Pages through ListObjectsV2 automatically.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}
	return keys, nil
}

// Get downloads the object at key and returns the response body.
// The caller must close the returned ReadCloser.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NotFound", "NoSuchKey":
				return nil, ErrNotFound
			case "InvalidObjectState":
				// Object is in a Glacier tier and either has no active
				// restore or the restore is still in progress. (#175)
				return nil, ErrGlacierThawing
			}
		}
		return nil, err
	}
	return out.Body, nil
}

// Delete removes the object at key. It is a no-op if the key does not exist.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

// Close is a no-op; the SDK's HTTP client doesn't require teardown.
func (s *S3Storage) Close() error { return nil }

// base64ToHex converts base64 (std, padded) to lower-case hex.
func base64ToHex(b64 string) (string, error) {
	b64 = strings.TrimSpace(b64)
	raw, err := decodeBase64(b64)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
