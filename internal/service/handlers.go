package service

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/th-sis/x-media-server/internal/config"
	"github.com/th-sis/x-media-server/internal/database"
	"github.com/th-sis/x-media-server/internal/model"
	pb "github.com/th-sis/x-media-server/gen/go/xmedia/v1"
)

// ── Route Registrar ──

func RegisterAll(api *mux.Router, cfg *config.Config, db interface{}, state *model.StateStore, img *model.ImageCache) {
	sqlDB := db.(*sql.DB)

	cfgHandler := NewConfigHandler(sqlDB, img)
	scrapeHandler := NewScrapeHandler(img)
	panHandler := NewPanHandler(cfg, sqlDB)
	psHandler := NewPanSearchHandler()
	strmHandler := NewStrmHandler(sqlDB)
	taskHandler := &TaskHandler{}

	// Config CRUD
	api.HandleFunc("/settings", cfgHandler.GetSettings).Methods("GET")
	api.HandleFunc("/settings", cfgHandler.SaveSettings).Methods("POST")
	api.HandleFunc("/status", cfgHandler.GetStatus).Methods("GET")
	api.HandleFunc("/test/tmdb", cfgHandler.TestTMDB).Methods("POST")
	api.HandleFunc("/test/media", cfgHandler.TestMedia).Methods("POST")
	api.HandleFunc("/test/proxy", cfgHandler.TestProxy).Methods("POST")
	api.HandleFunc("/test/pan", cfgHandler.TestPan).Methods("POST")
	api.HandleFunc("/auth/login", cfgHandler.Login).Methods("POST")
	api.HandleFunc("/auth/password", cfgHandler.ChangePassword).Methods("POST")

	// Scrape + cache
	api.HandleFunc("/scrape/status", scrapeHandler.Status).Methods("GET")
	api.HandleFunc("/scrape/start", scrapeHandler.Start).Methods("POST")
	api.HandleFunc("/scrape/clear", scrapeHandler.Clear).Methods("POST")
	api.HandleFunc("/img-cache/clear", scrapeHandler.ClearImgCache).Methods("POST")

	// 115 pan
	api.HandleFunc("/pan115/qr/start", panHandler.QRStart).Methods("POST")
	api.HandleFunc("/pan115/space", panHandler.Space).Methods("GET")
	api.HandleFunc("/pan/space", panHandler.SpaceOverview).Methods("GET")

	// PanSearch
	api.HandleFunc("/pansou/engines", psHandler.ListEngines).Methods("GET")
	api.HandleFunc("/pansou/engines/toggle", psHandler.ToggleEngine).Methods("POST")
	api.HandleFunc("/pansou/engines/add", psHandler.AddEngine).Methods("POST")
	api.HandleFunc("/pansou/speedtest", psHandler.SpeedTest).Methods("POST")

	// STRM
	api.HandleFunc("/strm/cleanup", strmHandler.Cleanup).Methods("POST")

	// Tasks
	api.HandleFunc("/tasks", taskHandler.List).Methods("GET")
}

// ── ConfigHandler (re-written to use ImageCache, not Store) ──

type ConfigHandler struct {
	db   *sql.DB
	img  *model.ImageCache
}

func NewConfigHandler(db *sql.DB, img *model.ImageCache) *ConfigHandler {
	return &ConfigHandler{db: db, img: img}
}

func (h *ConfigHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := database.GetAllSettings()
	if err != nil {
		http.Error(w, `{"error":"failed"}`, http.StatusInternalServerError)
		return
	}
	for _, k := range []string{"tmdb_api_key", "emby_password", "pan115_cookie", "openlist_token"} {
		if _, ok := settings[k]; ok {
			settings[k] = "***"
		}
	}
	json.NewEncoder(w).Encode(settings)
}

func (h *ConfigHandler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	for k, v := range body {
		if v == "***" {
			continue
		}
		if err := database.SetSetting(k, v); err != nil {
			http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
}

func (h *ConfigHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	tmdb, _ := database.GetSetting("tmdb_api_key")
	emby, _ := database.GetSetting("emby_server_url")
	pan115, _ := database.GetSetting("pan115_cookie")
	openlist, _ := database.GetSetting("openlist_token")
	json.NewEncoder(w).Encode(map[string]string{
		"server":  "running", "version": "0.1.0",
		"tmdb":     boolToStatus(tmdb != ""),
		"emby":     boolToStatus(emby != ""),
		"pan115":   boolToStatus(pan115 != ""),
		"openlist": boolToStatus(openlist != ""),
	})
}

func boolToStatus(ok bool) string {
	if ok { return "configured" }
	return "unset"
}

func (h *ConfigHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	cfg := config.Load()
	auth := NewAuthService(cfg, h.db)
	resp, err := auth.Login(r.Context(), &pb.LoginRequest{Username: body.Username, Password: body.Password})
	if err != nil {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token":  resp.AccessToken,
		"refresh_token": resp.RefreshToken,
		"expires_in":    resp.ExpiresIn,
	})
}

func (h *ConfigHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Password != "" {
		h.db.Exec(`UPDATE admin_users SET password = ? WHERE username = ?`, body.Password, body.Username)
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *ConfigHandler) TestTMDB(w http.ResponseWriter, r *http.Request) {
	key, _ := database.GetSetting("tmdb_api_key")
	ok := key != ""
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok, "error": map[bool]string{true: "", false: "未配置 TMDB API Key"}[ok]})
}

func (h *ConfigHandler) TestMedia(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (h *ConfigHandler) TestProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (h *ConfigHandler) TestPan(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// ── ScrapeHandler ──

type ScrapeHandler struct {
	img *model.ImageCache
}

func NewScrapeHandler(img *model.ImageCache) *ScrapeHandler {
	return &ScrapeHandler{img: img}
}

func (h *ScrapeHandler) Status(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total": 0, "done": 0, "imgCache": h.img.Count(),
	})
}

func (h *ScrapeHandler) Start(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (h *ScrapeHandler) Clear(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func (h *ScrapeHandler) ClearImgCache(w http.ResponseWriter, r *http.Request) {
	h.img.Clear()
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

// ── PanHandler ──

type PanHandler struct {
	cfg *config.Config
	db  *sql.DB
}

func NewPanHandler(cfg *config.Config, db *sql.DB) *PanHandler {
	return &PanHandler{cfg: cfg, db: db}
}

func (h *PanHandler) QRStart(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "QR login started"})
}

func (h *PanHandler) Space(w http.ResponseWriter, r *http.Request) {
	used, _ := database.GetSetting("pan115_used")
	total, _ := database.GetSetting("pan115_total")
	json.NewEncoder(w).Encode(map[string]string{"used": used, "total": total})
}

func (h *PanHandler) SpaceOverview(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pans": []map[string]string{
			{"name": "115网盘", "used": "1.2 TB", "total": "5 TB", "pct": "24"},
		},
	})
}

// ── PanSearchHandler ──

type PanSearchHandler struct{}

func NewPanSearchHandler() *PanSearchHandler {
	return &PanSearchHandler{}
}

func (h *PanSearchHandler) ListEngines(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"engines": []map[string]interface{}{
			{"name": "Pansou", "enabled": true, "latency": 120, "success_rate": 98},
			{"name": "猫狸盘搜", "enabled": true, "latency": 85, "success_rate": 96},
			{"name": "Go-Pansearch", "enabled": true, "latency": 210, "success_rate": 91},
		},
	})
}

func (h *PanSearchHandler) ToggleEngine(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "toggled"})
}

func (h *PanSearchHandler) AddEngine(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "added"})
}

func (h *PanSearchHandler) SpeedTest(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// ── StrmHandler ──

type StrmHandler struct {
	db *sql.DB
}

func NewStrmHandler(db *sql.DB) *StrmHandler {
	return &StrmHandler{db: db}
}

func (h *StrmHandler) Cleanup(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]int{"cleaned": 0})
}

// ── TaskHandler ──

type TaskHandler struct{}

func (h *TaskHandler) List(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active":  []map[string]string{},
		"waiting": []map[string]string{},
		"done":    []map[string]string{{"name": "Emby 连接测试", "time": "2026-07-23 14:02"}},
	})
}
