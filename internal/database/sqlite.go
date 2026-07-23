package database

import (
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"

	"github.com/th-sis/x-media-server/internal/config"
)

var (
	instance *sql.DB
	once     sync.Once
)

// Get returns the singleton SQLite connection with WAL mode enabled.
// Safe for concurrent access — SQLite handles serialized writes internally.
func Get(cfg *config.DatabaseConfig) (*sql.DB, error) {
	var initErr error
	once.Do(func() {
		var err error
		// WAL mode: concurrent reads + single writer, perfect for our workload
		// busy_timeout=5000ms: wait up to 5s instead of immediately failing on lock
		instance, err = sql.Open("sqlite3",
			cfg.Path+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_foreign_keys=on")
		if err != nil {
			initErr = fmt.Errorf("sqlite open: %w", err)
			return
		}
		// Single-writer model is optimal for SQLite
		instance.SetMaxOpenConns(1)
		instance.SetMaxIdleConns(1)

		if err = instance.Ping(); err != nil {
			initErr = fmt.Errorf("sqlite ping: %w", err)
			return
		}
		if err = migrate(instance); err != nil {
			initErr = fmt.Errorf("sqlite migrate: %w", err)
			return
		}
		log.Info().Str("path", cfg.Path).Msg("SQLite initialized (WAL mode)")
	})
	return instance, initErr
}

func migrate(db *sql.DB) error {
	schema := `
	-- Core settings (key-value, admin panel persistence)
	CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	);

	-- Admin users (currently single-user for local deployment)
	CREATE TABLE IF NOT EXISTS admin_users (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password TEXT NOT NULL DEFAULT ''
	);

	-- JWT tokens for session management
	CREATE TABLE IF NOT EXISTS tokens (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id       TEXT NOT NULL,
		access_token  TEXT NOT NULL UNIQUE,
		refresh_token TEXT NOT NULL UNIQUE,
		expires_at    TEXT NOT NULL,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	);

	-- Media metadata cache (TMDB-scraped data per media+source)
	CREATE TABLE IF NOT EXISTS media_cache (
		media_id   TEXT NOT NULL,
		source     TEXT NOT NULL DEFAULT 'tmdb',
		title      TEXT NOT NULL DEFAULT '',
		data       TEXT NOT NULL DEFAULT '{}',
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (media_id, source)
	);

	-- Playback history (resume position per media)
	CREATE TABLE IF NOT EXISTS play_history (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    TEXT NOT NULL DEFAULT 'admin',
		media_id   TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		position   INTEGER NOT NULL DEFAULT 0,
		duration   INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE INDEX IF NOT EXISTS idx_play_history_user ON play_history(user_id, media_id);

	-- Transfer tasks (async pan transfer queue)
	CREATE TABLE IF NOT EXISTS transfer_tasks (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		media_id      TEXT NOT NULL,
		source_url    TEXT NOT NULL,
		source_pan    TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL DEFAULT 'pending',
		progress      INTEGER NOT NULL DEFAULT 0,
		result_url    TEXT NOT NULL DEFAULT '',
		error_msg     TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL DEFAULT (datetime('now')),
		completed_at  TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_transfer_media ON transfer_tasks(media_id);

	-- Search engine registry
	CREATE TABLE IF NOT EXISTS search_engines (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT NOT NULL UNIQUE,
		url         TEXT NOT NULL,
		enabled     INTEGER NOT NULL DEFAULT 1,
		latency_ms  INTEGER NOT NULL DEFAULT 0,
		success_pct INTEGER NOT NULL DEFAULT 0
	);

	-- Seed default search engines
	INSERT OR IGNORE INTO search_engines (name, url, enabled) VALUES ('Pansou', 'https://pansou.com/search', 1);
	INSERT OR IGNORE INTO search_engines (name, url, enabled) VALUES ('猫狸盘搜', 'https://www.maolipansou.com/search', 1);
	INSERT OR IGNORE INTO search_engines (name, url, enabled) VALUES ('Go-Pansearch', 'https://go-pansearch.example.com/api', 1);

	-- Seed default admin user
	INSERT OR IGNORE INTO admin_users (username, password) VALUES ('admin', 'admin');
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Create default settings
	defaults := map[string]string{
		"search_max_size":      "50",
		"search_min_quality":   "720P",
		"search_timeout":       "2",
		"search_concurrency":   "5",
		"main_pan":             "115",
		"pan115_threshold":     "10",
		"strm_ttl":             "30",
		"strm_output_path":     "/mnt/IPT-FILES/XXY-FILE/Emby/strm",
	}
	for k, v := range defaults {
		db.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v)
	}
	return nil
}

// ── Settings helpers ──

func SetSetting(key, value string) error {
	_, err := instance.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=datetime('now')`, key, value)
	return err
}

func GetSetting(key string) (string, error) {
	var val string
	err := instance.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func GetAllSettings() (map[string]string, error) {
	rows, err := instance.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}
