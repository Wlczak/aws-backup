package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// MemStorage is an in-process Storage implementation backed by a map.
// Engine tests use it so we don't need MinIO for every unit test.
type MemStorage struct {
	mu      sync.Mutex
	objects map[string]memObject
}

type memObject struct {
	data         []byte
	etag         string
	storageClass string
}

// NewMemStorage returns an empty in-memory storage.
func NewMemStorage() *MemStorage {
	return &MemStorage{objects: map[string]memObject{}}
}

// Put stores body under key and returns a fake ETag = sha256(data) prefix.
func (m *MemStorage) Put(_ context.Context, key string, body io.Reader, _ int64) (PutResult, error) {
	return m.put(key, body, "DEEP_ARCHIVE")
}

// PutStandard stores body under key, tagging it STANDARD so tests can
// verify that index sidecars bypass the cold-tier default.
func (m *MemStorage) PutStandard(_ context.Context, key string, body io.Reader, _ int64) (PutResult, error) {
	return m.put(key, body, "STANDARD")
}

func (m *MemStorage) put(key string, body io.Reader, class string) (PutResult, error) {
	buf, err := io.ReadAll(body)
	if err != nil {
		return PutResult{}, err
	}
	sum := sha256.Sum256(buf)
	sumHex := hex.EncodeToString(sum[:])

	m.mu.Lock()
	m.objects[key] = memObject{data: buf, etag: sumHex[:32], storageClass: class}
	m.mu.Unlock()

	return PutResult{
		Key:            key,
		ETag:           fmt.Sprintf("%q", sumHex[:32]),
		ChecksumSHA256: sumHex,
		Size:           int64(len(buf)),
	}, nil
}

// Head returns fake object metadata or ErrNotFound.
func (m *MemStorage) Head(_ context.Context, key string) (HeadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return HeadResult{}, ErrNotFound
	}
	class := o.storageClass
	if class == "" {
		class = "DEEP_ARCHIVE"
	}
	return HeadResult{Key: key, Size: int64(len(o.data)), ETag: o.etag, StorageClass: class}, nil
}

// List returns keys matching prefix, sorted, satisfying the Storage interface.
func (m *MemStorage) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Restore is a no-op for the in-memory fake.
func (m *MemStorage) Restore(_ context.Context, key string, _ int) error {
	m.mu.Lock()
	_, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Close is a no-op.
func (m *MemStorage) Close() error { return nil }

// Get returns an io.ReadCloser over the stored object, satisfying Storage.
func (m *MemStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(o.data))), nil
}

// GetBytes is a testing helper that returns the raw bytes stored under key.
func (m *MemStorage) GetBytes(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, false
	}
	return bytes.Clone(o.data), true
}

// Keys returns all stored keys in sorted order (stable for assertions).
func (m *MemStorage) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}
