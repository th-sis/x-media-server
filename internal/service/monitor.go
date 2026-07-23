package service

import (
	"database/sql"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/th-sis/x-media-server/internal/model"
)

// SystemMonitor collects real-time metrics every 10 seconds.
type SystemMonitor struct {
	mu sync.RWMutex

	DB              *sql.DB
	DBPath          string
	ImageCache      *model.ImageCache
	SessionManager  *SessionManager
	TransferStore   *TransferTaskStore

	// Cached metrics
	SessionCount     int       `json:"session_count"`
	PendingTransfers int       `json:"pending_transfers"`
	DBSizeMB         float64   `json:"db_size_mb"`
	CacheHitRate     float64   `json:"cache_hit_rate"`
	CacheSize        int       `json:"cache_size"`
	UpdatedAt        time.Time `json:"updated_at"`
	Version          string    `json:"version"`
}

var globalMonitor *SystemMonitor

func GlobalMonitor() *SystemMonitor { return globalMonitor }

func NewSystemMonitor(db *sql.DB, dbPath string, imgCache *model.ImageCache, sm *SessionManager, ts *TransferTaskStore) *SystemMonitor {
	m := &SystemMonitor{
		DB:             db,
		DBPath:         dbPath,
		ImageCache:     imgCache,
		SessionManager: sm,
		TransferStore:  ts,
		Version:        "v0.1.1-alpha",
	}
	globalMonitor = m
	go m.run()
	return m
}

func (m *SystemMonitor) run() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Collect immediately on start
	m.collect()

	for range ticker.C {
		m.collect()
	}
}

func (m *SystemMonitor) collect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Session count
	sessionCount := 0
	m.SessionManager.sessions.Range(func(key, value interface{}) bool {
		sessionCount++
		return true
	})
	m.SessionCount = sessionCount

	// Pending transfers
	pendingCount := 0
	for _, t := range m.TransferStore.tasks {
		if t.Status == model.TransferPending || t.Status == model.TransferDownloading {
			pendingCount++
		}
	}
	m.PendingTransfers = pendingCount

	// DB file size
	if info, err := os.Stat(m.DBPath); err == nil {
		m.DBSizeMB = float64(info.Size()) / (1024 * 1024)
	}

	// Cache hit rate
	if m.ImageCache != nil {
		m.CacheSize = m.ImageCache.Count()
		m.CacheHitRate = m.ImageCache.HitRate()
	}

	m.UpdatedAt = time.Now()
	log.Debug().
		Int("sessions", m.SessionCount).
		Int("pending_transfers", m.PendingTransfers).
		Float64("db_mb", m.DBSizeMB).
		Float64("cache_hit", m.CacheHitRate).
		Msg("System metrics collected")
}

func (m *SystemMonitor) Snapshot() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]interface{}{
		"version":           m.Version,
		"uptime_secs":       int(time.Since(m.UpdatedAt).Seconds()),
		"session_count":     m.SessionCount,
		"pending_transfers": m.PendingTransfers,
		"db_size_mb":        m.DBSizeMB,
		"cache_hit_rate":    m.CacheHitRate,
		"cache_size":        m.CacheSize,
		"updated_at":        m.UpdatedAt.Format("15:04:05"),
	}
}
