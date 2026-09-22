// Package webdav exposes the Telegram-backed virtual file system as a full
// WebDAV endpoint (RFC 4918) compatible with Windows Explorer, macOS Finder,
// VLC, Infuse and Cyberduck.
//
// The heavy protocol work (PROPFIND XML, locks, MOVE, ...) is delegated to
// golang.org/x/net/webdav on top of a custom webdav.FileSystem that is backed
// by the SQLite index; file contents stream straight from Telegram through
// the io.ReadSeeker adapter, so HTTP Range requests (206) give instant
// seeking with zero disk caching.
package webdav

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/telegram"
)

// FS implements golang.org/x/net/webdav.FileSystem on top of the metadata
// index and the Telegram streaming service.
type FS struct {
	store *db.DB
	tg    *telegram.Service
}

// NewFS creates the WebDAV file system.
func NewFS(store *db.DB, tg *telegram.Service) *FS {
	return &FS{store: store, tg: tg}
}

func mapErr(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return os.ErrNotExist
	}
	return err
}

// cleanName normalizes the request path to an absolute virtual path.
func cleanName(name string) string {
	return path.Clean("/" + strings.TrimPrefix(path.Clean("/"+name), "/"))
}

// OpenFile implements webdav.FileSystem. Writes are not handled here: PUT is
// intercepted at the HTTP layer so uploads stream straight to Telegram with
// a known content length.
func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (xwebdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, os.ErrPermission
	}
	node, err := f.store.Lookup(ctx, cleanName(name))
	if err != nil {
		return nil, mapErr(err)
	}
	return &davFile{ctx: ctx, fs: f, node: node}, nil
}

// Stat implements webdav.FileSystem.
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	node, err := f.store.Lookup(ctx, cleanName(name))
	if err != nil {
		return nil, mapErr(err)
	}
	return &nodeInfo{node: node}, nil
}

// Mkdir implements webdav.FileSystem. The parent directory must exist.
func (f *FS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	name = cleanName(name)
	parent, err := f.store.Lookup(ctx, path.Dir(name))
	if err != nil {
		return mapErr(err)
	}
	if !parent.IsDir {
		return os.ErrExist
	}
	if _, err := f.store.Mkdir(ctx, parent.ID, path.Base(name)); err != nil {
		if isUniqueViolation(err) {
			return os.ErrExist
		}
		return err
	}
	return nil
}

// RemoveAll implements webdav.FileSystem. Deleting a file also deletes the
// backing Telegram message (unless another copy references it).
func (f *FS) RemoveAll(ctx context.Context, name string) error {
	node, err := f.store.Lookup(ctx, cleanName(name))
	if err != nil {
		return mapErr(err)
	}
	if node.ID == 0 {
		return os.ErrPermission
	}
	deleted, err := f.store.DeleteRecursive(ctx, node.ID)
	if err != nil {
		return err
	}
	if len(deleted) > 0 {
		go f.tg.MaybeDeleteChannelMessages(context.WithoutCancel(ctx), deleted)
	}
	return nil
}

// Rename implements webdav.FileSystem (used by MOVE).
func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	src, err := f.store.Lookup(ctx, cleanName(oldName))
	if err != nil {
		return mapErr(err)
	}
	if src.ID == 0 {
		return os.ErrPermission
	}
	dstParent, err := f.store.Lookup(ctx, path.Dir(cleanName(newName)))
	if err != nil {
		return mapErr(err)
	}
	if !dstParent.IsDir {
		return os.ErrExist
	}
	if err := f.store.Rename(ctx, src.ID, dstParent.ID, path.Base(cleanName(newName))); err != nil {
		if isUniqueViolation(err) {
			return os.ErrExist
		}
		return err
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint error.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------------------------------------------------------------------------
// webdav.File
// ---------------------------------------------------------------------------

// davFile is one open WebDAV resource. Directories serve Readdir; files
// stream from Telegram through the shared chunk cache.
type davFile struct {
	ctx     context.Context
	fs      *FS
	node    *db.Node
	reader  io.ReadSeeker
	entries []os.FileInfo
	readdir int
}

var _ xwebdav.File = (*davFile)(nil)

func (f *davFile) Close() error { return nil }

func (f *davFile) Stat() (os.FileInfo, error) { return &nodeInfo{node: f.node}, nil }

func (f *davFile) Read(p []byte) (int, error) {
	if f.node.IsDir {
		return 0, &os.PathError{Op: "read", Path: f.node.Name, Err: os.ErrInvalid}
	}
	if f.reader == nil {
		r, err := f.fs.tg.NewFileReader(f.ctx, f.node)
		if err != nil {
			return 0, err
		}
		f.reader = r
	}
	return f.reader.Read(p)
}

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	if f.node.IsDir {
		return 0, &os.PathError{Op: "seek", Path: f.node.Name, Err: os.ErrInvalid}
	}
	if f.reader == nil {
		r, err := f.fs.tg.NewFileReader(f.ctx, f.node)
		if err != nil {
			return 0, err
		}
		f.reader = r
	}
	return f.reader.Seek(offset, whence)
}

// Write is only reached if a client issues a write through the x/net code
// path; real uploads go through the streaming PUT handler.
func (f *davFile) Write(p []byte) (int, error) {
	return 0, os.ErrPermission
}

func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.node.IsDir {
		return nil, &os.PathError{Op: "readdir", Path: f.node.Name, Err: errors.New("not a directory")}
	}
	if f.entries == nil {
		kids, err := f.fs.store.Children(f.ctx, f.node.ID)
		if err != nil {
			return nil, err
		}
		f.entries = make([]os.FileInfo, 0, len(kids))
		for _, k := range kids {
			f.entries = append(f.entries, &nodeInfo{node: k})
		}
		sort.Slice(f.entries, func(i, j int) bool {
			di, dj := f.entries[i].IsDir(), f.entries[j].IsDir()
			if di != dj {
				return di
			}
			return f.entries[i].Name() < f.entries[j].Name()
		})
	}
	if count <= 0 { // return everything remaining
		out := f.entries[f.readdir:]
		f.readdir = len(f.entries)
		if len(out) == 0 && count > 0 {
			return nil, io.EOF
		}
		return out, nil
	}
	if f.readdir >= len(f.entries) {
		return nil, io.EOF
	}
	end := f.readdir + count
	if end > len(f.entries) {
		end = len(f.entries)
	}
	out := f.entries[f.readdir:end]
	f.readdir = end
	return out, nil
}

// ---------------------------------------------------------------------------
// os.FileInfo
// ---------------------------------------------------------------------------

type nodeInfo struct{ node *db.Node }

func (i *nodeInfo) Name() string { return i.node.Name }

func (i *nodeInfo) Size() int64 {
	if i.node.IsDir {
		return 0
	}
	return i.node.Size
}

func (i *nodeInfo) Mode() fs.FileMode {
	if i.node.IsDir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (i *nodeInfo) ModTime() time.Time { return i.node.MTime }
func (i *nodeInfo) IsDir() bool        { return i.node.IsDir }
func (i *nodeInfo) Sys() any           { return nil }
