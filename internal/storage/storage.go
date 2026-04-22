// Package storage abstracts the backup destination. S3 (or S3-compatible
// MinIO during development) is the only implementation that talks over
// the network; MemStorage is an in-process fake the engine tests use.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Head / GetRestoreStatus when a key is absent.
var ErrNotFound = errors.New("storage: key not found")

// PutResult reports what the destination acknowledged after an upload.
type PutResult struct {
	Key            string
	ETag           string
	ChecksumSHA256 string // hex-encoded, may be empty if backend didn't echo it
	Size           int64
}

// HeadResult is metadata returned by Head.
type HeadResult struct {
	Key          string
	Size         int64
	ETag         string
	StorageClass string
}

// Storage is the subset of S3-like operations aws-backup needs.
type Storage interface {
	// Put uploads body under key. size MAY be -1 (unknown); implementations
	// that need it can buffer or error out.
	Put(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error)

	// Head returns metadata or ErrNotFound.
	Head(ctx context.Context, key string) (HeadResult, error)

	// Restore issues a Glacier-style restore request for `days`. Returns
	// ErrUnsupported on storage classes that don't need restoration.
	Restore(ctx context.Context, key string, days int) error

	// Close releases any underlying client resources.
	Close() error
}

// ErrUnsupported is returned by operations a given backend does not implement.
var ErrUnsupported = errors.New("storage: operation unsupported by this backend")
