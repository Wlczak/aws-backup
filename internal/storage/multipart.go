// Resumable multipart upload path. The SDK's transfer manager hides
// the UploadId, which is exactly the piece we need to resume across
// runs — so this file goes a level lower and drives
// CreateMultipartUpload + UploadPart + CompleteMultipartUpload
// directly. The engine persists the UploadId between calls so a
// crash mid-upload doesn't waste the parts S3 already accepted.
//
// Layout:
//   * MultipartUpload + the small wrappers (Create / ListParts /
//     UploadPart / Complete / Abort) — direct passthroughs.
//   * PutResumable — the orchestrator: validates the local file's
//     SHA256 against any pre-existing parts, fans out the remaining
//     parts across a worker pool, and finalises the upload.

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// MultipartUpload is the small ticket that describes one in-flight
// resumable upload. The engine persists this in the index so the
// next run can hand it back to PutResumable.
type MultipartUpload struct {
	Key      string
	UploadID string
	PartSize int64
}

// ErrChecksumMismatch is returned by PutResumable when the local
// tmp file's SHA256 differs from the checksum of an already-uploaded
// part — the only safe response is to abort the old upload and
// restart from scratch. Surface as its own error so callers can do
// exactly that in one place.
var ErrChecksumMismatch = errors.New("storage: local file sha256 does not match S3 multipart parts")

// CreateMultipart starts a fresh multipart upload and returns its
// UploadId. The caller is expected to persist (Key, UploadID,
// PartSize, full-file SHA256) before issuing the first UploadPart so
// a crash before part 1 still leaves a recoverable record.
func (s *S3Storage) CreateMultipart(ctx context.Context, key string, class s3types.StorageClass, partSize int64) (MultipartUpload, error) {
	if partSize <= 0 {
		partSize = s.partSize
	}
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		StorageClass:      class,
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("create multipart: %w", err)
	}
	return MultipartUpload{
		Key:      key,
		UploadID: aws.ToString(out.UploadId),
		PartSize: partSize,
	}, nil
}

// ListUploadedParts returns every part S3 currently holds for the
// upload, in PartNumber order. Used on resume to learn what's
// already done before queuing the rest. Treats a missing upload
// (NoSuchUpload — the upload was aborted by lifecycle policy or
// another caller) as "nothing uploaded yet".
func (s *S3Storage) ListUploadedParts(ctx context.Context, mu MultipartUpload) ([]s3types.Part, error) {
	var parts []s3types.Part
	var marker *string
	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(s.bucket),
			Key:              aws.String(mu.Key),
			UploadId:         aws.String(mu.UploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
				return nil, nil
			}
			return nil, fmt.Errorf("list parts: %w", err)
		}
		parts = append(parts, out.Parts...)
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		marker = out.NextPartNumberMarker
	}
	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})
	return parts, nil
}

// UploadPart uploads one part. body must report the same length as
// size; both are passed in to avoid an extra Seek round-trip on a
// *os.File reader. checksumB64 is the base64-encoded SHA256 the
// caller has already computed over body — passing it in lets us
// pre-compute on the same goroutine that read the bytes, so the
// hash and the upload share a CPU cache line.
func (s *S3Storage) UploadPart(ctx context.Context, mu MultipartUpload, partNum int32, body io.ReadSeeker, size int64, checksumB64 string) (s3types.CompletedPart, error) {
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(mu.Key),
		UploadId:       aws.String(mu.UploadID),
		PartNumber:     aws.Int32(partNum),
		Body:           body,
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(checksumB64),
	})
	if err != nil {
		return s3types.CompletedPart{}, fmt.Errorf("upload part %d: %w", partNum, err)
	}
	return s3types.CompletedPart{
		PartNumber:     aws.Int32(partNum),
		ETag:           out.ETag,
		ChecksumSHA256: aws.String(checksumB64),
	}, nil
}

// CompleteMultipart finalises the upload. parts must be sorted by
// PartNumber. Returns a PutResult that mirrors what the single-shot
// Put returns so callers can collapse both paths.
func (s *S3Storage) CompleteMultipart(ctx context.Context, mu MultipartUpload, parts []s3types.CompletedPart, size int64) (PutResult, error) {
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(mu.Key),
		UploadId:        aws.String(mu.UploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return PutResult{}, fmt.Errorf("complete multipart: %w", err)
	}
	res := PutResult{Key: mu.Key, Size: size}
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

// AbortMultipart cleans up a multipart upload we no longer want to
// keep paying storage on. Idempotent: a NoSuchUpload error is
// reported as success because the upload is already gone.
func (s *S3Storage) AbortMultipart(ctx context.Context, mu MultipartUpload) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(mu.Key),
		UploadId: aws.String(mu.UploadID),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
			return nil
		}
		return fmt.Errorf("abort multipart: %w", err)
	}
	return nil
}

// ResumableOptions tunes one PutResumable call. Zero values pick
// the S3Storage defaults (configurable via S3Config).
type ResumableOptions struct {
	// UploadID is the persisted UploadId from a prior interrupted
	// run, or "" for a fresh upload. The caller is responsible for
	// persisting the new UploadId before the first part lands —
	// hand it back to ResumableOptions.OnUploadID below.
	UploadID string
	// PartSize overrides S3Storage.partSize. Must match the size
	// used for previously-uploaded parts when resuming.
	PartSize int64
	// Parallel is the number of concurrent UploadPart workers.
	// 0 → 4.
	Parallel int
	// StorageClass overrides the configured default.
	StorageClass s3types.StorageClass
	// OnUploadID fires once a fresh CreateMultipartUpload returns,
	// before any UploadPart — the engine uses it to persist the
	// UploadId synchronously so a crash before the first part still
	// leaves a recoverable row.
	OnUploadID func(uploadID string) error
	// OnProgress fires after each part lands. Bytes is cumulative
	// across both freshly-uploaded and already-resumed parts.
	OnProgress func(bytes, total int64)
}

// PutResumable is the entry point the engine calls instead of Put
// for any file/zip whose size >= S3Storage.ResumeThreshold(). body
// must be a *os.File so individual parts can be read at arbitrary
// offsets via ReadAt without buffering the whole file.
//
// Flow:
//
//  1. If opts.UploadID is empty, call CreateMultipartUpload and fire
//     opts.OnUploadID synchronously so the caller persists the
//     UploadId before any part work begins.
//  2. ListUploadedParts to learn what S3 already holds. For each
//     existing part, re-hash the corresponding chunk of the local
//     file and compare to the part's stored ChecksumSHA256. Any
//     mismatch returns ErrChecksumMismatch — the caller aborts and
//     restarts (a tampered tmp must never be partially attributed
//     to a stale UploadId).
//  3. Upload the remaining parts across a worker pool, computing
//     each part's SHA256 on the worker so we never pay for a second
//     full read.
//  4. CompleteMultipartUpload with the assembled (resumed +
//     uploaded) parts list.
//
// Returns the final PutResult plus the UploadId actually used so
// callers can confirm what they persisted.
func (s *S3Storage) PutResumable(ctx context.Context, key string, body *os.File, size int64, opts ResumableOptions) (PutResult, string, error) {
	partSize := opts.PartSize
	if partSize <= 0 {
		partSize = s.partSize
	}
	if partSize < 5*1024*1024 {
		return PutResult{}, "", fmt.Errorf("storage: part size %d below S3 minimum 5 MiB", partSize)
	}
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 4
	}
	class := opts.StorageClass
	if class == "" {
		class = s.storageClass
	}

	mu := MultipartUpload{Key: key, UploadID: opts.UploadID, PartSize: partSize}
	if mu.UploadID == "" {
		fresh, err := s.CreateMultipart(ctx, key, class, partSize)
		if err != nil {
			return PutResult{}, "", err
		}
		mu = fresh
		if opts.OnUploadID != nil {
			if err := opts.OnUploadID(mu.UploadID); err != nil {
				_ = s.AbortMultipart(ctx, mu)
				return PutResult{}, "", fmt.Errorf("persist upload id: %w", err)
			}
		}
	}

	totalParts := int32((size + partSize - 1) / partSize)
	if size == 0 {
		totalParts = 1
	}

	existing, err := s.ListUploadedParts(ctx, mu)
	if err != nil {
		return PutResult{}, mu.UploadID, err
	}
	completed := make([]s3types.CompletedPart, 0, totalParts)
	already := map[int32]struct{}{}
	var bytesDone int64

	// Validate every already-uploaded part: re-read the matching
	// slice of the local file and compare SHA256 against what S3
	// stored. Defensive — the caller already checked the full-file
	// SHA matches the persisted record before calling us, so a
	// part-level mismatch here means S3 saw bytes the local file
	// no longer has. (#162: tightened scope at user request.)
	for _, p := range existing {
		num := aws.ToInt32(p.PartNumber)
		if num < 1 || num > totalParts {
			continue
		}
		offset := int64(num-1) * partSize
		end := offset + partSize
		if end > size {
			end = size
		}
		buf := make([]byte, end-offset)
		if _, err := body.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
			return PutResult{}, mu.UploadID, fmt.Errorf("read part %d for verify: %w", num, err)
		}
		sum := sha256.Sum256(buf)
		want := base64.StdEncoding.EncodeToString(sum[:])
		got := aws.ToString(p.ChecksumSHA256)
		if got == "" || got != want {
			return PutResult{}, mu.UploadID, fmt.Errorf("part %d: %w", num, ErrChecksumMismatch)
		}
		completed = append(completed, s3types.CompletedPart{
			PartNumber:     aws.Int32(num),
			ETag:           p.ETag,
			ChecksumSHA256: p.ChecksumSHA256,
		})
		already[num] = struct{}{}
		bytesDone += end - offset
	}
	if opts.OnProgress != nil {
		opts.OnProgress(bytesDone, size)
	}

	type job struct{ partNum int32 }
	jobs := make(chan job)
	results := make(chan s3types.CompletedPart, totalParts)
	errCh := make(chan error, parallel)

	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				offset := int64(j.partNum-1) * partSize
				end := offset + partSize
				if end > size {
					end = size
				}
				buf := make([]byte, end-offset)
				if _, err := body.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
					errCh <- fmt.Errorf("read part %d: %w", j.partNum, err)
					return
				}
				sum := sha256.Sum256(buf)
				cs := base64.StdEncoding.EncodeToString(sum[:])
				cp, err := s.UploadPart(ctx, mu, j.partNum, &byteSeeker{b: buf}, int64(len(buf)), cs)
				if err != nil {
					errCh <- err
					return
				}
				results <- cp
			}
		}()
	}

	go func() {
		defer close(jobs)
		for n := int32(1); n <= totalParts; n++ {
			if _, ok := already[n]; ok {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case jobs <- job{partNum: n}:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
		close(errCh)
	}()

	for cp := range results {
		completed = append(completed, cp)
		// approximate; precise per-part size = partSize for all but last
		num := aws.ToInt32(cp.PartNumber)
		offset := int64(num-1) * partSize
		end := offset + partSize
		if end > size {
			end = size
		}
		bytesDone += end - offset
		if opts.OnProgress != nil {
			opts.OnProgress(bytesDone, size)
		}
	}
	for err := range errCh {
		if err != nil {
			return PutResult{}, mu.UploadID, err
		}
	}
	if err := ctx.Err(); err != nil {
		return PutResult{}, mu.UploadID, err
	}

	sort.Slice(completed, func(i, j int) bool {
		return aws.ToInt32(completed[i].PartNumber) < aws.ToInt32(completed[j].PartNumber)
	})

	res, err := s.CompleteMultipart(ctx, mu, completed, size)
	if err != nil {
		return PutResult{}, mu.UploadID, err
	}
	return res, mu.UploadID, nil
}

// byteSeeker is the lightweight io.ReadSeeker wrapper around a part's
// in-memory bytes — UploadPartInput.Body needs Seek so the SDK can
// rewind after a sigv4 retry.
type byteSeeker struct {
	b   []byte
	pos int64
}

func (s *byteSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *byteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		abs = int64(len(s.b)) + offset
	default:
		return 0, fmt.Errorf("byteSeeker: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("byteSeeker: negative position")
	}
	s.pos = abs
	return abs, nil
}

// HashFileSHA256 returns the hex SHA256 of the file at path. Helper
// used by the engine before calling PutResumable to validate the
// local tmp matches the persisted record. Streams the file so it
// works on multi-GB tmps without pinning memory.
func HashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
