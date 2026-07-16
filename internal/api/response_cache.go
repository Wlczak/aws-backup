package api

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	fileResponseCacheTTL        = 5 * time.Minute
	fileResponseErrorTTL        = 500 * time.Millisecond
	fileResponseCacheMaxBytes   = 128 << 20
	fileResponseCacheMaxEntries = 1024
	cacheEntryOverhead          = 128
)

type responseCacheEntry struct {
	key        string
	revision   uint64
	generation uint64
	body       []byte
	err        error
	expires    time.Time
	size       int64
}

type responseCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	bytes      int64
	maxBytes   int64
	maxEntries int
	successTTL time.Duration
	errorTTL   time.Duration
	now        func() time.Time
	generation uint64
	sf         singleflight.Group
}

func newFileResponseCache() *responseCache {
	return newResponseCache(
		fileResponseCacheMaxBytes,
		fileResponseCacheMaxEntries,
		fileResponseCacheTTL,
		fileResponseErrorTTL,
		time.Now,
	)
}

func newResponseCache(maxBytes int64, maxEntries int, successTTL, errorTTL time.Duration, now func() time.Time) *responseCache {
	return &responseCache{
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		successTTL: successTTL,
		errorTTL:   errorTTL,
		now:        now,
	}
}

// Get returns a response for key at revision, coalescing concurrent misses.
// The cache generation prevents an in-flight load from repopulating entries
// after Clear, which is required when the active profile changes.
func (c *responseCache) Get(ctx context.Context, key string, revision func() uint64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	startRevision := revision()
	generation := c.currentGeneration()
	if body, err, ok := c.lookup(key, startRevision, generation); ok {
		return body, err
	}

	flightKey := fmt.Sprintf("%d:%d:%s", generation, startRevision, key)
	v, err, _ := c.sf.Do(flightKey, func() (any, error) {
		if body, cachedErr, ok := c.lookup(key, startRevision, generation); ok {
			return body, cachedErr
		}
		body, loadErr := load(ctx)
		if loadErr != nil {
			if cacheableResponseError(loadErr) && revision() == startRevision {
				c.store(responseCacheEntry{
					key: key, revision: startRevision, generation: generation,
					err: loadErr, expires: c.now().Add(c.errorTTL),
				})
			}
			return nil, loadErr
		}
		if revision() == startRevision {
			c.store(responseCacheEntry{
				key: key, revision: startRevision, generation: generation,
				body: body, expires: c.now().Add(c.successTTL),
			})
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (c *responseCache) lookup(key string, revision, generation uint64) ([]byte, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return nil, nil, false
	}
	entry := elem.Value.(*responseCacheEntry)
	if entry.revision != revision || entry.generation != generation || !c.now().Before(entry.expires) {
		c.removeLocked(elem)
		return nil, nil, false
	}
	c.lru.MoveToFront(elem)
	return entry.body, entry.err, true
}

func (c *responseCache) store(entry responseCacheEntry) {
	entry.size = int64(len(entry.key) + len(entry.body) + cacheEntryOverhead)
	if entry.err != nil {
		entry.size += int64(len(entry.err.Error()))
	}
	if entry.size > c.maxBytes || c.maxBytes <= 0 || c.maxEntries <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.generation != c.generation {
		return
	}
	if old, ok := c.entries[entry.key]; ok {
		c.removeLocked(old)
	}
	elem := c.lru.PushFront(&entry)
	c.entries[entry.key] = elem
	c.bytes += entry.size
	for c.bytes > c.maxBytes || c.lru.Len() > c.maxEntries {
		c.removeLocked(c.lru.Back())
	}
}

func (c *responseCache) removeLocked(elem *list.Element) {
	if elem == nil {
		return
	}
	entry := elem.Value.(*responseCacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(elem)
	c.bytes -= entry.size
}

func (c *responseCache) currentGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *responseCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]*list.Element)
	c.lru.Init()
	c.bytes = 0
	c.generation++
	c.mu.Unlock()
}

type nonCacheableError interface {
	NonCacheable() bool
}

func cacheableResponseError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var noCache nonCacheableError
	return !errors.As(err, &noCache) || !noCache.NonCacheable()
}
