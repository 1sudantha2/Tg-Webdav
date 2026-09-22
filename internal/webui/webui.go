// Package webui serves the embedded Web UI (vanilla HTML/CSS/JS) together
// with a small JSON API for browsing and managing the virtual file system.
// Playback itself streams straight from the /dav endpoint using the session
// cookie, so HTML5 seeking works with full Range support.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/telegram"
)

//go:embed static
var staticFS embed.FS

// UI is the /web endpoint handler.
type UI struct {
	store *db.DB
	tg    *telegram.Service
	auth  *Authenticator

	static http.Handler
}

// New creates the Web UI handler.
func New(store *db.DB, tg *telegram.Service, auth *Authenticator) (*UI, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return &UI{
		store:  store,
		tg:     tg,
		auth:   auth,
		static: http.FileServer(http.FS(sub)),
	}, nil
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/web")
	rel = strings.TrimPrefix(rel, "/")

	if strings.HasPrefix(rel, "api/") {
		if rel != "api/login" && !u.auth.Authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
			return
		}
		u.api(w, r, rel)
		return
	}

	// Static assets live below the /web/ prefix; the bare path serves the
	// single-page shell. (Serving "/" instead of "/index.html" avoids
	// http.FileServer's canonicalizing redirect for index pages.)
	r2 := r.Clone(r.Context())
	if rel == "" {
		r2.URL.Path = "/"
	} else {
		r2.URL.Path = "/" + rel
	}
	r2.URL.RawPath = ""
	u.static.ServeHTTP(w, r2)
}

// ---------------------------------------------------------------------------
// JSON API
// ---------------------------------------------------------------------------

type entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

func (u *UI) api(w http.ResponseWriter, r *http.Request, rel string) {
	switch {
	case rel == "api/login" && r.Method == http.MethodPost:
		u.handleLogin(w, r)
	case rel == "api/logout" && r.Method == http.MethodPost:
		u.auth.ClearSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case rel == "api/session":
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": u.auth.Authorized(r)})
	case rel == "api/ls":
		u.handleLS(w, r)
	case rel == "api/mkdir" && r.Method == http.MethodPost:
		u.handleMkdir(w, r)
	case rel == "api/rename" && r.Method == http.MethodPost:
		u.handleRename(w, r)
	case rel == "api/delete" && r.Method == http.MethodPost:
		u.handleDelete(w, r)
	case rel == "api/upload" && r.Method == http.MethodPut:
		u.handleUpload(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (u *UI) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if !u.auth.CheckCredentials(req.Username, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
		return
	}
	u.auth.SetSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleLS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir := path.Clean("/" + r.URL.Query().Get("path"))
	node, err := u.store.Lookup(ctx, dir)
	if err != nil {
		apiErr(w, err)
		return
	}
	if !node.IsDir {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not a directory"})
		return
	}
	kids, err := u.store.Children(ctx, node.ID)
	if err != nil {
		apiErr(w, err)
		return
	}
	entries := make([]entry, 0, len(kids))
	for _, k := range kids {
		entries = append(entries, entry{Name: k.Name, IsDir: k.IsDir, Size: k.Size, MTime: k.MTime.Unix()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries})
}

func (u *UI) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if _, err := u.store.MkdirAll(r.Context(), req.Path); err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.From == "" || req.To == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "from and to required"})
		return
	}
	ctx := r.Context()
	src, err := u.store.Lookup(ctx, req.From)
	if err != nil {
		apiErr(w, err)
		return
	}
	dstParent, err := u.store.Lookup(ctx, path.Dir(path.Clean("/"+req.To)))
	if err != nil {
		apiErr(w, err)
		return
	}
	if !dstParent.IsDir || src.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid rename"})
		return
	}
	if err := u.store.Rename(ctx, src.ID, dstParent.ID, path.Base(path.Clean("/"+req.To))); err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	ctx := r.Context()
	node, err := u.store.Lookup(ctx, req.Path)
	if err != nil {
		apiErr(w, err)
		return
	}
	if node.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot delete root"})
		return
	}
	deleted, err := u.store.DeleteRecursive(ctx, node.ID)
	if err != nil {
		apiErr(w, err)
		return
	}
	if len(deleted) > 0 {
		go u.tg.MaybeDeleteChannelMessages(context.WithoutCancel(ctx), deleted)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUpload streams a browser upload (raw PUT body) into the storage
// channel. Content-Length is known, so Telegram gets the exact file size.
func (u *UI) handleUpload(w http.ResponseWriter, r *http.Request) {
	dir := path.Clean("/" + r.URL.Query().Get("path"))
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	mime := telegram.GuessMIME(name)
	_, full, err := u.tg.UploadStreamReplace(r.Context(), dir, name, r.Body, r.ContentLength, mime, nil)
	if err != nil {
		slog.Warn("web upload failed", "dir", dir, "name", name, "err", err)
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "path": full})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}
