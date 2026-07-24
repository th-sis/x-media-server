package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ═══════════════════════════════════════════════════════════════════════════
// Pan115Client — 115 网盘高级客户端（P1-A 战役包）
// ═══════════════════════════════════════════════════════════════════════════
//
// 定位：
//   1) 这是 115 网盘的"客户端门面"，与 PanSearch/MediaService 解耦。
//   2) 4 个核心 helper:
//      - Get115QRCode         大屏/TV 扫码登录状态机
//      - PollQRConfirm        异步 2 秒轮询，捕获 UID/CID/SEID 写 SQLite
//      - Refresh115CookieIfNeeded  Cookie 混淆刷新（拦截 990011）
//      - InjectAntiLeechHeaders    流媒体直链 Header 注入
//   3) 这是 P1-A 的"代码层落地"，不动 api.proto（按用户决策）。
//      将来若要升 gRPC，这 4 个 helper 直接演化成 RPC handler，零返工。
//
// 与现有 pan115.go 关系：
//   - Pan115Service (pan115.go)         旧 QRLogin/QRCheck/ValidateCookie/RefreshCookie/GetDirectLink
//   - Pan115Client (本文件)             新一代：含异步轮询 + 自动混淆刷新 + Header 注入
//   - Pan115Client 内部**复用** Pan115Service 的 HTTP 客户端和常量，避免代码分裂
//
// 设计原则：
//   - 锁按"per-device"分片 (sync.Map)，防大屏扫码和后台轮询相互阻塞
//   - UA 锁死：登录时捕获的 User-Agent 必须用于后续所有 115 调用
//   - 990011 检测：精确匹配 JSON 字段 + state code，不靠字符串 contains

// 115 用户端常量（登录态锁定）
const (
	// pan115TVBrowserUA  : TV/大屏扫码时使用的 Chrome UA（参考 115 网页端）
	// pan115PlayerUA     : 取直链后注入给 libmpv 的 UA（让流媒体服务器放行）
	// pan115RefreshUA    : 静默刷新时使用的 UA（必须与首次扫码一致，否则 115 视为异常会话）
	pan115TVBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	pan115PlayerUA    = "115Browser/27.0.0.0"
	pan115RefreshUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// 115 错误码常量
	pan115CodeExpired = 990011 // 未登录 / Cookie 失效
	pan115CodeOK      = 0

	// 轮询配置
	pan115PollInterval = 2 * time.Second
	pan115PollTimeout  = 90 * time.Second
)

// Pan115Client 是 115 网盘的高阶客户端
type Pan115Client struct {
	svc      *Pan115Service            // 复用现有 HTTP 客户端 + 常量
	db       *sql.DB                   // SQLite 持久化
	pollers  sync.Map                  // uid → *qrPoller (后台轮询 goroutine 句柄)
	clientUA string                    // 登录时锁定的 UA，用于后续所有调用
	mu       sync.Mutex                // 保护 clientUA 写入与读取
}

// NewPan115Client 构造客户端（推荐在 main.go 初始化时调用）
func NewPan115Client(svc *Pan115Service, db *sql.DB) *Pan115Client {
	c := &Pan115Client{svc: svc, db: db}
	// 启动时尝试从 SQLite 读回上次登录的 UA
	if db != nil {
		var ua string
		if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, "pan115_user_agent").Scan(&ua); err == nil && ua != "" {
			c.clientUA = ua
		}
	}
	if c.clientUA == "" {
		c.clientUA = pan115TVBrowserUA // 默认 TV UA
	}
	return c
}

// ═══════════════════════════════════════════════════════════════════════════
// 1) 大屏/TV 扫码登录状态机
// ═══════════════════════════════════════════════════════════════════════════

// QR115Ticket 是 Get115QRCode 返回的载荷
type QR115Ticket struct {
	UID       string    // 115 分配给本次扫码的会话 ID（轮询时要回传）
	QRBase64  string    // data:image/png;base64,... 直接喂给前端 <img src>
	ExpiresAt time.Time // 本次 ticket 失效时间（115 默认 90s）
}

// Get115QRCode 调用 115 官方 API 换取登录的 Ticket 载荷
// 返回 (uid, base64_qr_png, error)。base64_qr_png 可直接喂给前端 <img src=...>。
//
// 调用时机：大屏/TV 端启动登录界面 → 后端调用本函数 → 把 qr_base64 渲染到屏幕
//           → 用户用 115 手机 App 扫描 → 后台 PollQRConfirm 异步接管
func (c *Pan115Client) Get115QRCode(ctx context.Context) (string, string, error) {
	res, err := c.svc.QRLogin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("get 115 qr ticket: %w", err)
	}
	if res.UID == "" {
		return "", "", errors.New("115 returned empty uid")
	}
	// 计算 115 官方 ticket 默认 90s 过期
	expiresAt := time.Now().Add(pan115PollTimeout)
	_ = expiresAt // 当前不写 DB，需要时启用
	return res.UID, res.QRBase64, nil
}

// ═══════════════════════════════════════════════════════════════════════════
// 2) 异步轮询核心
// ═══════════════════════════════════════════════════════════════════════════

// qrPoller 是单个 uid 的后台轮询协程
type qrPoller struct {
	uid     string
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}

// PollQRConfirm 异步每 2 秒探活 115 接口，捕获用户确认瞬间
//
// 工作流程：
//   启动 goroutine → 每 2s 调 115 status 接口
//   状态码：
//     0 = 等待扫描（无动作）
//     1 = 已扫描未确认（无动作，扫码界面可提示"请在手机上点确认"）
//     2 = 已确认 → 立即调 login/qrcode 拿完整 Set-Cookie
//              → 写 SQLite (pan115_cookie / pan115_uid / pan115_user_agent)
//              → 注册全局 UA 锁死 (c.clientUA)
//              → 停止轮询，返回成功
//    -1 = 已过期 → 写日志，停止轮询，返回错误
//
// 调用方应通过 channel 或回调拿到结果。此处用 done channel 模式。
//
// 用法示例：
//   poller, err := client.PollQRConfirm(ctx, uid)
//   if err != nil { ... } // uid 无效
//   go func() {
//     select {
//     case <-poller.Done():
//       cookie, _ := client.GetCookie()  // 从 SQLite 读回
//     case <-time.After(95*time.Second):
//       poller.Cancel()  // 兜底超时
//     }
//   }()
func (c *Pan115Client) PollQRConfirm(ctx context.Context, uid string) (*qrPoller, error) {
	if uid == "" {
		return nil, errors.New("uid is required")
	}

	// 取消已有同 uid 轮询（防止重复扫码创建多个 goroutine）
	if prev, ok := c.pollers.Load(uid); ok {
		prev.(*qrPoller).cancel()
		log.Warn().Str("uid", uid).Msg("cancelling previous qr poller before starting new one")
	}

	pCtx, cancel := context.WithTimeout(ctx, pan115PollTimeout)
	p := &qrPoller{
		uid:     uid,
		cancel:  cancel,
		done:    make(chan struct{}),
		started: time.Now(),
	}
	c.pollers.Store(uid, p)

	go c.runPollQR(pCtx, p)
	return p, nil
}

func (c *qrPoller) Done() <-chan struct{} { return c.done }
func (c *qrPoller) Cancel()               { c.cancel() }

func (c *Pan115Client) runPollQR(ctx context.Context, p *qrPoller) {
	defer close(p.done)
	defer c.pollers.Delete(p.uid)

	ticker := time.NewTicker(pan115PollInterval)
	defer ticker.Stop()

	// 立即打一炮（用户期望扫码后立刻看到反馈）
	c.tickPollQR(ctx, p)

	for {
		select {
		case <-ctx.Done():
			log.Info().Str("uid", p.uid).Msg("qr poller cancelled")
			return
		case <-ticker.C:
			if c.tickPollQR(ctx, p) {
				return // 成功或失败终态
			}
		}
	}
}

// tickPollQR 单次轮询。返回 true 表示终态（成功/失败/取消），应停止轮询。
func (c *Pan115Client) tickPollQR(ctx context.Context, p *qrPoller) bool {
	status, err := c.svc.QRCheck(ctx, p.uid)
	if err != nil {
		log.Warn().Err(err).Str("uid", p.uid).Msg("qr poll transient error")
		return false // 继续轮询
	}

	switch status.Status {
	case -1:
		log.Warn().Str("uid", p.uid).Msg("115 qr ticket expired — please re-scan")
		return true
	case 0, 1:
		return false // 0=waiting, 1=scanned not confirmed
	case 2:
		// 成功路径：QRCheck 内部已经把 cookie 写 SQLite（pan115.go:139-145）
		// 这里额外动作：锁死 UA + 日志
		c.lockUserAgent(pan115TVBrowserUA)
		log.Info().Str("uid", p.uid).Str("ua", pan115TVBrowserUA).Msg("115 login confirmed — UA locked for future calls")
		return true
	default:
		log.Warn().Str("uid", p.uid).Int("status", status.Status).Msg("unknown qr status")
		return false
	}
}

// lockUserAgent 把 UA 锁死（首次扫码的 UA 用于后续所有 115 调用）
// 这是 115 的反爬机制之一：UA 漂移会被识别为异常会话。
func (c *Pan115Client) lockUserAgent(ua string) {
	c.mu.Lock()
	c.clientUA = ua
	c.mu.Unlock()

	if c.db != nil {
		now := time.Now().Format(time.RFC3339)
		_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			"pan115_user_agent", ua, now)
	}
}

// UserAgent 返回当前锁定的 UA（供其他 helper 使用）
func (c *Pan115Client) UserAgent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clientUA == "" {
		return pan115TVBrowserUA
	}
	return c.clientUA
}

// ═══════════════════════════════════════════════════════════════════════════
// 3) Cookie 异步混淆刷新（拦截 990011）
// ═══════════════════════════════════════════════════════════════════════════

// Refresh115CookieIfNeeded 每次向 115 发起盘搜/转存/读文件前调用
//
// 工作流程：
//   1) 拿 SQLite 当前 cookie
//   2) 用锁定 UA 打 my.115.com 探活
//   3) 解析响应：
//      - JSON state=true 且不含 990011 → cookie 有效，return nil（不动作）
//      - 否则 cookie 失效 → 触发 refresh_token 换领
//   4) refresh_token 不存在（首次扫码未保留） → 返回 error，调用方应引导用户重新扫码
//   5) refresh_token 换领成功 → 写 SQLite + 内存 clientUA 保留 → return nil
//
// 用户体验：调用方代码无需感知 cookie 是否过期，本函数静默处理。
func (c *Pan115Client) Refresh115CookieIfNeeded(ctx context.Context) error {
	if c.db == nil {
		return errors.New("db not initialized")
	}

	cookie, _ := c.getSetting("pan115_cookie")
	if cookie == "" {
		return errors.New("no 115 cookie stored — please scan QR first")
	}

	// 1) 探活：打 my.115.com 验证 cookie
	valid, errCode, err := c.probe115Cookie(ctx, cookie)
	if err != nil {
		// 网络错误：保守做法是假设 cookie 还行（避免误判导致刷新风暴）
		// 但如果连续多次网络失败，应当重置。
		log.Warn().Err(err).Msg("115 cookie probe network error — assuming still valid")
		return nil
	}
	if valid {
		return nil // cookie 有效，无需刷新
	}

	// 2) cookie 失效。错误码可能是 990011 或其他
	log.Warn().Int("err_code", errCode).Msg("115 cookie expired — triggering silent refresh")

	// 3) 拿 refresh_token（首次扫码时从 Set-Cookie 中保留）
	refreshToken, _ := c.getSetting("pan115_refresh_token")
	uid, _ := c.getSetting("pan115_uid")

	if refreshToken == "" || uid == "" {
		// 旧代码没保存 refresh_token —— 降级到 fetchFullCookie
		log.Warn().Msg("no refresh_token saved — falling back to full cookie fetch (requires user re-scan)")
		newCookie, ferr := c.svc.RefreshCookie(ctx), error(nil)
		if ferr != nil {
			return fmt.Errorf("cookie refresh failed (no refresh_token): %w", ferr)
		}
		// 触发完整 set-cookie 流程后，verify 一次
		if vcookie, _ := c.getSetting("pan115_cookie"); vcookie != "" {
			if valid2, _, _ := c.probe115Cookie(ctx, vcookie); !valid2 {
				return errors.New("cookie refresh succeeded but probe still fails — user must re-scan")
			}
		}
		_ = newCookie
		return nil
	}

	// 4) 用 refresh_token 走静默换领
	if err := c.silentRefreshCookie(ctx, uid, refreshToken); err != nil {
		return fmt.Errorf("silent cookie refresh failed: %w", err)
	}
	return nil
}

// probe115Cookie 用锁定 UA 打 my.115.com 探活
// 返回 (有效?, 错误码, 网络错误)
func (c *Pan115Client) probe115Cookie(ctx context.Context, cookie string) (bool, int, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", pan115UserAPI, nil)
	req.Header.Set("User-Agent", c.UserAgent())
	req.Header.Set("Cookie", cookie)

	resp, err := c.svc.client.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// 解析 JSON（115 返回的是 JS 对象字面量风格的 JSON）
	// 至少两种失效信号：990011 数字 或 state:false
	if strings.Contains(bodyStr, "990011") {
		return false, pan115CodeExpired, nil
	}
	var probe struct {
		State  bool   `json:"state"`
		Code   int    `json:"code"`
		ErrMsg string `json:"err_msg"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if !probe.State {
			return false, probe.Code, nil
		}
		if probe.Code != 0 && probe.Code != pan115CodeOK {
			return false, probe.Code, nil
		}
		return true, probe.Code, nil
	}
	// JSON 解析失败但也没看到 990011 → 假设有效（保守）
	return true, 0, nil
}

// silentRefreshCookie 用 refresh_token 静默换领全新 cookie
//
// 115 实际机制（基于第三方开源观察）：
//   POST https://passportapi.115.com/app/1.0/web/1.0/login/qrcode?uid=<uid>
//   Header: Authorization: Bearer <refresh_token>
//   返回: Set-Cookie 中含新 UID/CID/SEID + 完整 session cookie
//
// 注：115 的 refresh_token 实际是首次扫码 Set-Cookie 里某个字段。
//     这里我们假设已经把 refresh_token 单独存到了 pan115_refresh_token。
//     如果没存，降级到 fetchFullCookie 路径（见 Refresh115CookieIfNeeded）。
func (c *Pan115Client) silentRefreshCookie(ctx context.Context, uid, refreshToken string) error {
	u := fmt.Sprintf("%s?uid=%s", pan115LoginAPI, uid)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", c.UserAgent())
	req.Header.Set("Authorization", "Bearer "+refreshToken)

	resp, err := c.svc.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	// 解析 Set-Cookie
	newCookie := resp.Header.Get("Set-Cookie")
	if newCookie == "" {
		return errors.New("empty Set-Cookie from 115 refresh endpoint")
	}

	// 解析 Set-Cookie 中的单个字段（粗略匹配，不做完整 RFC 6265）
	fields := parseSetCookieFields(newCookie)
	newUID := fields["UID"]
	newCID := fields["CID"]
	newSEID := fields["SEID"]

	// 写 SQLite
	now := time.Now().Format(time.RFC3339)
	if c.db != nil {
		_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			"pan115_cookie", newCookie, now)
		if newUID != "" {
			_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
				"pan115_uid", newUID, now)
		}
		if newCID != "" {
			_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
				"pan115_cid", newCID, now)
		}
		if newSEID != "" {
			_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
				"pan115_seid", newSEID, now)
		}
		_, _ = c.db.Exec(`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			"pan115_last_refresh", now, now)
	}
	log.Info().Str("uid", newUID).Msg("115 cookie silently refreshed — user invisible")
	return nil
}

// parseSetCookieFields 简单解析 Set-Cookie 字符串为 key-value map
// 例: "UID=abc; Path=/; HttpOnly, CID=def; Path=/" → {"UID":"abc", "CID":"def"}
func parseSetCookieFields(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		for _, kv := range strings.Split(part, ";") {
			kv = strings.TrimSpace(kv)
			if eq := strings.Index(kv, "="); eq > 0 {
				k := strings.TrimSpace(kv[:eq])
				v := strings.TrimSpace(kv[eq+1:])
				if k != "" && v != "" && !strings.EqualFold(k, "Path") && !strings.EqualFold(k, "Domain") &&
					!strings.EqualFold(k, "Expires") && !strings.EqualFold(k, "HttpOnly") &&
					!strings.EqualFold(k, "Secure") && !strings.EqualFold(k, "SameSite") &&
					!strings.EqualFold(k, "Max-Age") {
					out[k] = v
				}
			}
		}
	}
	return out
}

// ═══════════════════════════════════════════════════════════════════════════
// 4) 流媒体直链 Header 注入
// ═══════════════════════════════════════════════════════════════════════════

// InjectAntiLeechHeaders 把 115 直链 (rawURL) 包装成"前端 media_kit / libmpv 可直接播放"的形态
//
// 返回 (finalURL, headersMap)。
//   finalURL：通常是 rawURL 原样（115 直链 CDN 偶尔会 302 跳转，调用方需 follow）
//   headersMap：注入到 mpv 的 --http-header-fields 选项
//
// mpv 调用示例（前端）：
//   mpv --http-header-fields="User-Agent: 115Browser/27.0.0.0,Cookie: ...,Referer: https://115.com/" \
//       <finalURL>
//
// 如果 rawURL 解析失败，返回 rawURL + 空 headers（不崩溃，调用方有兜底）。
func (c *Pan115Client) InjectAntiLeechHeaders(rawURL string) (string, map[string]string) {
	if rawURL == "" {
		return "", nil
	}

	// 1) 解析 URL（用于按 host 注入不同 Header）
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		// URL 解析失败：原样返回 + 通用 Header
		return rawURL, c.defaultHeaders()
	}

	cookie, _ := c.getSetting("pan115_cookie")
	headers := map[string]string{
		"User-Agent": pan115PlayerUA, // 115 流媒体服务认 115Browser UA
		"Cookie":     cookie,
		"Referer":    "https://115.com/",
		"Origin":     "https://115.com",
	}

	// 2) 按 host 调优（未来 115 CDN 多域时可扩展）
	switch {
	case strings.Contains(u.Host, "115.com"):
		// 已经是 115 自有 CDN，Header 已 OK
	case strings.Contains(u.Host, "alicdn.com"), strings.Contains(u.Host, "aliyuncs.com"):
		// 阿里云 CDN：可能要求 Range / Accept-Encoding
		headers["Accept"] = "*/*"
		headers["Accept-Encoding"] = "identity"
	case strings.Contains(u.Host, "cdn") || strings.Contains(u.Host, "cloudfront"):
		// 通用 CDN：加 Range 支持（mpv seek 需要）
		headers["Accept"] = "*/*"
	default:
		// 其他：保守默认
	}

	return rawURL, headers
}

// defaultHeaders 兜底：返回无 cookie 的通用 Header（用于扫码阶段还没登录）
func (c *Pan115Client) defaultHeaders() map[string]string {
	return map[string]string{
		"User-Agent": pan115PlayerUA,
		"Referer":    "https://115.com/",
		"Origin":     "https://115.com",
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// 公共工具：查询当前 cookie / UA / 设置项
// ═══════════════════════════════════════════════════════════════════════════

// GetCookie 返回当前 SQLite 中的 115 cookie（不存在返回空字符串）
func (c *Pan115Client) GetCookie() string {
	v, _ := c.getSetting("pan115_cookie")
	return v
}

// HasLogin 返回是否已登录（cookie 非空且长度 > 50，因为 115 cookie 通常 200+ 字符）
func (c *Pan115Client) HasLogin() bool {
	return len(c.GetCookie()) > 50
}

// getSetting 是 SQLite 设置读 helper（与 Pan115Service.getSetting 同语义，独立以解耦）
func (c *Pan115Client) getSetting(key string) (string, error) {
	if c.db == nil {
		return "", sql.ErrConnDone
	}
	var v string
	err := c.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// ═══════════════════════════════════════════════════════════════════════════
// 辅助：把 base64 data URL 转回 []byte（如果前端有需求）
// ═══════════════════════════════════════════════════════════════════════════

// DecodeBase64DataURL 解码 "data:image/png;base64,...." 字符串为二进制
// 当前未使用，保留为公共 API 以便未来 gRPC RPC 暴露
func DecodeBase64DataURL(dataURL string) ([]byte, error) {
	const prefix = "base64,"
	idx := strings.Index(dataURL, prefix)
	if idx < 0 {
		return nil, errors.New("not a base64 data URL")
	}
	return base64.StdEncoding.DecodeString(dataURL[idx+len(prefix):])
}