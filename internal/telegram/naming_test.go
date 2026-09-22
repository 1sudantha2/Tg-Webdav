package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestDecideName(t *testing.T) {
	at := time.Date(2026, 9, 22, 15, 4, 5, 0, time.UTC)

	cases := []struct {
		desc     string
		caption  string
		original string
		mime     string
		want     string
	}{
		{"caption with extension", "My Movie.mkv", "upload.bin", "video/x-matroska", "My Movie.mkv"},
		{"caption without extension gets mime ext", "Holiday video", "IMG_001.mp4", "video/mp4", "Holiday video.mp4"},
		{"caption wins over original", "report.pdf", "scan.jpg", "application/pdf", "report.pdf"},
		{"no caption uses original name", "", "song.mp3", "audio/mpeg", "song.mp3"},
		{"original without extension gets ext", "", "voice", "audio/ogg", "voice.ogg"},
		{"fallback timestamped name", "", "", "video/mp4", "file_20260922_150405.mp4"},
		{"fallback unknown mime", "", "", "application/x-weird", "file_20260922_150405.bin"},
		{"dangerous caption sanitized", "../etc/passwd", "", "text/plain", ".._etc_passwd.txt"},
	}
	for _, c := range cases {
		if got := decideName(c.caption, c.original, c.mime, at); got != c.want {
			t.Errorf("%s: decideName(%q,%q,%q) = %q, want %q", c.desc, c.caption, c.original, c.mime, got, c.want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("  a/b\\c:d*e?f\"g<h>i|j  "); got != "a_b_c_d_e_f_g_h_i_j" {
		t.Errorf("sanitize: %q", got)
	}
	if got := sanitizeName(""); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := sanitizeName("multi\nline\rcaption"); !strings.Contains(got, "multi") || strings.ContainsAny(got, "\n\r") {
		t.Errorf("multiline: %q", got)
	}
	long := strings.Repeat("x", 300) + ".mp4"
	if got := sanitizeName(long); len(got) > maxNameLen || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("truncation: len=%d %q", len(got), got)
	}
}

func TestGuessMIME(t *testing.T) {
	cases := map[string]string{
		"a.mp4":    "video/mp4",
		"a.MKV":    "video/x-matroska",
		"a.mp3":    "audio/mpeg",
		"a.jpg":    "image/jpeg",
		"noext":    "application/octet-stream",
		"a.tar.gz": "application/gzip",
	}
	for name, want := range cases {
		if got := GuessMIME(name); got != want {
			t.Errorf("GuessMIME(%q) = %q, want %q", name, got, want)
		}
	}
}
