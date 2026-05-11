// Package storage abstracts the backup destination. S3 (or S3-compatible
// MinIO during development) is the only implementation that talks over
// the network; MemStorage is an in-process fake the engine tests use.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// RestoreTier selects the Glacier retrieval speed/cost tradeoff when
// requesting a thaw for archived objects.
type RestoreTier string

const (
	RestoreTierStandard RestoreTier = "standard"
	RestoreTierBulk     RestoreTier = "bulk"
)

// ErrNotFound is returned by Head / GetRestoreStatus when a key is absent.
var ErrNotFound = errors.New("storage: key not found")

// ErrGlacierThawing is returned by Get when the object lives in a Glacier
// tier and is not yet (or no longer) restored to a downloadable state.
// Callers (the restore engine) translate it into an operator-friendly
// "still thawing" message instead of leaking the raw SDK error. (#175)
var ErrGlacierThawing = errors.New("storage: object is in glacier and not restored")

// ErrAlreadyExists is returned by PutIfAbsent when an object already
// exists at the requested key. Callers (the engine's zip path) treat it
// as a signal to advance to the next counter slot rather than silently
// overwriting a previous DEEP_ARCHIVE object that may differ in
// content. (#116)
var ErrAlreadyExists = errors.New("storage: key already exists")

// ErrRestoreInProgress is returned by Restore when S3 reports
// RestoreAlreadyInProgress: the key already has an active restore.
// Callers should treat this as a soft-success and not as a failure. (#242)
var ErrRestoreInProgress = errors.New("storage: restore already in progress")

// ErrNotArchived is returned by Restore when the object's storage class
// doesn't need (or accept) restoration — STANDARD, or already-restored.
// Callers should treat this as 'nothing to do, the bytes are already
// available' rather than a failure. (#242)
var ErrNotArchived = errors.New("storage: object not in archive tier")

// PutResult reports what the destination acknowledged after an upload.
type PutResult struct {
	Key            string
	ETag           string
	ChecksumSHA256 string // hex-encoded, may be empty if backend didn't echo it
	Size           int64
}

// HeadResult is metadata returned by Head.
type HeadResult struct {
	Key            string
	Size           int64
	ETag           string
	StorageClass   string
	ChecksumSHA256 string // hex-encoded; empty if backend didn't echo it
	LastModified   time.Time
	// Restore is the raw S3 x-amz-restore header, e.g.
	// `ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`
	// when the object has been restored from Glacier, or
	// `ongoing-request="true"` while the restore is still in progress.
	// Empty when the object has never been restored or has cooled back
	// to the archive tier.
	Restore string
}

// Storage is the subset of S3-like operations aws-backup needs.
type Storage interface {
	// Put uploads body under key. size MAY be -1 (unknown); implementations
	// that need it can buffer or error out.
	Put(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error)

	// PutStandard uploads body under key with the STANDARD storage class
	// regardless of the configured default. Used for small sidecar objects
	// (zip index files) that must stay instantly retrievable while the
	// main payload sits in a cold tier like DEEP_ARCHIVE.
	PutStandard(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error)

	// PutIfAbsent uploads body under key only if no object exists there
	// already. Returns ErrAlreadyExists if the key is occupied. Used by
	// the engine's zip path so a retry under the same key can't
	// silently replace a prior DEEP_ARCHIVE object whose content may
	// differ — the engine then advances to a fresh counter slot. (#116)
	PutIfAbsent(ctx context.Context, key string, body io.Reader, size int64) (PutResult, error)

	// Head returns metadata or ErrNotFound.
	Head(ctx context.Context, key string) (HeadResult, error)

	// Restore issues a Glacier-style restore request for `days` using the
	// chosen retrieval tier. Returns ErrUnsupported on storage classes
	// that don't need restoration.
	Restore(ctx context.Context, key string, days int, tier RestoreTier) error

	// List returns every object key in the bucket whose key starts with
	// prefix. Pass "" to list everything. Used by the index-sync operation.
	List(ctx context.Context, prefix string) ([]string, error)

	// Get downloads the object at key and returns a ReadCloser. Caller must
	// close the body. Returns ErrNotFound when the key is absent.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object at key. A no-op if the key does not exist.
	Delete(ctx context.Context, key string) error

	// Close releases any underlying client resources.
	Close() error
}

// ErrUnsupported is returned by operations a given backend does not implement.
var ErrUnsupported = errors.New("storage: operation unsupported by this backend")

// ResumableStorage is the optional capability the engine type-asserts
// for to decide whether large files should go through PutResumable.
// MemStorage and any future test fakes that don't implement it
// transparently keep the single-shot path. (#162)
type ResumableStorage interface {
	ResumeThreshold() int64
	PartSize() int64
}
