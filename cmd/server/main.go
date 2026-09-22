// Command tg-webdav serves a Telegram channel as a full WebDAV share (/dav)
// and a modern Web UI (/web). Files stream straight from Telegram MTProto
// with HTTP Range support; uploads stream straight back to the channel. No
// file content is ever written to disk.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/1sudantha2/Tg-Webdav/internal/config"
	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/stream"
	"github.com/1sudantha2/Tg-Webdav/internal/telegram"
	"github.com/1sudantha2/Tg-Webdav/internal/webdav"
	"github.com/1sudantha2/Tg-Webdav/internal/webui"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open metadata db: %w", err)
	}
	defer store.Close()

	bootCtx := context.Background()
	if _, err := store.MkdirAll(bootCtx, "/"+cfg.DefaultFolder); err != nil {
		return fmt.Errorf("ensure default folder: %w", err)
	}

	cache := stream.NewChunkCache(int64(cfg.ChunkCacheSizeMB) << 20)
	tgSvc := telegram.New(cfg, store, cache)

	auth, err := webui.NewAuthenticator(cfg)
	if err != nil {
		return err
	}

	fs := webdav.NewFS(store, tgSvc)
	davHandler := webdav.NewHandler("/dav", fs, tgSvc, auth)
	ui, err := webui.New(store, tgSvc, auth)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/dav", redirectTo("/dav/"))
	mux.Handle("/dav/", davHandler)
	mux.Handle("/web", redirectTo("/web/"))
	mux.Handle("/web/", ui)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/web/", http.StatusFound)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		// No Read/Write timeouts on purpose: file transfers can last hours
		// and must support slow streaming in both directions.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down http server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		slog.Info("http server listening",
			"addr", addr, "webdav", "/dav", "webui", "/web/",
			"default_folder", "/"+cfg.DefaultFolder, "cache_mb", cfg.ChunkCacheSizeMB)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	// Blocks until ctx is canceled or Telegram fails fatally.
	return tgSvc.Run(ctx)
}

func redirectTo(target string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}
