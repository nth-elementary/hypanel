// Package auth implements the single admin login: bcrypt password, mandatory TOTP,
// server-side sessions and login throttling.
//
// There is deliberately no web setup page, so an exposed panel can't be claimed by
// whoever reaches it first. Credentials are set with `hypanel admin`.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"hypanel/internal/core"
)

const (
	cookieName = "hypanel_session"
	sessionTTL = 12 * time.Hour
	// Throttling is per IP, plus a global ceiling so that many source addresses
	// (or everyone arriving through one proxy) can't brute-force either.
	maxPerIP    = 5
	maxGlobal   = 30
	attemptSpan = 15 * time.Minute
)

type Auth struct {
	app *core.App

	mu       sync.Mutex
	attempts map[string][]time.Time
	lastCode string // the most recently accepted TOTP code, blocks replay
}

func New(app *core.App) (*Auth, error) {
	_, err := app.DB.Exec(`CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, expires INTEGER NOT NULL, ip TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return &Auth{app: app, attempts: map[string][]time.Time{}}, nil
}

func (a *Auth) Name() string { return "auth" }

// SetAdmin stores the credentials, generates a new TOTP secret, and ends all sessions.
func (a *Auth) SetAdmin(user, password string) (otpauthURL string, err error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	for k, v := range map[string]string{"admin_user": user, "admin_hash": string(hash), "admin_totp": secret} {
		if err := a.app.SetSetting(k, v); err != nil {
			return "", err
		}
	}
	if _, err := a.app.DB.Exec(`DELETE FROM sessions`); err != nil {
		return "", err
	}
	q := url.Values{"secret": {secret}, "issuer": {"HyPanel"}}
	return "otpauth://totp/HyPanel:" + url.PathEscape(user) + "?" + q.Encode(), nil
}

func (a *Auth) Routes(r chi.Router) {
	r.Post("/auth/login", a.login)
	r.Post("/auth/logout", a.logout)
	r.Get("/auth/me", func(w http.ResponseWriter, r *http.Request) {
		core.OK(w, map[string]any{"authenticated": a.valid(r), "user": a.app.Setting("admin_user")})
	})
}

// Protected routes: password change and the audit log.
func (a *Auth) ProtectedRoutes(r chi.Router) {
	r.Post("/auth/password", a.changePassword)
	r.Get("/auth/audit", a.audit)
}

// throttled reports whether ip must wait, and prunes expired attempts.
func (a *Auth) throttled(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-attemptSpan)
	total := 0
	for k, list := range a.attempts {
		kept := list[:0]
		for _, t := range list {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(a.attempts, k)
			continue
		}
		a.attempts[k] = kept
		total += len(kept)
	}
	return len(a.attempts[ip]) >= maxPerIP || total >= maxGlobal
}

func (a *Auth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts[ip] = append(a.attempts[ip], time.Now())
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ User, Password, Code string }
	if !core.Read(w, r, &req) {
		return
	}
	ip := core.ClientIP(r)
	if a.throttled(ip) {
		core.Fail(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	user, hash, secret := a.app.Setting("admin_user"), a.app.Setting("admin_hash"), a.app.Setting("admin_totp")
	if hash == "" {
		core.Fail(w, http.StatusServiceUnavailable, "no admin configured: run `hypanel admin` on the server")
		return
	}
	// bcrypt runs before the username check so timing doesn't reveal the username.
	pwOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(req.User)) == 1
	codeOK := validTOTP(secret, req.Code, time.Now())
	a.mu.Lock()
	replay := codeOK && req.Code == a.lastCode
	if pwOK && userOK && codeOK && !replay {
		a.lastCode = req.Code
	}
	a.mu.Unlock()
	if !pwOK || !userOK || !codeOK || replay {
		a.recordFailure(ip)
		a.app.Audit(r, "login.failed", req.User)
		core.Fail(w, http.StatusUnauthorized, "wrong username, password or code")
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(sessionTTL)
	if _, err := a.app.DB.Exec(`INSERT INTO sessions(token_hash,expires,ip) VALUES(?,?,?)`, hashToken(token), expires.Unix(), ip); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	_, _ = a.app.DB.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix())
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	a.app.Audit(r, "login", user)
	core.OK(w, map[string]string{"user": user})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_, _ = a.app.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	core.OK(w, map[string]bool{"ok": true})
}

func (a *Auth) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Current, Next string }
	if !core.Read(w, r, &req) {
		return
	}
	if len(req.Next) < 12 {
		core.Fail(w, http.StatusBadRequest, "use at least 12 characters")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.app.Setting("admin_hash")), []byte(req.Current)) != nil {
		core.Fail(w, http.StatusForbidden, "current password is wrong")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Next), 12)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := a.app.SetSetting("admin_hash", string(hash)); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	// Every other session ends; this one continues.
	var keep string
	if c, err := r.Cookie(cookieName); err == nil {
		keep = hashToken(c.Value)
	}
	_, _ = a.app.DB.Exec(`DELETE FROM sessions WHERE token_hash<>?`, keep)
	a.app.Audit(r, "password.change", "")
	core.OK(w, map[string]bool{"ok": true})
}

func (a *Auth) audit(w http.ResponseWriter, r *http.Request) {
	rows, err := a.app.DB.Query(`SELECT ts,ip,action,detail FROM audit ORDER BY ts DESC LIMIT 500`)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts int64
		var ip, action, detail string
		if rows.Scan(&ts, &ip, &action, &detail) == nil {
			out = append(out, map[string]any{"ts": ts, "ip": ip, "action": action, "detail": detail})
		}
	}
	core.OK(w, out)
}

func (a *Auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	var expires int64
	err = a.app.DB.QueryRow(`SELECT expires FROM sessions WHERE token_hash=?`, hashToken(c.Value)).Scan(&expires)
	return err == nil && expires > time.Now().Unix()
}

// Require rejects unauthenticated requests, and cross-site writes and WebSockets.
// SameSite=Strict already stops most of these; the Origin check is a second layer.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			core.Fail(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if !SameOrigin(r) && (r.Method != http.MethodGet || r.Header.Get("Upgrade") != "") {
			core.Fail(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SameOrigin reports whether the Origin header, if any, matches the request's host.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// validTOTP implements RFC 6238 with a one-step tolerance for clock drift.
func validTOTP(secret, code string, now time.Time) bool {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(code) != 6 {
		return false
	}
	step := now.Unix() / 30
	ok := false
	for _, s := range []int64{step - 1, step, step + 1} {
		if subtle.ConstantTimeCompare([]byte(totpAt(key, s)), []byte(code)) == 1 {
			ok = true
		}
	}
	return ok
}

func totpAt(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}
