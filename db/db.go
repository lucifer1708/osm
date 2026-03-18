package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

// ─── Types ────────────────────────────────────────────────────────────────────

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   string
	IsAdmin      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Session struct {
	Token     string
	UserID    int64
	Username  string
	NeedsTOTP bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Permission defines what a user can do on a bucket+prefix scope.
// Bucket "*" means all buckets. Prefix "" means all paths within the bucket.
// Access is "read" (list/download/preview) or "write" (read + upload/delete/rename/folder).
type Permission struct {
	ID        int64
	UserID    int64
	Bucket    string
	Prefix    string
	Access    string
	CreatedAt time.Time
}

type AuditEntry struct {
	ID        int64
	UserID    *int64
	Username  string
	Event     string
	IP        string
	UserAgent string
	CreatedAt time.Time
}

// ─── Init & Migrate ───────────────────────────────────────────────────────────

func Init(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("db: create dir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_journal=WAL&_timeout=5000&_fk=true")
	if err != nil {
		return fmt.Errorf("db: open: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite only supports one writer
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	// Idempotent: add is_admin column for existing DBs (error ignored if column exists)
	db.Exec(`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`)
	// The first registered user is always admin
	db.Exec(`UPDATE users SET is_admin = 1 WHERE id = (SELECT MIN(id) FROM users) AND is_admin = 0`)
	// Idempotent: create trusted_devices table for existing DBs
	db.Exec(`CREATE TABLE IF NOT EXISTS trusted_devices (
		token      TEXT    PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME NOT NULL
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_trusted_user ON trusted_devices(user_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_trusted_exp  ON trusted_devices(expires_at)`)

	DB = db
	return nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
	PRAGMA journal_mode=WAL;
	PRAGMA foreign_keys=ON;

	CREATE TABLE IF NOT EXISTS users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT    NOT NULL,
		totp_secret   TEXT    NOT NULL DEFAULT '',
		is_admin      INTEGER NOT NULL DEFAULT 0,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token      TEXT    PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		needs_totp INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
	CREATE INDEX IF NOT EXISTS idx_sessions_exp  ON sessions(expires_at);

	CREATE TABLE IF NOT EXISTS audit_log (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
		username   TEXT    NOT NULL DEFAULT '',
		event      TEXT    NOT NULL,
		ip         TEXT    NOT NULL DEFAULT '',
		user_agent TEXT    NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_log(user_id);
	CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(created_at DESC);

	CREATE TABLE IF NOT EXISTS permissions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		bucket     TEXT    NOT NULL DEFAULT '*',
		prefix     TEXT    NOT NULL DEFAULT '',
		access     TEXT    NOT NULL DEFAULT 'read',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, bucket, prefix)
	);

	CREATE INDEX IF NOT EXISTS idx_perm_user ON permissions(user_id);

	CREATE TABLE IF NOT EXISTS trusted_devices (
		token      TEXT    PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_trusted_user ON trusted_devices(user_id);
	CREATE INDEX IF NOT EXISTS idx_trusted_exp  ON trusted_devices(expires_at);
	`)
	return err
}

// ─── User operations ──────────────────────────────────────────────────────────

func CreateUser(username, passwordHash string) (*User, error) {
	// First user ever created gets admin rights
	var count int
	DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	isAdmin := 0
	if count == 0 {
		isAdmin = 1
	}
	res, err := DB.Exec(
		`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?)`,
		username, passwordHash, isAdmin,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return GetUserByID(id)
}

func GetUserByUsername(username string) (*User, error) {
	u := &User{}
	var isAdmin int
	err := DB.QueryRow(
		`SELECT id, username, password_hash, totp_secret, is_admin, created_at, updated_at
		 FROM users WHERE username = ? COLLATE NOCASE`,
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &isAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	u.IsAdmin = isAdmin == 1
	return u, err
}

func GetUserByID(id int64) (*User, error) {
	u := &User{}
	var isAdmin int
	err := DB.QueryRow(
		`SELECT id, username, password_hash, totp_secret, is_admin, created_at, updated_at
		 FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &isAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	u.IsAdmin = isAdmin == 1
	return u, err
}

func GetAllUsers() ([]User, error) {
	rows, err := DB.Query(
		`SELECT id, username, password_hash, totp_secret, is_admin, created_at, updated_at
		 FROM users ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var isAdmin int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &isAdmin, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		users = append(users, u)
	}
	return users, rows.Err()
}

func CountUsers() (int, error) {
	var n int
	err := DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func SetTOTPSecret(userID int64, secret string) error {
	_, err := DB.Exec(
		`UPDATE users SET totp_secret = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		secret, userID,
	)
	return err
}

func UpdatePasswordHash(userID int64, hash string) error {
	_, err := DB.Exec(
		`UPDATE users SET password_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		hash, userID,
	)
	return err
}

func DeleteUser(userID int64) error {
	_, err := DB.Exec(`DELETE FROM users WHERE id = ?`, userID)
	return err
}

// ─── Session operations ───────────────────────────────────────────────────────

func CreateSession(token string, userID int64, needsTOTP bool, ttl time.Duration) error {
	needs := 0
	if needsTOTP {
		needs = 1
	}
	_, err := DB.Exec(
		`INSERT INTO sessions (token, user_id, needs_totp, expires_at)
		 VALUES (?, ?, ?, ?)`,
		token, userID, needs, time.Now().Add(ttl),
	)
	return err
}

func GetSession(token string) (*Session, error) {
	s := &Session{}
	var needsTOTP int
	err := DB.QueryRow(
		`SELECT s.token, s.user_id, u.username, s.needs_totp, s.created_at, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token = ? AND s.expires_at > CURRENT_TIMESTAMP`,
		token,
	).Scan(&s.Token, &s.UserID, &s.Username, &needsTOTP, &s.CreatedAt, &s.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.NeedsTOTP = needsTOTP == 1
	return s, nil
}

func PromoteSession(token string) error {
	_, err := DB.Exec(
		`UPDATE sessions SET needs_totp = 0 WHERE token = ?`,
		token,
	)
	return err
}

func DeleteSession(token string) error {
	_, err := DB.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func DeleteUserSessions(userID int64) error {
	_, err := DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func CleanExpiredSessions() error {
	_, err := DB.Exec(`DELETE FROM sessions WHERE expires_at <= CURRENT_TIMESTAMP`)
	DB.Exec(`DELETE FROM trusted_devices WHERE expires_at <= CURRENT_TIMESTAMP`)
	return err
}

// ─── Trusted device operations ────────────────────────────────────────────────

func CreateTrustedDevice(userID int64, token string, ttl time.Duration) error {
	_, err := DB.Exec(
		`INSERT INTO trusted_devices (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, time.Now().Add(ttl),
	)
	return err
}

// IsTrustedDevice returns true if the token is valid and belongs to the given user.
func IsTrustedDevice(userID int64, token string) bool {
	var count int
	DB.QueryRow(
		`SELECT COUNT(*) FROM trusted_devices
		 WHERE token = ? AND user_id = ? AND expires_at > CURRENT_TIMESTAMP`,
		token, userID,
	).Scan(&count)
	return count > 0
}

func DeleteTrustedDevice(token string) error {
	_, err := DB.Exec(`DELETE FROM trusted_devices WHERE token = ?`, token)
	return err
}

func DeleteUserTrustedDevices(userID int64) error {
	_, err := DB.Exec(`DELETE FROM trusted_devices WHERE user_id = ?`, userID)
	return err
}

// ─── Audit log ────────────────────────────────────────────────────────────────

func LogEvent(userID *int64, username, event, ip, userAgent string) error {
	_, err := DB.Exec(
		`INSERT INTO audit_log (user_id, username, event, ip, user_agent) VALUES (?, ?, ?, ?, ?)`,
		userID, username, event, ip, userAgent,
	)
	return err
}

// ─── Permission operations ────────────────────────────────────────────────────

func GetUserPermissions(userID int64) ([]Permission, error) {
	rows, err := DB.Query(
		`SELECT id, user_id, bucket, prefix, access, created_at
		 FROM permissions WHERE user_id = ? ORDER BY bucket, prefix`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var perms []Permission
	for rows.Next() {
		var p Permission
		if err := rows.Scan(&p.ID, &p.UserID, &p.Bucket, &p.Prefix, &p.Access, &p.CreatedAt); err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// UpsertPermission creates or updates a permission rule.
func UpsertPermission(userID int64, bucket, prefix, access string) error {
	_, err := DB.Exec(
		`INSERT INTO permissions (user_id, bucket, prefix, access)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, bucket, prefix) DO UPDATE SET access = excluded.access`,
		userID, bucket, prefix, access,
	)
	return err
}

// DeletePermission removes a permission rule (verifying it belongs to the given user).
func DeletePermission(id, userID int64) error {
	_, err := DB.Exec(`DELETE FROM permissions WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// EffectiveAccess returns the most specific access level ("read", "write", or "")
// for the given user on the given bucket+path. Considers wildcard bucket "*" and
// prefix inheritance (permission on "photos/" covers "photos/vacation/").
func EffectiveAccess(userID int64, bucket, path string) string {
	perms, err := GetUserPermissions(userID)
	if err != nil || len(perms) == 0 {
		return ""
	}

	type candidate struct {
		access    string
		specific  bool // true = named bucket, false = wildcard "*"
		prefixLen int
	}
	var best *candidate

	for _, p := range perms {
		// bucket must match
		if p.Bucket != "*" && p.Bucket != bucket {
			continue
		}
		// prefix must be an ancestor-or-equal of path
		if p.Prefix != "" && !strings.HasPrefix(path, p.Prefix) {
			continue
		}
		c := candidate{
			access:    p.Access,
			specific:  p.Bucket == bucket,
			prefixLen: len(p.Prefix),
		}
		if best == nil ||
			(c.specific && !best.specific) ||
			(c.specific == best.specific && c.prefixLen > best.prefixLen) {
			best = &c
		}
	}
	if best == nil {
		return ""
	}
	return best.access
}

func GetAuditLog(limit int) ([]AuditEntry, error) {
	rows, err := DB.Query(
		`SELECT id, user_id, username, event, ip, user_agent, created_at
		 FROM audit_log ORDER BY created_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.Event, &e.IP, &e.UserAgent, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
