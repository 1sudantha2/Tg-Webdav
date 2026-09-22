// Package stream implements the high-performance streaming pipeline between
// Telegram MTProto storage and HTTP clients: a sliding-window in-memory chunk
// cache and an io.ReadSeeker adapter that supports instant Range seeking.
package stream

import (
	"container/list"
	"context"
	"sync"
)

// ChunkSize is the granularity of cached chunks (4 MiB). A chunk is served
// from Telegram with one or more aligned 1 MiB upload.getFile requests.
const ChunkSize = 4 << 20

// FetchFunc fetches exactly the requested cache chunk. It is invoked at most
// once per key at any time (single-flight).
type FetchFunc func(ctx context.Context) ([]byte, error)

type cacheItem struct {
	key   string
	data  []byte
	elem  *list.Element
	ready chan struct{} // closed when fetch finished; nil once data is final
	err   error
}

// ChunkCache is a bounded LRU cache of Telegram byte chunks with built-in
// single-flight deduplication: concurrent readers of the same chunk share one
// upstream fetch. The cache is safe for concurrent use.
type ChunkCache struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	items    map[string]*cacheItem
	lru      *list.List // front = most recently used
}

// NewChunkCache creates a cache holding at most capacityBytes of chunk data.
func NewChunkCache(capacityBytes int64) *ChunkCache {
	if capacityBytes < ChunkSize {
		capacityBytes = ChunkSize
	}
	return &ChunkCache{
		capacity: capacityBytes,
		items:    make(map[string]*cacheItem, capacityBytes/ChunkSize+8),
		lru:      list.New(),
	}
}

// Get returns the chunk for key, fetching it through fetch on a miss. The
// returned slice is owned by the cache and must not be modified.
func (c *ChunkCache) Get(ctx context.Context, key string, fetch FetchFunc) ([]byte, error) {
	c.mu.Lock()
	if it, ok := c.items[key]; ok {
		if it.ready == nil {
			// Completed entry: refresh LRU position.
			c.lru.MoveToFront(it.elem)
			c.mu.Unlock()
			return it.data, nil
		}
		// In-flight fetch: wait for it outside the lock.
		ready := it.ready
		c.mu.Unlock()
		select {
		case <-ready:
			if it.err != nil {
				return nil, it.err
			}
			return it.data, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Cache miss: register an in-flight entry.
	it := &cacheItem{key: key, ready: make(chan struct{})}
	it.elem = c.lru.PushFront(it)
	c.items[key] = it
	c.mu.Unlock()

	data, err := fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// Drop failed entries so the next reader retries.
		c.lru.Remove(it.elem)
		delete(c.items, key)
		it.err = err
		close(it.ready)
		return nil, err
	}
	it.data = data
	it.ready = nil // marks the entry as completed
	it.err = nil
	c.used += int64(len(data))
	c.evictLocked(it)
	close(it.ready)
	return data, nil
}

// evictLocked drops least-recently-used completed entries until the cache is
// within budget. In-flight entries and the freshly inserted keep entry are
// never evicted. The cache holds only capacity/ChunkSize entries (e.g. 16
// for a 64 MiB budget), so the backward scan is cheap.
func (c *ChunkCache) evictLocked(keep *cacheItem) {
	for c.used > c.capacity {
		evicted := false
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			it := e.Value.(*cacheItem)
			if it.ready != nil || it == keep {
				continue
			}
			c.lru.Remove(it.elem)
			delete(c.items, it.key)
			c.used -= int64(len(it.data))
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// Stats reports the current cache usage (for diagnostics).
func (c *ChunkCache) Stats() (usedBytes int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used, len(c.items)
}
