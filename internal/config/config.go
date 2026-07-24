package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// Config is the global configuration loaded from environment variables.
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Auth     AuthConfig
	TMDB     TMDBConfig
	Emby     EmbyConfig
	Proxy    ProxyConfig
	Log      LogConfig
	Search   SearchConfig
	Transfer TransferConfig
}

type ServerConfig struct {
	GRPCPort    string // :50051
	HTTPPort    string // :35678
	ExternalURL string // http://192.168.7.154:35678
}

type DatabaseConfig struct {
	Path string // /app/data/xmedia.db
}

type AuthConfig struct {
	JWTSecret       string
	TokenTTL        int // seconds, default 2592000
	RefreshTokenTTL int // seconds, default 7776000
	AdminUsername   string
	AdminPassword   string
}

type TMDBConfig struct {
	APIKey       string
	ImageBaseURL string
}

type EmbyConfig struct {
	ServerURL string
	Username  string
	Password  string
}

type JellyfinConfig struct {
	ServerURL string
	Username  string
	Password  string
}

type PlexConfig struct {
	ServerURL string `json:"plex_server_url"`
	Token     string `json:"plex_token"`
}

type SMBConfig struct {
	IP       string `json:"smb_ip"`
	Share    string `json:"smb_share"`
	Username string `json:"smb_user"`
	Password string `json:"smb_pass"`
}

type WebDAVConfig struct {
	URL      string `json:"webdav_url"`
	Username string `json:"webdav_user"`
	Password string `json:"webdav_pass"`
}

type ProxyConfig struct {
	HTTP  string
	HTTPS string
}

type LogConfig struct {
	Level string // debug | info | warn | error
}

type SearchConfig struct {
	MaxSizeGB     float64 // 50
	MinQuality    string  // 720p
	TimeoutSecs   int     // 2
	Concurrency   int     // 5
}

type TransferConfig struct {
	MainPan      string  // 115
	MainPanCookie string
	Threshold    float64 // space threshold percentage
}

func Load() *Config {
	cfg := &Config{
		Server: ServerConfig{
			GRPCPort:    envOr("SERVER_GRPC_PORT", ":50051"),
			HTTPPort:    envOr("SERVER_HTTP_PORT", ":35678"),
			ExternalURL: envOr("SERVER_EXTERNAL_URL", "http://192.168.7.154:35678"),
		},
		Database: DatabaseConfig{
			Path: envOr("DB_PATH", "/app/data/xmedia.db"),
		},
		Auth: AuthConfig{
			JWTSecret:       envOr("AUTH_JWT_SECRET", "x-media-default-jwt-secret-change-me"),
			TokenTTL:        envInt("AUTH_TOKEN_TTL", 2592000),
			RefreshTokenTTL: envInt("AUTH_REFRESH_TOKEN_TTL", 7776000),
			AdminUsername:   envOr("AUTH_ADMIN_USER", "admin"),
			AdminPassword:   envOr("AUTH_ADMIN_PASS", "admin"),
		},
		TMDB: TMDBConfig{
			APIKey:       os.Getenv("TMDB_API_KEY"),
			ImageBaseURL: "https://image.tmdb.org/t/p",
		},
		Emby: EmbyConfig{
			ServerURL: envOr("EMBY_SERVER_URL", "http://192.168.7.1:2345"),
			Username:  envOr("EMBY_USERNAME", "xiaoya"),
			Password:  os.Getenv("EMBY_PASSWORD"),
		},
		Proxy: ProxyConfig{
			HTTP:  os.Getenv("PROXY_HTTP"),
			HTTPS: os.Getenv("PROXY_HTTPS"),
		},
		Log: LogConfig{
			Level: envOr("LOG_LEVEL", "info"),
		},
		Search: SearchConfig{
			MaxSizeGB:   envFloat("SEARCH_MAX_SIZE_GB", 50),
			MinQuality:  envOr("SEARCH_MIN_QUALITY", "720P"),
			TimeoutSecs: envInt("SEARCH_TIMEOUT_SECS", 2),
			Concurrency: envInt("SEARCH_CONCURRENCY", 5),
		},
		Transfer: TransferConfig{
			MainPan:      envOr("MAIN_PAN", "115"),
			MainPanCookie: os.Getenv("PAN115_COOKIE"),
			Threshold:    envFloat("PAN115_THRESHOLD", 10),
		},
	}
	cfg.LogConfigIssues()
	return cfg
}

// LogConfigIssues prints explicit warnings for required-but-missing env vars.
// Called once at startup so operators immediately know what's misconfigured
// instead of discovering missing keys via cryptic 401s at runtime.
func (c *Config) LogConfigIssues() {
	if c.TMDB.APIKey == "" {
		log.Warn().Msg("⚠ TMDB_API_KEY is empty — TMDB metadata lookup / 健康检查网络类会失败。建议在 .env 设置 TMDB_API_KEY=<your_key>")
	} else {
		log.Info().Str("key_prefix", maskPrefix(c.TMDB.APIKey)).Msg("✅ TMDB API Key loaded")
	}

	if c.Auth.JWTSecret == "x-media-default-jwt-secret-change-me" {
		log.Warn().Msg("⚠ AUTH_JWT_SECRET using default value — 生产环境必须改为强随机字符串")
	}
	if c.Auth.AdminPassword == "admin" {
		log.Warn().Msg("⚠ AUTH_ADMIN_PASS=admin — 生产环境必须修改")
	}
	if c.OpenListProxyURL() == "" {
		log.Info().Msg("ℹ OPENLIST_URL 未配置 — 网盘调度走 sidecar DNS（http://openlist:5244），需 docker compose")
	}
}

// OpenListProxyURL returns the base URL for talking to the OpenList sidecar.
// Empty means "use compose service discovery" — caller should fall back to http://openlist:5244.
func (c *Config) OpenListProxyURL() string {
	if v := strings.TrimSpace(os.Getenv("OPENLIST_URL")); v != "" {
		return v
	}
	return ""
}

func maskPrefix(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:4] + "***"
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
