package telegram

import (
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
)

// maxNameLen keeps names well below Telegram / file system limits.
const maxNameLen = 180

// decideName implements the ingestion filename policy:
//
//  1. the message caption (if it looks like a filename),
//  2. caption without extension gets the MIME-derived extension appended,
//  3. otherwise the original uploaded file name,
//  4. otherwise a MIME-aware timestamped name: file_20060102_150405.<ext>.
func decideName(caption, originalName, mimeType string, at time.Time) string {
	if name := sanitizeName(caption); name != "" {
		if path.Ext(name) == "" {
			if ext := extForMIME(mimeType); ext != "" {
				name += ext
			}
		}
		return name
	}
	if name := sanitizeName(originalName); name != "" {
		if path.Ext(name) == "" {
			if ext := extForMIME(mimeType); ext != "" {
				name += ext
			}
		}
		return name
	}
	ext := extForMIME(mimeType)
	if ext == "" {
		ext = ".bin"
	}
	return "file_" + at.UTC().Format("20060102_150405") + ext
}

// sanitizeName strips path separators, control characters and other
// characters that are unsafe on common file systems / WebDAV clients.
// It returns "" when nothing usable remains.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00':
			return '_'
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return ""
	}
	if len(s) > maxNameLen {
		ext := path.Ext(s)
		base := strings.TrimSuffix(s, ext)
		keep := maxNameLen - len(ext)
		if keep < 8 {
			ext, keep = "", maxNameLen
		}
		s = base[:keep] + ext
	}
	return s
}

// extForMIME maps a MIME type to a canonical file extension (with leading
// dot), preferring extensions that players and OSes handle well.
func extForMIME(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "" {
		return ""
	}
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	// Hand-picked common types where mime.ExtensionsByType returns odd or
	// multiple options.
	switch mimeType {
	case "video/mp4":
		return ".mp4"
	case "video/x-matroska":
		return ".mkv"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "audio/opus":
		return ".opus"
	case "audio/flac":
		return ".flac"
	case "audio/mp4":
		return ".m4a"
	case "audio/x-wav", "audio/wav":
		return ".wav"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "application/x-7z-compressed":
		return ".7z"
	case "application/x-rar-compressed":
		return ".rar"
	case "application/x-tar":
		return ".tar"
	case "application/gzip":
		return ".gz"
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "application/octet-stream":
		return ""
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// GuessMIME returns a MIME type for a file name, defaulting to
// application/octet-stream.
func GuessMIME(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == ".mkv" {
		return "video/x-matroska"
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}
