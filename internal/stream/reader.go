package stream

import (
	"context"
	"fmt"
	"io"
)

// ChunkFetch fetches up to size bytes of the underlying file starting at the
// (chunk-aligned) absolute offset. Implementations talk to Telegram
// (upload.getFile) and may internally split the range into 1 MiB requests.
type ChunkFetch func(ctx context.Context, offset int64, size int) ([]byte, error)

// Reader adapts Telegram MTProto chunks to an io.ReadSeeker so the standard
// library's http.ServeContent can answer Range requests (HTTP 206) directly,
// giving instant video seeking without buffering whole files.
//
// A Reader is bound to one file; use a separate Reader per HTTP request. All
// reads go through the shared ChunkCache, so recently viewed segments are
// served from memory.
type Reader struct {
	key   string // stable cache key prefix (Telegram document id)
	size  int64
	off   int64
	cache *ChunkCache
	fetch ChunkFetch
	ctx   context.Context
}

// NewReader creates a ReadSeeker over a Telegram file of the given size.
// ctx bounds all fetches performed through Read.
func NewReader(ctx context.Context, cache *ChunkCache, key string, size int64, fetch ChunkFetch) *Reader {
	return &Reader{key: key, size: size, cache: cache, fetch: fetch, ctx: ctx}
}

// Size returns the total size of the underlying file.
func (r *Reader) Size() int64 { return r.size }

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.off >= r.size {
		return 0, io.EOF
	}

	chunkIdx := r.off / ChunkSize
	inChunk := r.off % ChunkSize

	key := fmt.Sprintf("%s:%d", r.key, chunkIdx)
	want := ChunkSize
	if rem := r.size - chunkIdx*ChunkSize; rem < int64(want) {
		want = int(rem)
	}
	data, err := r.cache.Get(r.ctx, key, func(ctx context.Context) ([]byte, error) {
		return r.fetch(ctx, chunkIdx*ChunkSize, want)
	})
	if err != nil {
		return 0, err
	}
	if inChunk >= int64(len(data)) {
		// Short tail chunk (should not happen for well-formed files).
		return 0, io.ErrUnexpectedEOF
	}

	n := copy(p, data[inChunk:])
	r.off += int64(n)
	if r.off >= r.size && n < len(p) {
		// Signal EOF together with the last bytes, like os.File does.
		return n, io.EOF
	}
	return n, nil
}

// Seek implements io.Seeker.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("stream: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("stream: negative position %d", abs)
	}
	r.off = abs
	return abs, nil
}
