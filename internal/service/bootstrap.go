package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// OpenListBootstrapper auto-configures OpenList on first boot
// so users don't need to visit OpenList's own admin panel.
type OpenListBootstrapper struct {
	baseURL string
	db      *sql.DB
	client  *http.Client
}

func NewOpenListBootstrapper(db *sql.DB) *OpenListBootstrapper {
	b := &OpenListBootstrapper{
		db:     db,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	// Read base URL from settings (may be overwritten by env)
	b.baseURL, _ = getSetting(db, "openlist_url")
	if b.baseURL == "" {
		b.baseURL = "http://openlist:5244"
	}
	return b
}

// Bootstrap runs on server start. If OpenList has no admin token configured,
// it auto-initializes one and stores it in our SQLite.
func (b *OpenListBootstrapper) Bootstrap() {
	time.Sleep(5 * time.Second) // Wait for OpenList to be ready

	// Check if we already have a token
	token, _ := getSetting(b.db, "openlist_token")
	if token != "" {
		// Verify existing token
		if b.verifyToken(token) {
			log.Info().Msg("OpenList token verified — already configured")
			return
		}
		log.Warn().Msg("OpenList token invalid — re-initializing")
	}

	// Generate a new admin token
	newToken := b.initializeOpenList()
	if newToken != "" {
		_, _ = b.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))`,
			"openlist_token", newToken)
		log.Info().Msg("OpenList auto-initialized — token stored")
	}
}

func (b *OpenListBootstrapper) initializeOpenList() string {
	// Try to get existing admin token from OpenList
	req, _ := http.NewRequest("GET", b.baseURL+"/api/admin/token", nil)
	resp, err := b.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("OpenList bootstrap: API not reachable")
		return b.fallbackInit()
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var result struct {
			Code int `json:"code"`
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Data.Token != "" {
			return result.Data.Token
		}
	}

	// Fallback: try default password-based init
	return b.fallbackInit()
}

func (b *OpenListBootstrapper) fallbackInit() string {
	// OpenList default: first boot with no password → login with admin:admin
	body := map[string]string{"username": "admin", "password": "admin"}
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", b.baseURL+"/api/auth/login", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("OpenList fallback login failed")
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Data.Token != "" {
		return result.Data.Token
	}
	return ""
}

func (b *OpenListBootstrapper) verifyToken(token string) bool {
	req, _ := http.NewRequest("GET", b.baseURL+"/api/admin/meta", nil)
	req.Header.Set("Authorization", token)
	resp, err := b.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// getSetting reads a setting from SQLite
func getSetting(db *sql.DB, key string) (string, error) {
	if db == nil {
		return "", sql.ErrConnDone
	}
	var v string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v, err
}

// Ensure context import
var _ = context.Background
