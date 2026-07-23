package service

import (
	"context"
	"database/sql"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// HealthCategory — 5 大类错误分类
type HealthCategory string

const (
	HealthAuth     HealthCategory = "auth"     // 凭证与鉴权
	HealthNetwork  HealthCategory = "network"  // 外部网络与API断联
	HealthRuntime  HealthCategory = "runtime"  // 系统性能与运行时
	HealthStorage  HealthCategory = "storage"  // 存储与文件系统
	HealthPlayback HealthCategory = "playback" // 播放与流媒体
)

type HealthStatus struct {
	Healthy  bool   `json:"healthy"`
	Message  string `json:"message,omitempty"`
	LastCheck string `json:"last_check"`
}

type FullHealthReport struct {
	Overall  bool                    `json:"overall"`
	Checks   map[HealthCategory]HealthStatus `json:"checks"`
	Metrics  map[string]interface{}  `json:"metrics"`
}

type HealthChecker struct {
	db          *sql.DB
	pan115      *Pan115Service
	mu          sync.RWMutex
	lastReport  *FullHealthReport
}

var globalHealthChecker *HealthChecker

func NewHealthChecker(db *sql.DB, pan115 *Pan115Service) *HealthChecker {
	h := &HealthChecker{db: db, pan115: pan115}
	globalHealthChecker = h
	return h
}

func GetHealthChecker() *HealthChecker { return globalHealthChecker }

func (h *HealthChecker) RunFullCheck() *FullHealthReport {
	checks := make(map[HealthCategory]HealthStatus)
	overall := true
	now := time.Now().Format("15:04:05")

	// 1. Auth: 115 Cookie validity
	if status := h.checkCookie(); !status.Healthy {
		overall = false
	}
	status := h.checkCookie()
	status.LastCheck = now
	checks[HealthAuth] = status

	// 2. Network: TMDB API reachable
	ns := h.checkTMDB()
	ns.LastCheck = now
	checks[HealthNetwork] = ns
	if !ns.Healthy {
		overall = false
	}

	// 3. Runtime: memory usage < 200MB (basic warning)
	rs := h.checkRuntime()
	rs.LastCheck = now
	checks[HealthRuntime] = rs
	if !rs.Healthy {
		overall = false
	}

	// 4. Storage: DB accessible
	ss := h.checkStorage()
	ss.LastCheck = now
	checks[HealthStorage] = ss
	if !ss.Healthy {
		overall = false
	}

	// 5. Playback: placeholder (media_kit checks on client side)
	checks[HealthPlayback] = HealthStatus{Healthy: true, Message: "Client-side only", LastCheck: now}

	report := &FullHealthReport{
		Overall: overall,
		Checks:  checks,
		Metrics: map[string]interface{}{
			"goroutines": runtime.NumGoroutine(),
			"alloc_mb":   float64(getAlloc()) / (1024 * 1024),
		},
	}

	h.mu.Lock()
	h.lastReport = report
	h.mu.Unlock()

	return report
}

func (h *HealthChecker) checkCookie() HealthStatus {
	if h.pan115 == nil {
		return HealthStatus{Healthy: true, Message: "115 service disabled"}
	}
	valid, err := h.pan115.ValidateCookie(context.Background())
	if err != nil || !valid {
		return HealthStatus{Healthy: false, Message: "115 Cookie已过期，请重新扫码"}
	}
	return HealthStatus{Healthy: true, Message: "115 Cookie有效"}
}

func (h *HealthChecker) checkTMDB() HealthStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "HEAD", "https://api.themoviedb.org/3/configuration", nil)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil || resp == nil || resp.StatusCode >= 500 {
		return HealthStatus{Healthy: false, Message: "TMDB API 不可达"}
	}
	resp.Body.Close()
	return HealthStatus{Healthy: true, Message: "TMDB API 正常"}
}

func (h *HealthChecker) checkRuntime() HealthStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	allocMB := m.Alloc / (1024 * 1024)
	if allocMB > 200 {
		return HealthStatus{Healthy: false, Message: "内存使用过高 (>200MB)"}
	}
	return HealthStatus{Healthy: true, Message: "运行时正常"}
}

func (h *HealthChecker) checkStorage() HealthStatus {
	if h.db == nil {
		return HealthStatus{Healthy: true, Message: "DB not configured"}
	}
	if err := h.db.Ping(); err != nil {
		return HealthStatus{Healthy: false, Message: "数据库连接失败"}
	}
	return HealthStatus{Healthy: true, Message: "数据库正常"}
}

func (h *HealthChecker) GetLastReport() *FullHealthReport {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastReport
}

var getAlloc = func() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc
}
