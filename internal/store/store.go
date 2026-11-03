// Package store persists cluster state in SQLite.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	DB *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, role TEXT NOT NULL DEFAULT 'member',
  api_key_hash TEXT NOT NULL UNIQUE, credits REAL NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS invites (
  code TEXT PRIMARY KEY, role TEXT NOT NULL, uses_left INTEGER NOT NULL, expires INTEGER, created_by TEXT, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY, owner_id TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS project_members (project TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY(project, user_id));
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'pending', paused INTEGER NOT NULL DEFAULT 0, pause_reason TEXT NOT NULL DEFAULT '',
  admin_paused INTEGER NOT NULL DEFAULT 0,
  info TEXT NOT NULL DEFAULT '{}', last_seen INTEGER, connected_since INTEGER,
  online_seconds INTEGER NOT NULL DEFAULT 0, tracked_seconds INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS gpu_classes (id TEXT PRIMARY KEY, name TEXT NOT NULL, vram_mb INTEGER NOT NULL, credits_per_hour REAL NOT NULL);
CREATE TABLE IF NOT EXISTS container_groups (
  id TEXT PRIMARY KEY, project TEXT NOT NULL, name TEXT NOT NULL, owner_id TEXT NOT NULL,
  spec TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL,
  desired_replicas INTEGER NOT NULL DEFAULT 0, autoscale_state TEXT NOT NULL DEFAULT '{}',
  gateway_token TEXT NOT NULL DEFAULT '', start_time INTEGER, finish_time INTEGER,
  created INTEGER NOT NULL, updated INTEGER NOT NULL, UNIQUE(project, name));
CREATE TABLE IF NOT EXISTS instances (
  id TEXT PRIMARY KEY, group_id TEXT NOT NULL, project TEXT NOT NULL, node_id TEXT NOT NULL DEFAULT '',
  gpu_indexes TEXT NOT NULL DEFAULT '[]', version INTEGER NOT NULL, state TEXT NOT NULL,
  ready INTEGER NOT NULL DEFAULT 0, started INTEGER NOT NULL DEFAULT 0, deletion_cost INTEGER NOT NULL DEFAULT 0,
  exit_code INTEGER, message TEXT NOT NULL DEFAULT '', pull_progress REAL NOT NULL DEFAULT 0,
  metrics TEXT NOT NULL DEFAULT '{}', stop_reason TEXT NOT NULL DEFAULT '', imds_secret TEXT NOT NULL,
  created INTEGER NOT NULL, updated INTEGER NOT NULL, assigned_at INTEGER, running_at INTEGER, stop_requested_at INTEGER,
  billed_until INTEGER, replica_index INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_instances_group ON instances(group_id, state);
CREATE INDEX IF NOT EXISTS idx_instances_node ON instances(node_id, state);
CREATE TABLE IF NOT EXISTS node_bans (node_id TEXT NOT NULL, group_id TEXT NOT NULL, until INTEGER NOT NULL, PRIMARY KEY(node_id, group_id));
CREATE TABLE IF NOT EXISTS queues (
  id TEXT PRIMARY KEY, project TEXT NOT NULL, name TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '', settings TEXT NOT NULL DEFAULT '{}', created INTEGER NOT NULL, updated INTEGER NOT NULL,
  UNIQUE(project, name));
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, queue_id TEXT NOT NULL, status TEXT NOT NULL, input TEXT NOT NULL, output TEXT,
  metadata TEXT, webhook TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL,
  timeout_s INTEGER NOT NULL, next_attempt_at INTEGER NOT NULL, instance_id TEXT NOT NULL DEFAULT '',
  last_instance_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', events TEXT NOT NULL DEFAULT '[]',
  seq INTEGER NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_jobs_queue_status ON jobs(queue_id, status, next_attempt_at, seq);
CREATE INDEX IF NOT EXISTS idx_jobs_instance ON jobs(instance_id) WHERE status = 'running';
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, project TEXT NOT NULL DEFAULT '', group_id TEXT NOT NULL DEFAULT '',
  instance_id TEXT NOT NULL DEFAULT '', node_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, message TEXT NOT NULL, time INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_events_group ON events(group_id, id);
CREATE TABLE IF NOT EXISTS logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT, project TEXT NOT NULL, group_id TEXT NOT NULL, instance_id TEXT NOT NULL,
  node_id TEXT NOT NULL, stream TEXT NOT NULL, line TEXT NOT NULL, time INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_logs_group ON logs(group_id, id);
CREATE TABLE IF NOT EXISTS ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT NOT NULL, delta REAL NOT NULL, reason TEXT NOT NULL,
  ref TEXT NOT NULL DEFAULT '', time INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_ledger_user ON ledger(user_id, id);
CREATE TABLE IF NOT EXISTS files (
  project TEXT NOT NULL, path TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, content_type TEXT NOT NULL,
  created INTEGER NOT NULL, PRIMARY KEY(project, path));
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id TEXT PRIMARY KEY, url TEXT NOT NULL, payload TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT '', done INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_webhooks_due ON webhook_deliveries(done, next_attempt_at);
`

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file:mem" + NewID("") + "?mode=memory&cache=shared"
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(0)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and avoids SQLITE_BUSY entirely.
	// Callers must not hold a *sql.Rows open while issuing another query.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// ---------- helpers ----------

// NewID returns a random, URL-safe identifier with an optional prefix.
func NewID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	if prefix == "" {
		return id
	}
	return prefix + "_" + id
}

// NewSecret returns a random hex secret of n bytes.
func NewSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Hash returns the SHA-256 hex digest of a secret.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Now returns the current unix-millis timestamp.
func Now() int64 { return time.Now().UnixMilli() }

// T converts unix millis to time.
func T(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// TP converts a nullable unix-millis value to *time.Time.
func TP(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := T(ms.Int64)
	return &t
}

// JSON marshals v, panicking never (falls back to "null").
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// GetSetting reads a setting.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SettingOr returns the setting or initialises it with def().
func (s *Store) SettingOr(key string, def func() string) (string, error) {
	v, err := s.GetSetting(key)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	v = def()
	return v, s.SetSetting(key, v)
}

// AddEvent records a system event.
func (s *Store) AddEvent(project, groupID, instanceID, nodeID, kind, msg string) {
	_, _ = s.DB.Exec(`INSERT INTO events(project,group_id,instance_id,node_id,kind,message,time) VALUES(?,?,?,?,?,?,?)`,
		project, groupID, instanceID, nodeID, kind, msg, Now())
}
