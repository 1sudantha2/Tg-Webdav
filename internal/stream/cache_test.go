package stream

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCacheHitAndMiss(t *testing.T) {
	c := NewChunkCache(3 * ChunkSize)
	ctx := context.Background()

	var calls int32
	fetch := func(ctx context.Context) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("hello"), nil
	}

	for i := 0; i < 3; i++ {
		data, err := c.Get(ctx, "k", fetch)
		if err != nil || string(data) != "hello" {
			t.Fatalf("get: %v %q", err, data)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 fetch, got %d", calls)
	}
}

func TestCacheSingleFlight(t *testing.T) {
	c := NewChunkCache(3 * ChunkSize)
	ctx := context.Background()

	start := make(chan struct{})
	var calls int32
	fetch := func(ctx context.Context) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		<-start
		return []byte("data"), nil
	}

	// Start the first fetch and wait until it is registered as in-flight,
	// so every subsequent Get must join it instead of fetching again.
	first := make(chan []byte, 1)
	go func() {
		data, _ := c.Get(ctx, "same", fetch)
		first <- data
	}()
	deadline := 0
	for atomic.LoadInt32(&calls) == 0 && deadline < 500 {
		runtime.Gosched()
		deadline++
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.Get(ctx, "same", fetch)
			if err != nil || string(data) != "data" {
				t.Errorf("get: %v %q", err, data)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := <-first; string(got) != "data" {
		t.Fatalf("first fetch: %q", got)
	}
	if calls != 1 {
		t.Fatalf("single-flight violated: %d fetches", calls)
	}
}

func TestCacheEviction(t *testing.T) {
	c := NewChunkCache(2 * ChunkSize) // room for exactly 2 full chunks
	ctx := context.Background()
	chunk := make([]byte, ChunkSize)

	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, err := c.Get(ctx, key, func(context.Context) ([]byte, error) { return chunk, nil }); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	used, entries := c.Stats()
	if entries > 2 || used > 2*ChunkSize {
		t.Fatalf("eviction failed: %d entries, %d bytes", entries, used)
	}
}
