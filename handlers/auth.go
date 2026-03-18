package handlers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"image/png"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// ─── Session Store ────────────────────────────────────────────────────────────

type sessionEntry struct {
	CreatedAt    time.Time
	NeedsTOTP    bool // true = password ok, waiting for TOTP code
}

var (
	sessionStore   = map[string]sessionEntry{}
	sessionStoreMu sync.RWMutex
	sessionTTL     = 24 * time.Hour
)

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func createSession(w http.ResponseWriter, needsTOTP bool) {
	token, err := newSessionToken()
	if err != nil {
		log.Printf("session: failed to create token: %v", err)
		return
	}
	sessionStoreMu.Lock()
	sessionStore[token] = sessionEntry{CreatedAt: time.Now(), NeedsTOTP: needsTOTP}
	sessionStoreMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "osm_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func getSession(r *http.Request) (sessionEntry, bool) {
	c, err := r.Cookie("osm_session")
	if err != nil {
		return sessionEntry{}, false
	}
	sessionStoreMu.RLock()
	entry, ok := sessionStore[c.Value]
	sessionStoreMu.RUnlock()
	if !ok {
		return sessionEntry{}, false
	}
	if time.Since(entry.CreatedAt) > sessionTTL {
		sessionStoreMu.Lock()
		delete(sessionStore, c.Value)
		sessionStoreMu.Unlock()
		return sessionEntry{}, false
	}
	return entry, true
}

func promoteSession(r *http.Request) {
	c, err := r.Cookie("osm_session")
	if err != nil {
		return
	}
	sessionStoreMu.Lock()
	if entry, ok := sessionStore[c.Value]; ok {
		entry.NeedsTOTP = false
		sessionStore[c.Value] = entry
	}
	sessionStoreMu.Unlock()
}

func destroySession(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("osm_session")
	if err == nil {
		sessionStoreMu.Lock()
		delete(sessionStore, c.Value)
		sessionStoreMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "osm_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

// ─── Auth Config ──────────────────────────────────────────────────────────────

type authCfg struct {
	Username     string
	PasswordHash string
	TOTPSecret   string
}

var auth authCfg

func InitAuth() {
	auth.Username = os.Getenv("AUTH_USERNAME")
	if auth.Username == "" {
		auth.Username = "admin"
	}
	auth.PasswordHash = os.Getenv("AUTH_PASSWORD_HASH")
	auth.TOTPSecret = os.Getenv("AUTH_TOTP_SECRET")

	if auth.PasswordHash == "" {
		log.Println("auth: AUTH_PASSWORD_HASH not set — run `make gen-password` and add it to .env")
	}
}

func hasTOTP() bool     { return auth.TOTPSecret != "" }
func hasPassword() bool { return auth.PasswordHash != "" }

// ─── Middleware ───────────────────────────────────────────────────────────────

// AuthMiddleware protects all routes except /login, /static, /favicon.
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Public paths
		if path == "/login" || strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		sess, ok := getSession(r)
		if !ok {
			if isHTMXRequest(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Session exists but TOTP not yet verified
		if sess.NeedsTOTP && path != "/login/totp" {
			if isHTMXRequest(r) {
				w.Header().Set("HX-Redirect", "/login/totp")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
			return
		}

		// Authenticated — attach to context and proceed
		ctx := context.WithValue(r.Context(), ctxKeyAuth{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxKeyAuth struct{}

func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// ─── Auth Handlers ────────────────────────────────────────────────────────────

func LoginPage(w http.ResponseWriter, r *http.Request) {
	// Already logged in
	if sess, ok := getSession(r); ok && !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	renderAuthTemplate(w, "login.html", map[string]interface{}{
		"NoPasswordSet": !hasPassword(),
	})
}

func LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderAuthError(w, "login.html", "Invalid request", nil)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if !hasPassword() {
		renderAuthError(w, "login.html", "No password set — run `make gen-password` first", nil)
		return
	}

	if username != auth.Username {
		renderAuthError(w, "login.html", "Invalid username or password", nil)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(auth.PasswordHash), []byte(password)); err != nil {
		renderAuthError(w, "login.html", "Invalid username or password", nil)
		return
	}

	// Password correct — check TOTP
	if hasTOTP() {
		createSession(w, true) // partial session, TOTP pending
		http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
		return
	}

	// No TOTP configured — full session, redirect to setup
	createSession(w, false)
	http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
}

func TOTPVerifyPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := getSession(r)
	if !ok || !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	renderAuthTemplate(w, "totp_verify.html", nil)
}

func TOTPVerifySubmit(w http.ResponseWriter, r *http.Request) {
	sess, ok := getSession(r)
	if !ok || !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderAuthError(w, "totp_verify.html", "Invalid request", nil)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !totp.Validate(code, auth.TOTPSecret) {
		renderAuthError(w, "totp_verify.html", "Invalid code — try again", nil)
		return
	}

	promoteSession(r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func TOTPSetupPage(w http.ResponseWriter, r *http.Request) {
	// Already has TOTP — skip setup
	if hasTOTP() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	secret, err := generateAndSaveTOTPSecret()
	if err != nil {
		http.Error(w, "Failed to generate TOTP secret: "+err.Error(), http.StatusInternalServerError)
		return
	}

	qrDataURL, err := totpQRDataURL(secret)
	if err != nil {
		http.Error(w, "Failed to generate QR code: "+err.Error(), http.StatusInternalServerError)
		return
	}

	renderAuthTemplate(w, "totp_setup.html", map[string]interface{}{
		"Secret":    secret,
		"QRDataURL": template.URL(qrDataURL),
		"Issuer":    "ObjectStore Manager",
		"Account":   auth.Username,
	})
}

func TOTPSetupVerify(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	secret := r.FormValue("secret")
	code := strings.TrimSpace(r.FormValue("code"))

	if !totp.Validate(code, secret) {
		qrDataURL, _ := totpQRDataURL(secret)
		renderAuthError(w, "totp_setup.html", "Code mismatch — scan again and retry", map[string]interface{}{
			"Secret":    secret,
			"QRDataURL": template.URL(qrDataURL),
			"Issuer":    "ObjectStore Manager",
			"Account":   auth.Username,
		})
		return
	}

	// Persist to .env
	if err := writeEnvKey(".env", "AUTH_TOTP_SECRET", secret); err != nil {
		log.Printf("totp setup: failed to write .env: %v", err)
	}
	auth.TOTPSecret = secret

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func Logout(w http.ResponseWriter, r *http.Request) {
	destroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ─── TOTP helpers ─────────────────────────────────────────────────────────────

func generateAndSaveTOTPSecret() (string, error) {
	// Reuse if already generated but not yet verified (stored in env mid-setup)
	if existing := os.Getenv("AUTH_TOTP_SECRET_PENDING"); existing != "" {
		return existing, nil
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "ObjectStore Manager",
		AccountName: auth.Username,
	})
	if err != nil {
		return "", err
	}
	secret := key.Secret()
	// Stash temporarily in env (not saved to file yet — saved only after verification)
	os.Setenv("AUTH_TOTP_SECRET_PENDING", secret)
	return secret, nil
}

func totpQRDataURL(secret string) (string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "ObjectStore Manager",
		AccountName: auth.Username,
		Secret:      []byte(decodeBase32(secret)),
	})
	if err != nil {
		return "", err
	}
	img, err := key.Image(256, 256)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func decodeBase32(s string) string {
	// pquerna/otp uses base32; we just need the raw secret bytes
	b, err := base32Decode(s)
	if err != nil {
		return s
	}
	return string(b)
}

func base32Decode(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimRight(s, "="))
	// pad to multiple of 8
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	var bits uint
	var val uint64
	var out []byte
	for _, c := range s {
		if c == '=' {
			break
		}
		idx := strings.IndexRune(alphabet, c)
		if idx < 0 {
			return nil, fmt.Errorf("invalid base32 char: %c", c)
		}
		val = (val << 5) | uint64(idx)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(val>>bits))
			val &= (1 << bits) - 1
		}
	}
	return out, nil
}

// ─── .env writer ─────────────────────────────────────────────────────────────

// writeEnvKey updates or appends a key=value line in the given env file.
func writeEnvKey(filename, key, value string) error {
	f, err := os.ReadFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := strings.Split(string(f), "\n")
	found := false
	for i, line := range lines {
		k, _, _ := strings.Cut(line, "=")
		if strings.TrimSpace(k) == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}

	return os.WriteFile(filename, []byte(strings.Join(lines, "\n")), 0600)
}

// ─── Template rendering ───────────────────────────────────────────────────────

func renderAuthTemplate(w http.ResponseWriter, name string, data interface{}) {
	t, err := template.New(name).Funcs(template.FuncMap{}).ParseFiles("templates/auth/" + name)
	if err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("auth template error: %v", err)
	}
}

func renderAuthError(w http.ResponseWriter, name, msg string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	data["Error"] = msg
	renderAuthTemplate(w, name, data)
}

// GenPasswordHash is called by `make gen-password` via a small CLI helper.
func GenPasswordHash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// ─── Cleanup goroutine ────────────────────────────────────────────────────────

func init() {
	go func() {
		for range time.Tick(10 * time.Minute) {
			sessionStoreMu.Lock()
			for token, entry := range sessionStore {
				if time.Since(entry.CreatedAt) > sessionTTL {
					delete(sessionStore, token)
				}
			}
			sessionStoreMu.Unlock()
		}
	}()
}

// ─── Password scan helper (reads from stdin) ──────────────────────────────────

func ReadPasswordFromStdin() string {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	return strings.TrimSpace(scanner.Text())
}
