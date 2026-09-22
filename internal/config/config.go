// Package config loads and validates the server configuration from the
// environment (and an optional .env file in the working directory).
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the server.
type Config struct {
	// Port is the HTTP listen port for both /dav and /web.
	Port int

	// Telegram credentials (https://my.telegram.org).
	TelegramAPIID   int
	TelegramAPIHash string

	// TelegramBotToken is the bot token from @BotFather.
	TelegramBotToken string

	// TelegramChannelID is the storage channel: either a numeric ID
	// (e.g. -1001234567890 or 1234567890) or a public @username.
	TelegramChannelID string

	// WebDAV / Web UI credentials (HTTP Basic).
	WebDAVUser     string
	WebDAVPassword string

	// DefaultFolder is the virtual directory where bot uploads are stored.
	DefaultFolder string

	// ChunkCacheSizeMB is the size of the in-memory sliding-window cache.
	ChunkCacheSizeMB int

	// DBPath is the SQLite metadata database path.
	DBPath string

	// SessionPath is where the MTProto session is persisted.
	SessionPath string
}

// Load reads configuration from the environment. Values already present in
// the real environment take precedence over the .env file.
func Load() (*Config, error) {
	loadDotEnv(filepath.Join(".", ".env"))

	cfg := &Config{
		DefaultFolder:    getEnv("DEFAULT_FOLDER", "general"),
		ChunkCacheSizeMB: getEnvInt("CHUNK_CACHE_SIZE_MB", 64),
		DBPath:           getEnv("DB_PATH", "./data/metadata.db"),
		Port:             getEnvInt("PORT", 8080),

		TelegramAPIID:     getEnvInt("TELEGRAM_API_ID", 0),
		TelegramAPIHash:   strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH")),
		TelegramBotToken:  strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChannelID: strings.TrimSpace(os.Getenv("TELEGRAM_CHANNEL_ID")),

		WebDAVUser:     os.Getenv("WEBDAV_USER"),
		WebDAVPassword: os.Getenv("WEBDAV_PASSWORD"),
	}

	cfg.DefaultFolder = strings.Trim(strings.TrimSpace(cfg.DefaultFolder), "/")
	if cfg.DefaultFolder == "" {
		cfg.DefaultFolder = "general"
	}
	if cfg.ChunkCacheSizeMB < 4 {
		cfg.ChunkCacheSizeMB = 4
	}
	cfg.SessionPath = filepath.Join(filepath.Dir(cfg.DBPath), "session.json")

	var missing []string
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("PORT must be a valid TCP port (got %d)", cfg.Port)
	}
	if cfg.TelegramAPIID <= 0 {
		missing = append(missing, "TELEGRAM_API_ID")
	}
	if cfg.TelegramAPIHash == "" {
		missing = append(missing, "TELEGRAM_API_HASH")
	}
	if cfg.TelegramBotToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if cfg.TelegramChannelID == "" {
		missing = append(missing, "TELEGRAM_CHANNEL_ID")
	}
	if cfg.WebDAVUser == "" {
		missing = append(missing, "WEBDAV_USER")
	}
	if cfg.WebDAVPassword == "" {
		missing = append(missing, "WEBDAV_PASSWORD")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s (see .env.example)", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// getEnv returns the value of key or def when unset/empty.
func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// getEnvInt returns key parsed as int or def when unset/invalid.
func getEnvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// loadDotEnv parses a simple KEY=VALUE .env file into the environment.
// Existing environment variables are never overwritten. Lines starting with
// '#' and blank lines are ignored. Values may be wrapped in single or double
// quotes.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
