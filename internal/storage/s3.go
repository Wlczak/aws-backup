package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

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
}

// S3Storage is the real S3 / MinIO backend.
type S3Storage struct {
	client       *s3.Client
	bucket       string
	storageClass s3types.StorageClass
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

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3Storage{
		client:       client,
		bucket:       cfg.Bucket,
		storageClass: s3types.StorageClass(cfg.StorageClass),
	}, nil
}

// Put uploads body under key via the SDK's multipart uploader with a
// server-verified SHA256 checksum. Returns the ETag, size, and checksum
// echoed by the service.
func (s *S3Storage) Put(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error) {
	return s.putWithClass(ctx, key, body, size, s.storageClass)
}

// PutStandard uploads body under key with STANDARD storage class, bypassing
// the configured default so zip index sidecars remain instantly readable.
func (s *S3Storage) PutStandard(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error) {
	return s.putWithClass(ctx, key, body, size, s3types.StorageClassStandard)
}

func (s *S3Storage) putWithClass(ctx context.Context, key string, body io.Reader, _ int64, class s3types.StorageClass) (PutResult, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              body,
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
		StorageClass:      class,
	})
	if err != nil {
		return PutResult{}, err
	}

	res := PutResult{Key: key}
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

// Head returns object metadata or ErrNotFound.
func (s *S3Storage) Head(ctx context.Context, key string) (HeadResult, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
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
	r := HeadResult{Key: key, ETag: aws.ToString(out.ETag), StorageClass: string(out.StorageClass)}
	if out.ContentLength != nil {
		r.Size = *out.ContentLength
	}
	return r, nil
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
			}
		}
		return nil, err
	}
	return out.Body, nil
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
