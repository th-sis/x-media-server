package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ═══════════════════════════════════════════════════════════════════════════
// 115 Service — 扫码登录 / Cookie刷新 / 直链获取 / 防盗链Header注入
// ═══════════════════════════════════════════════════════════════════════════

const (
	pan115UA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	pan115QRAPI   = "https://qrcodeapi.115.com/api/1.0/web/1.0/token"
	pan115StatusAPI = "https://qrcodeapi.115.com/api/1.0/web/1.0/status"
	pan115LoginAPI  = "https://passportapi.115.com/app/1.0/web/1.0/login/qrcode"
	pan115UserAPI   = "https://my.115.com/?ct=ajax&ac=nav"
	pan115FileAPI   = "https://webapi.115.com/files/download"
)

type Pan115Service struct {
	client *http.Client
	db     *sql.DB
}

func NewPan115Service() *Pan115Service {
	return &Pan115Service{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Pan115Service) SetDB(db *sql.DB) { s.db = db }

// ── QR Login Step 1: Get QR code image as base64 ──

type QRResult struct {
	UID       string `json:"uid"`
	QRBase64  string `json:"qr_base64"`
	QRImage   string `json:"qr_image"`
	ExpiresIn int    `json:"expires_in"`
}

func (s *Pan115Service) QRLogin(ctx context.Context) (*QRResult, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", pan115QRAPI, nil)
	req.Header.Set("User-Agent", pan115UA)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("115 QR API unreachable: %w", err)
	}
	defer resp.Body.Close()

	var raw struct {
		State int `json:"state"`
		Data  struct {
			UID    string `json:"uid"`
			QrCode string `json:"qrcode"`
			Time   int    `json:"time"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("115 QR parse error: %w", err)
	}
	if raw.State != 1 {
		return nil, fmt.Errorf("115 API returned error state: %d", raw.State)
	}

	// Fetch QR image and convert to base64
	qrReq, _ := http.NewRequestWithContext(ctx, "GET", raw.Data.QrCode, nil)
	qrResp, err := s.client.Do(qrReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch QR image: %w", err)
	}
	defer qrResp.Body.Close()

	imgBytes, _ := io.ReadAll(qrResp.Body)
	qrBase64 := base64.StdEncoding.EncodeToString(imgBytes)

	return &QRResult{
		UID:      raw.Data.UID,
		QRBase64: "data:image/png;base64," + qrBase64,
		QRImage:  raw.Data.QrCode,
	}, nil
}

// ── QR Login Step 2: Poll for scan status ──

type QRStatus struct {
	Status  int    `json:"status"` // 0=waiting, 1=scanned, 2=confirmed, -1=expired
	Message string `json:"message"`
}

func (s *Pan115Service) QRCheck(ctx context.Context, uid string) (*QRStatus, error) {
	u := fmt.Sprintf("%s?uid=%s&time=%d", pan115StatusAPI, uid, time.Now().UnixMilli())
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", pan115UA)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw struct {
		State int    `json:"state"`
		Msg   string `json:"msg"`
		Data  struct {
			Status int `json:"status"`
			UID    string `json:"uid"`
			CID    string `json:"cid"`
			SEID   string `json:"seid"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	st := &QRStatus{Status: raw.Data.Status, Message: raw.Msg}

	// Step 3: on confirmed, fetch full cookie from login endpoint
	if raw.Data.Status == 2 {
		fullCookie, err := s.fetchFullCookie(ctx, uid)
		if err != nil {
			return st, nil
		}
		st.Message = "登录成功"

		// Persist to SQLite
		if s.db != nil {
			now := time.Now().Format(time.RFC3339)
			_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_cookie", fullCookie, now)
			_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_uid", uid, now)
			_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_user_agent", pan115UA, now)
			_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_last_refresh", now, now)
		}
		log.Info().Str("uid", uid).Msg("115 QR login confirmed — cookie persisted")
	}

	return st, nil
}

func (s *Pan115Service) fetchFullCookie(ctx context.Context, uid string) (string, error) {
	u := fmt.Sprintf("%s?uid=%s", pan115LoginAPI, uid)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", pan115UA)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return resp.Header.Get("Set-Cookie"), nil
}

// ── Cookie Auto-Refresh (detect 990011 silent expiry) ──

func (s *Pan115Service) ValidateCookie(ctx context.Context) (bool, error) {
	cookie, _ := s.getSetting("pan115_cookie")
	if cookie == "" {
		return false, fmt.Errorf("no cookie stored")
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", pan115UserAPI, nil)
	req.Header.Set("User-Agent", pan115UA)
	req.Header.Set("Cookie", cookie)

	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Check for 990011 (not logged in / expired)
	if strings.Contains(bodyStr, "990011") || strings.Contains(bodyStr, "\"state\":false") {
		return false, nil
	}
	return true, nil
}

func (s *Pan115Service) RefreshCookie(ctx context.Context) error {
	uid, _ := s.getSetting("pan115_uid")
	if uid == "" {
		return fmt.Errorf("no UID stored — please re-scan QR")
	}

	cookie, err := s.fetchFullCookie(ctx, uid)
	if err != nil {
		return fmt.Errorf("cookie refresh failed: %w", err)
	}
	if cookie == "" {
		return fmt.Errorf("empty cookie returned from 115")
	}

	now := time.Now().Format(time.RFC3339)
	if s.db != nil {
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_cookie", cookie, now)
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "pan115_last_refresh", now, now)
	}
	log.Info().Msg("115 Cookie auto-refreshed successfully")
	return nil
}

func (s *Pan115Service) getSetting(key string) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("no DB")
	}
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v, err
}

// ── Direct Link with Anti-Hotlink Headers ──

type Pan115DirectLink struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

func (s *Pan115Service) GetDirectLink(ctx context.Context, pickCode string) (*Pan115DirectLink, error) {
	cookie, _ := s.getSetting("pan115_cookie")
	if cookie == "" {
		return nil, fmt.Errorf("no 115 cookie — please scan QR first")
	}

	// Validate cookie freshness
	valid, err := s.ValidateCookie(ctx)
	if err != nil || !valid {
		log.Warn().Msg("115 Cookie expired — attempting auto-refresh")
		if refreshErr := s.RefreshCookie(ctx); refreshErr != nil {
			return nil, fmt.Errorf("cookie expired and refresh failed: %w", refreshErr)
		}
		cookie, _ = s.getSetting("pan115_cookie")
	}

	u := fmt.Sprintf("%s?pickcode=%s", pan115FileAPI, pickCode)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", pan115UA)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", "https://115.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 115 returns direct URL in Location header or JSON body
	dlURL := resp.Header.Get("Location")
	if dlURL == "" {
		var raw struct {
			State int `json:"state"`
			Data  struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(body, &raw) == nil && raw.Data.URL != "" {
			dlURL = raw.Data.URL
		}
	}

	if dlURL == "" {
		return nil, fmt.Errorf("failed to resolve 115 direct link")
	}

	return &Pan115DirectLink{
		URL: dlURL,
		Headers: map[string]string{
			"User-Agent": pan115UA,
			"Cookie":     cookie,
			"Referer":    "https://115.com/",
		},
	}, nil
}

// ── OpenList Token Auto-Refresher ──

type TokenRefresher struct {
	client   *http.Client
	db       *sql.DB
	stopCh   chan struct{}
}

func NewTokenRefresher(db *sql.DB) *TokenRefresher {
	return &TokenRefresher{
		client: &http.Client{Timeout: 10 * time.Second},
		db:     db,
		stopCh: make(chan struct{}),
	}
}

func (tr *TokenRefresher) Start() {
	go tr.loop()
	log.Info().Msg("OpenList Token refresher started (100min interval)")
}

func (tr *TokenRefresher) Stop() {
	close(tr.stopCh)
}

func (tr *TokenRefresher) loop() {
	// Do an immediate refresh on start, then every 100 minutes
	tr.refreshAll()

	ticker := time.NewTicker(100 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-tr.stopCh:
			return
		case <-ticker.C:
			tr.refreshAll()
		}
	}
}

func (tr *TokenRefresher) refreshAll() {
	log.Debug().Msg("TokenRefresher: checking all pan tokens...")

	panTypes := []string{"openlist", "aliyundrive", "quark"}
	for _, pt := range panTypes {
		tokenKey := pt + "_token"
		token, err := tr.getSetting(tokenKey)
		if err != nil || token == "" {
			continue
		}

		// Attempt token refresh via OpenList API
		if pt == "openlist" {
			tr.refreshOpenListToken(token)
		}
	}
}

func (tr *TokenRefresher) refreshOpenListToken(currentToken string) {
	refreshURL, _ := tr.getSetting("openlist_url")
	if refreshURL == "" {
		return
	}

	u := strings.TrimSuffix(refreshURL, "/") + "/api/auth/refresh"
	req, _ := http.NewRequest("POST", u, nil)
	req.Header.Set("Authorization", "Bearer "+currentToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := tr.client.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("OpenList token refresh failed")
		return
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Data.Token != "" {
		now := time.Now().Format(time.RFC3339)
		if tr.db != nil {
			tr.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, "openlist_token", result.Data.Token, now)
		}
		log.Info().Msg("OpenList token refreshed")
	}
}

func (tr *TokenRefresher) getSetting(key string) (string, error) {
	if tr.db == nil {
		return "", fmt.Errorf("no DB")
	}
	var v string
	err := tr.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v, err
}

// ── Directory Cache (anti-rate-limiting) ──

type DirCacheService struct {
	db *sql.DB
}

func NewDirCacheService(db *sql.DB) *DirCacheService {
	dc := &DirCacheService{db: db}
	dc.initTable()
	return dc
}

func (dc *DirCacheService) initTable() {
	if dc.db == nil {
		return
	}
	dc.db.Exec(`CREATE TABLE IF NOT EXISTS dir_caches (
		cache_key   TEXT PRIMARY KEY,
		response    TEXT NOT NULL,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	dc.db.Exec(`CREATE INDEX IF NOT EXISTS idx_dir_caches_created ON dir_caches(created_at)`)
}

func (dc *DirCacheService) Get(key string) (string, bool) {
	if dc.db == nil {
		return "", false
	}
	var resp string
	var createdAt string
	err := dc.db.QueryRow(`SELECT response, created_at FROM dir_caches WHERE cache_key = ?`, key).Scan(&resp, &createdAt)
	if err != nil {
		return "", false
	}

	// Check TTL
	t, err := time.Parse("2006-01-02 15:04:05", createdAt)
	if err != nil || time.Since(t) > 15*time.Minute {
		dc.db.Exec(`DELETE FROM dir_caches WHERE cache_key = ?`, key)
		return "", false
	}
	return resp, true
}

func (dc *DirCacheService) Set(key, response string) {
	if dc.db == nil {
		return
	}
	dc.db.Exec(`INSERT OR REPLACE INTO dir_caches (cache_key, response, created_at) VALUES (?, ?, datetime('now'))`, key, response)
}

func (dc *DirCacheService) Clean() {
	if dc.db == nil {
		return
	}
	dc.db.Exec(`DELETE FROM dir_caches WHERE created_at < datetime('now', '-30 minutes')`)
}

// Ensure context import is used
var _ = context.Background
var _ = url.QueryEscape
