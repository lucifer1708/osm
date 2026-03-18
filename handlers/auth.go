package handlers

import (
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
	"strings"
	"time"

	"github.com/lucifer1708/object_storage_manager/db"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

const sessionTTL = 24 * time.Hour
const cookieName = "osm_session"

// ctxKeySession is the context key for the authenticated session.
type ctxKeySession struct{}

// CurrentSession returns the authenticated session from context, or nil.
func CurrentSession(r *http.Request) *db.Session {
	if s, ok := r.Context().Value(ctxKeySession{}).(*db.Session); ok {
		return s
	}
	return nil
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Always public
		if strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		// First-run: no users yet → only /setup is accessible
		count, _ := db.CountUsers()
		if count == 0 {
			if path != "/setup" {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Public auth paths (only when users exist)
		if path == "/login" || path == "/login/totp" {
			next.ServeHTTP(w, r)
			return
		}

		// Check session
		sess := sessionFromRequest(r)
		if sess == nil {
			redirectToLogin(w, r)
			return
		}

		// Password ok but TOTP pending
		if sess.NeedsTOTP && path != "/login/totp" {
			if isHTMX(r) {
				w.Header().Set("HX-Redirect", "/login/totp")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
			return
		}

		// /setup disabled once users exist
		if path == "/setup" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// Attach session to context
		ctx := context.WithValue(r.Context(), ctxKeySession{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ─── First-run setup ──────────────────────────────────────────────────────────

func SetupPage(w http.ResponseWriter, r *http.Request) {
	renderAuthTemplate(w, "setup.html", nil)
}

func SetupSubmit(w http.ResponseWriter, r *http.Request) {
	// Double-check: only allowed when no users exist
	count, _ := db.CountUsers()
	if count > 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderAuthError(w, "setup.html", "Invalid request", nil)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	if username == "" || len(username) < 2 {
		renderAuthError(w, "setup.html", "Username must be at least 2 characters", nil)
		return
	}
	if len(password) < 8 {
		renderAuthError(w, "setup.html", "Password must be at least 8 characters", nil)
		return
	}
	if password != confirm {
		renderAuthError(w, "setup.html", "Passwords do not match", nil)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		renderAuthError(w, "setup.html", "Failed to hash password", nil)
		return
	}

	user, err := db.CreateUser(username, string(hash))
	if err != nil {
		renderAuthError(w, "setup.html", "Failed to create user: "+err.Error(), nil)
		return
	}

	db.LogEvent(&user.ID, user.Username, "account_created", clientIP(r), r.UserAgent())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ─── Login / Logout ───────────────────────────────────────────────────────────

func LoginPage(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFromRequest(r); sess != nil && !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	renderAuthTemplate(w, "login.html", nil)
}

func LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderAuthError(w, "login.html", "Invalid request", nil)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	ip := clientIP(r)
	ua := r.UserAgent()

	user, err := db.GetUserByUsername(username)
	if err != nil || user == nil {
		db.LogEvent(nil, username, "login_failed", ip, ua)
		renderAuthError(w, "login.html", "Invalid username or password", nil)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		db.LogEvent(&user.ID, user.Username, "login_failed", ip, ua)
		renderAuthError(w, "login.html", "Invalid username or password", nil)
		return
	}

	// Password correct
	if user.TOTPSecret != "" {
		// TOTP required — create a partial session
		if err := newSession(w, user.ID, true); err != nil {
			renderAuthError(w, "login.html", "Session error", nil)
			return
		}
		db.LogEvent(&user.ID, user.Username, "login_password_ok", ip, ua)
		http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
		return
	}

	// No TOTP configured — full session, go set it up
	if err := newSession(w, user.ID, false); err != nil {
		renderAuthError(w, "login.html", "Session error", nil)
		return
	}
	db.LogEvent(&user.ID, user.Username, "login_success", ip, ua)
	http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
}

func Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if sess, _ := db.GetSession(c.Value); sess != nil {
			db.LogEvent(&sess.UserID, sess.Username, "logout", clientIP(r), r.UserAgent())
		}
		db.DeleteSession(c.Value)
	}
	clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ─── TOTP verify (login step 2) ───────────────────────────────────────────────

func TOTPVerifyPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil || !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	renderAuthTemplate(w, "totp_verify.html", nil)
}

func TOTPVerifySubmit(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil || !sess.NeedsTOTP {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderAuthError(w, "totp_verify.html", "Invalid request", nil)
		return
	}

	user, err := db.GetUserByID(sess.UserID)
	if err != nil || user == nil {
		renderAuthError(w, "totp_verify.html", "Session error", nil)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !totp.Validate(code, user.TOTPSecret) {
		db.LogEvent(&user.ID, user.Username, "totp_failed", clientIP(r), r.UserAgent())
		renderAuthError(w, "totp_verify.html", "Invalid code — try again", nil)
		return
	}

	c, _ := r.Cookie(cookieName)
	db.PromoteSession(c.Value)
	db.LogEvent(&user.ID, user.Username, "login_success", clientIP(r), r.UserAgent())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ─── TOTP setup ───────────────────────────────────────────────────────────────

func TOTPSetupPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	user, _ := db.GetUserByID(sess.UserID)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if user.TOTPSecret != "" {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	secret, qrURL, err := generateTOTPAssets(user.Username)
	if err != nil {
		http.Error(w, "Failed to generate TOTP: "+err.Error(), http.StatusInternalServerError)
		return
	}
	renderAuthTemplate(w, "totp_setup.html", map[string]interface{}{
		"Secret":    secret,
		"QRDataURL": template.URL(qrURL),
		"Issuer":    "ObjectStore Manager",
		"Account":   user.Username,
	})
}

func TOTPSetupVerify(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	secret := r.FormValue("secret")
	code := strings.TrimSpace(r.FormValue("code"))

	if !totp.Validate(code, secret) {
		qrURL, _ := totpQRDataURL(sess.Username, secret)
		renderAuthError(w, "totp_setup.html", "Code mismatch — scan the QR again and retry", map[string]interface{}{
			"Secret":    secret,
			"QRDataURL": template.URL(qrURL),
			"Issuer":    "ObjectStore Manager",
			"Account":   sess.Username,
		})
		return
	}

	if err := db.SetTOTPSecret(sess.UserID, secret); err != nil {
		http.Error(w, "Failed to save TOTP secret: "+err.Error(), http.StatusInternalServerError)
		return
	}

	db.LogEvent(&sess.UserID, sess.Username, "totp_setup", clientIP(r), r.UserAgent())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ─── Settings page ────────────────────────────────────────────────────────────

func SettingsPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	user, err := db.GetUserByID(sess.UserID)
	if err != nil || user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	auditLog, _ := db.GetAuditLog(50)
	renderAuthTemplate(w, "settings.html", map[string]interface{}{
		"User":     user,
		"AuditLog": auditLog,
		"HasTOTP":  user.TOTPSecret != "",
	})
}

func ChangePassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		settingsError(w, sess, "Invalid request")
		return
	}

	current := r.FormValue("current_password")
	newPwd := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	user, _ := db.GetUserByID(sess.UserID)
	if user == nil {
		settingsError(w, sess, "User not found")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(current)); err != nil {
		settingsError(w, sess, "Current password is incorrect")
		return
	}
	if len(newPwd) < 8 {
		settingsError(w, sess, "New password must be at least 8 characters")
		return
	}
	if newPwd != confirm {
		settingsError(w, sess, "Passwords do not match")
		return
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte(newPwd), bcrypt.DefaultCost)
	db.UpdatePasswordHash(sess.UserID, string(hash))
	db.DeleteUserSessions(sess.UserID) // invalidate all sessions
	db.LogEvent(&sess.UserID, sess.Username, "password_changed", clientIP(r), r.UserAgent())

	clearCookie(w)
	http.Redirect(w, r, "/login?msg=password_changed", http.StatusSeeOther)
}

func ResetTOTP(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromRequest(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	db.SetTOTPSecret(sess.UserID, "")
	db.LogEvent(&sess.UserID, sess.Username, "totp_reset", clientIP(r), r.UserAgent())
	http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
}

// ─── Session helpers ──────────────────────────────────────────────────────────

func newSession(w http.ResponseWriter, userID int64, needsTOTP bool) error {
	token, err := randomHex(32)
	if err != nil {
		return err
	}
	if err := db.CreateSession(token, userID, needsTOTP, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

func sessionFromRequest(r *http.Request) *db.Session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	sess, err := db.GetSession(c.Value)
	if err != nil {
		log.Printf("session lookup error: %v", err)
		return nil
	}
	return sess
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}


// ─── TOTP helpers ─────────────────────────────────────────────────────────────

func generateTOTPAssets(username string) (secret, qrDataURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "ObjectStore Manager",
		AccountName: username,
	})
	if err != nil {
		return "", "", err
	}
	secret = key.Secret()
	qrDataURL, err = totpQRDataURL(username, secret)
	return secret, qrDataURL, err
}

func totpQRDataURL(username, secret string) (string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "ObjectStore Manager",
		AccountName: username,
		Secret:      decodeBase32Bytes(secret),
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

func decodeBase32Bytes(s string) []byte {
	s = strings.ToUpper(strings.TrimRight(s, "="))
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	var bits uint
	var val uint64
	var out []byte
	for _, c := range s {
		if c == '=' {
			break
		}
		idx := strings.IndexRune(alpha, c)
		if idx < 0 {
			return nil
		}
		val = (val << 5) | uint64(idx)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(val>>bits))
			val &= (1 << bits) - 1
		}
	}
	return out
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func settingsError(w http.ResponseWriter, sess *db.Session, msg string) {
	user, _ := db.GetUserByID(sess.UserID)
	auditLog, _ := db.GetAuditLog(50)
	renderAuthError(w, "settings.html", msg, map[string]interface{}{
		"User":     user,
		"AuditLog": auditLog,
		"HasTOTP":  user != nil && user.TOTPSecret != "",
	})
}

// ─── GenPasswordHash — used by cmd/create-user ────────────────────────────────

func GenPasswordHash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// ─── Template helpers ─────────────────────────────────────────────────────────

func renderAuthTemplate(w http.ResponseWriter, name string, data interface{}) {
	t, err := template.New(name).Funcs(authFuncMap()).ParseFiles("templates/auth/" + name)
	if err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("auth template %s error: %v", name, err)
	}
}

func renderAuthError(w http.ResponseWriter, name, msg string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	data["Error"] = msg
	renderAuthTemplate(w, name, data)
}

func authFuncMap() template.FuncMap {
	return template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("Jan 02, 2006 15:04:05")
		},
		"eventIcon": func(event string) string {
			icons := map[string]string{
				"login_success":    "✅",
				"login_failed":     "❌",
				"login_password_ok": "🔑",
				"logout":           "👋",
				"totp_setup":       "📱",
				"totp_failed":      "⚠️",
				"totp_reset":       "🔄",
				"password_changed": "🔐",
				"account_created":  "👤",
			}
			if icon, ok := icons[event]; ok {
				return icon
			}
			return "📝"
		},
		"eventLabel": func(event string) string {
			labels := map[string]string{
				"login_success":    "Signed in",
				"login_failed":     "Failed sign-in attempt",
				"login_password_ok": "Password verified",
				"logout":           "Signed out",
				"totp_setup":       "2FA enabled",
				"totp_failed":      "Invalid 2FA code",
				"totp_reset":       "2FA reset",
				"password_changed": "Password changed",
				"account_created":  "Account created",
			}
			if label, ok := labels[event]; ok {
				return label
			}
			return event
		},
		"eventColor": func(event string) string {
			switch {
			case strings.Contains(event, "failed"):
				return "text-red-400"
			case strings.Contains(event, "success"), strings.Contains(event, "created"):
				return "text-green-400"
			default:
				return "text-slate-400"
			}
		},
		"fmt": fmt.Sprintf,
		"initial": func(s string) string {
			if s == "" {
				return "?"
			}
			return strings.ToUpper(string([]rune(s)[0]))
		},
	}
}

// ─── Cleanup goroutine ────────────────────────────────────────────────────────

func InitAuth() {
	go func() {
		for range time.Tick(15 * time.Minute) {
			if err := db.CleanExpiredSessions(); err != nil {
				log.Printf("session cleanup: %v", err)
			}
		}
	}()
}
