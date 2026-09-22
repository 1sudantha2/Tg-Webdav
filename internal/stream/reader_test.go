package stream

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"
)

// fakeBackend serves deterministic pseudo-random content and counts fetch
// requests, mimicking upload.getFile (1 MiB aligned slices).
type fakeBackend struct {
	data   []byte
	fetches int
}

func (b *fakeBackend) fetch(offset int64, size int) ([]byte, error) {
	b.fetches++
	end := offset + int64(size)
	if end > int64(len(b.data)) {
		end = int64(len(b.data))
	}
	out := make([]byte, end-offset)
	copy(out, b.data[offset:end])
	return out, nil
}

func newReaderFor(t *testing.T, data []byte, cacheBytes int64) (*Reader, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{data: data}
	cache := NewChunkCache(cacheBytes)
	r := NewReader(context.Background(), cache, "test", int64(len(data)),
		func(_ context.Context, offset int64, size int) ([]byte, error) {
			return b.fetch(offset, size)
		})
	return r, b
}

func TestSequentialRead(t *testing.T) {
	data := make([]byte, 9<<20+123) // ~9 MiB, unaligned tail
	rand.New(rand.NewSource(1)).Read(data)
	r, _ := newReaderFor(t, data, 64<<20)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: got %d bytes", len(got))
	}
}

func TestSeekAndRead(t *testing.T) {
	data := make([]byte, 12<<20)
	rand.New(rand.NewSource(2)).Read(data)
	r, _ := newReaderFor(t, data, 64<<20)

	// Seek from every whence and compare a window around the position.
	checks := []struct {
		off    int64
		whence int
	}{
		{0, io.SeekStart},
		{5<<20 + 7, io.SeekStart},
		{-100, io.SeekEnd},
		{1024, io.SeekCurrent},
	}
	pos := int64(0)
	for _, c := range checks {
		want := c.off
		switch c.whence {
		case io.SeekEnd:
			want = int64(len(data)) + c.off
		case io.SeekCurrent:
			want = pos + c.off
		}
		got, err := r.Seek(c.off, c.whence)
		if err != nil || got != want {
			t.Fatalf("seek(%d,%d) = %d,%v want %d", c.off, c.whence, got, err, want)
		}
		pos = got
		n := int64(4096)
		if rem := int64(len(data)) - pos; rem < n {
			n = rem
		}
		if n <= 0 {
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatalf("read at %d: %v", pos, err)
		}
		if !bytes.Equal(buf, data[pos:pos+n]) {
			t.Fatalf("mismatch after seek to %d", pos)
		}
		pos += n
	}
}

func TestCachePreventsRefetch(t *testing.T) {
	data := make([]byte, 5<<20)
	rand.New(rand.NewSource(3)).Read(data)
	r, b := newReaderFor(t, data, 64<<20)

	buf := make([]byte, 64<<10)
	// Read the same region twice: second time must be served from cache.
	for i := 0; i < 2; i++ {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatal(err)
		}
	}
	firstChunkFetches := b.fetches
	if firstChunkFetches != 1 {
		t.Fatalf("expected exactly 1 chunk fetch for the first 4MiB region, got %d", firstChunkFetches)
	}
}
