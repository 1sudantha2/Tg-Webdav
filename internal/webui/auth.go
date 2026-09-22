package webui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/1sudantha2/Tg-Webdav/internal/config"
)

const (
	sessionCookie  = "tgwd_session"
	sessionLifetime = 7 * 24 * time.Hour
)

// Authenticator validates HTTP Basic credentials and issues HMAC-signed
// session cookies (used by the Web UI so <video>/<audio>/<img> elements can
// stream from /dav without embedding credentials in URLs).
type Authenticator struct {
	user   string
	pass   string
	secret []byte
}

// NewAuthenticator loads (or creates) the signing secret next to the
// database and binds the configured credentials.
func NewAuthenticator(cfg *config.Config) (*Authenticator, error) {
	secretPath := filepath.Join(filepath.Dir(cfg.DBPath), ".session_secret")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
			return nil, fmt.Errorf("store session secret: %w", err)
		}
	}
	return &Authenticator{user: cfg.WebDAVUser, pass: cfg.WebDAVPassword, secret: secret}, nil
}

// CheckCredentials verifies a username/password pair in constant time.
func (a *Authenticator) CheckCredentials(user, pass string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(a.user))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(a.pass))
	return u == 1 && p == 1
}

// Authorized reports whether the request carries valid HTTP Basic
// credentials or a valid session cookie.
func (a *Authenticator) Authorized(r *http.Request) bool {
	if user, pass, ok := r.BasicAuth(); ok && a.CheckCredentials(user, pass) {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && a.validToken(c.Value) {
		return true
	}
	return false
}

// RequireAuth sends a 401 response asking for HTTP Basic credentials.
func (a *Authenticator) RequireAuth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Tg-WebDAV", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// SetSessionCookie issues a session cookie for the Web UI.
func (a *Authenticator) SetSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.newToken(),
		Path:     "/",
		Expires:  time.Now().Add(sessionLifetime),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie.
func (a *Authenticator) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// newToken builds "user.expiry.hmac" where the HMAC covers user and expiry.
func (a *Authenticator) newToken() string {
	expiry := time.Now().Add(sessionLifetime).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(a.user)) + "." + strconv.FormatInt(expiry, 10)
	return payload + "." + a.sign(payload)
}

func (a *Authenticator) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Authenticator) validToken(token string) bool {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	_, expStr, ok := strings.Cut(payload, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	want := a.sign(payload)
	return hmac.Equal([]byte(want), []byte(sig))
}
