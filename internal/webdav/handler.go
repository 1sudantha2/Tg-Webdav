package webdav

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	xwebdav "golang.org/x/net/webdav"

	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/telegram"
	"github.com/1sudantha2/Tg-Webdav/internal/webui"
)

// Handler serves the /dav endpoint: HTTP Basic authentication plus full
// WebDAV. PUT is intercepted so uploads stream directly to Telegram with the
// request's Content-Length (no buffering, no 10 MB small-file limit), and
// COPY is intercepted so copies are index-only operations instead of
// re-downloading and re-uploading file contents.
type Handler struct {
	prefix string
	fs     *FS
	tg     *telegram.Service
	auth   *webui.Authenticator
	dav    *xwebdav.Handler
}

// NewHandler wires the WebDAV endpoint. prefix is the URL prefix the handler
// is mounted at (e.g. "/dav").
func NewHandler(prefix string, fs *FS, tg *telegram.Service, auth *webui.Authenticator) *Handler {
	return &Handler{
		prefix: strings.TrimSuffix(prefix, "/"),
		fs:     fs,
		tg:     tg,
		auth:   auth,
		dav: &xwebdav.Handler{
			Prefix:     prefix,
			FileSystem: fs,
			LockSystem: xwebdav.NewMemLS(),
			Logger: func(r *http.Request, err error) {
				slog.Debug("webdav", "method", r.Method, "path", r.URL.Path, "err", err)
			},
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.auth.Authorized(r) {
		h.auth.RequireAuth(w)
		return
	}
	switch r.Method {
	case http.MethodPut:
		h.handlePut(w, r)
	case "COPY":
		h.handleCopy(w, r)
	default:
		h.dav.ServeHTTP(w, r)
	}
}

// relPath strips the mount prefix and normalizes the request path.
func (h *Handler) relPath(p string) string {
	if h.prefix != "" {
		p = strings.TrimPrefix(p, h.prefix)
	}
	return path.Clean("/" + p)
}

// handlePut streams the request body straight into the storage channel.
func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	rel := h.relPath(r.URL.Path)
	if rel == "/" || rel == "" {
		http.Error(w, "cannot upload to root", http.StatusConflict)
		return
	}
	ctx := r.Context()

	parent, err := h.fs.store.Lookup(ctx, path.Dir(rel))
	if err != nil {
		http.Error(w, "parent directory does not exist", http.StatusConflict)
		return
	}
	if !parent.IsDir {
		http.Error(w, "parent is not a directory", http.StatusConflict)
		return
	}

	name := path.Base(rel)
	existing, childErr := h.fs.store.Child(ctx, parent.ID, name)
	if childErr == nil && existing.IsDir {
		http.Error(w, "target exists and is a collection", http.StatusConflict)
		return
	}
	existed := childErr == nil

	if r.ContentLength == 0 {
		http.Error(w, "empty files cannot be stored in Telegram", http.StatusUnsupportedMediaType)
		return
	}

	mime := telegram.GuessMIME(name)
	if _, _, err := h.tg.UploadStreamReplace(ctx, path.Dir(rel), name, r.Body, r.ContentLength, mime, nil); err != nil {
		slog.Warn("PUT failed", "path", rel, "err", err)
		if errors.Is(err, context.Canceled) {
			// Client aborted: nothing sensible to write at this point.
			return
		}
		http.Error(w, "upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleCopy performs COPY as a pure index operation: the Telegram document
// is shared between both entries, so no bytes move.
func (h *Handler) handleCopy(w http.ResponseWriter, r *http.Request) {
	dstHdr := r.Header.Get("Destination")
	if dstHdr == "" {
		http.Error(w, "missing Destination header", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(dstHdr)
	if err != nil {
		http.Error(w, "invalid Destination header", http.StatusBadRequest)
		return
	}
	if u.Host != "" && u.Host != r.Host {
		http.Error(w, "cross-server copies are not supported", http.StatusBadGateway)
		return
	}
	overwrite := r.Header.Get("Overwrite") != "F"

	ctx := r.Context()
	src := h.relPath(r.URL.Path)
	dst := h.relPath(u.Path)

	srcNode, err := h.fs.store.Lookup(ctx, src)
	if err != nil {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	if dst == src {
		http.Error(w, "source and destination are identical", http.StatusForbidden)
		return
	}

	dstNode, dstErr := h.fs.store.Lookup(ctx, dst)
	if dstErr == nil {
		if dstNode.IsDir {
			// Copy into an existing collection.
			dst = path.Join(dst, srcNode.Name)
			dstNode, dstErr = h.fs.store.Lookup(ctx, dst)
		}
	}
	if dstErr == nil { // destination exists
		if !overwrite {
			http.Error(w, "destination exists and Overwrite is F", http.StatusPreconditionFailed)
			return
		}
		if dstNode.ID == 0 {
			http.Error(w, "cannot overwrite root", http.StatusForbidden)
			return
		}
		deleted, err := h.fs.store.DeleteRecursive(ctx, dstNode.ID)
		if err != nil {
			http.Error(w, "could not remove destination", http.StatusInternalServerError)
			return
		}
		if len(deleted) > 0 {
			go h.tg.MaybeDeleteChannelMessages(context.WithoutCancel(ctx), deleted)
		}
	} else if !errors.Is(dstErr, db.ErrNotFound) {
		http.Error(w, "destination lookup failed", http.StatusInternalServerError)
		return
	}

	dstParent, err := h.fs.store.Lookup(ctx, path.Dir(dst))
	if err != nil || !dstParent.IsDir {
		http.Error(w, "destination parent does not exist", http.StatusConflict)
		return
	}
	if err := h.fs.store.CopyRecursive(ctx, srcNode.ID, dstParent.ID, path.Base(dst)); err != nil {
		slog.Warn("COPY failed", "src", src, "dst", dst, "err", err)
		http.Error(w, "copy failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if overwrite {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}
