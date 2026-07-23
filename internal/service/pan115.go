package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── 115 QR Code Login ──

type Pan115Service struct {
	client *http.Client
}

func NewPan115Service() *Pan115Service {
	return &Pan115Service{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// QRLoginStep1 returns a QR code image URL and session token
func (s *Pan115Service) QRLoginStep1() (*QRResult, error) {
	// 115 API: get QR code
	req, _ := http.NewRequest("GET", "https://qrcodeapi.115.com/api/1.0/web/1.0/token", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("115 QR API unreachable: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		State bool `json:"state"`
		Data  struct {
			UID  string `json:"uid"`
			QrCode string `json:"qrcode"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("115 QR parse error: %w", err)
	}
	if !result.State {
		return nil, fmt.Errorf("115 API returned error")
	}

	return &QRResult{
		UID:    result.Data.UID,
		QRImage: result.Data.QrCode,
	}, nil
}

// QRLoginStep2 polls for scan status
func (s *Pan115Service) QRLoginStep2(uid string) (*QRStatus, error) {
	t := time.Now().UnixMilli()
	url := fmt.Sprintf("https://qrcodeapi.115.com/api/1.0/web/1.0/status?uid=%s&time=%d", uid, t)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		State bool   `json:"state"`
		Msg   string `json:"msg"`
		Data  struct {
			Status int    `json:"status"`
			Cookie string `json:"cookie"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	status := &QRStatus{
		Status:  result.Data.Status,
		Message: result.Msg,
	}
	if result.Data.Status == 2 && result.Data.Cookie != "" {
		status.Cookie = result.Data.Cookie
	}
	return status, nil
}

// QRLoginStep3 gets full cookie with account info
func (s *Pan115Service) QRLoginStep3(uid string) (string, error) {
	url := fmt.Sprintf("https://passportapi.115.com/app/1.0/web/1.0/login/qrcode?uid=%s", uid)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	setCookie := resp.Header.Get("Set-Cookie")
	return setCookie, nil
}

type QRResult struct {
	UID     string `json:"uid"`
	QRImage string `json:"qr_image"`
}

type QRStatus struct {
	Status  int    `json:"status"`  // 0=waiting, 1=scanned, 2=confirmed
	Message string `json:"message"`
	Cookie  string `json:"cookie,omitempty"`
}

// ── 115 API: Get user info (verify cookie) ──

func (s *Pan115Service) GetUserInfo(cookie string) (*Pan115UserInfo, error) {
	req, _ := http.NewRequest("GET", "https://my.115.com/?ct=ajax&ac=nav", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		State bool            `json:"state"`
		Data  *Pan115UserInfo `json:"data"`
	}
	json.Unmarshal(body, &result)

	if result.Data != nil {
		return result.Data, nil
	}
	return nil, fmt.Errorf("invalid cookie")
}

type Pan115UserInfo struct {
	UserID   int64  `json:"user_id"`
	UserName string `json:"user_name"`
	Face     string `json:"face"`
}

// ── 115 API: Get space info ──

func (s *Pan115Service) GetSpaceInfo(cookie string) (*Pan115SpaceInfo, error) {
	req, _ := http.NewRequest("GET", "https://webapi.115.com/files/index_info", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		State bool             `json:"state"`
		Data  *Pan115SpaceInfo `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, nil
}

type Pan115SpaceInfo struct {
	TotalSize string `json:"total_size"`
	UsedSize  string `json:"used_size"`
}

// ── PanSearch Aggregation Engine ──

type SearchEngine struct {
	Name string
	URL  string
}

type SearchResult struct {
	Title    string
	URL      string
	Size     string
	Source   string
	Quality  string
	Magnet   string
}

type PanSearchService struct {
	client  *http.Client
	engines []SearchEngine
}

func NewPanSearchService() *PanSearchService {
	return &PanSearchService{
		client: &http.Client{Timeout: 3 * time.Second},
		engines: []SearchEngine{
			{Name: "Pansou", URL: "https://api.pansou.com/search"},
			{Name: "猫狸盘搜", URL: "https://www.maolipansou.com/api/search"},
			{Name: "Go-Pansearch", URL: "https://go-pansearch.pages.dev/api/search"},
		},
	}
}

func (s *PanSearchService) SearchAll(query string, maxSizeGB float64, minQuality string, timeoutSecs int) []SearchResult {
	results := make(chan []SearchResult, len(s.engines))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	for _, engine := range s.engines {
		go func(e SearchEngine) {
			r, err := s.searchEngine(ctx, e, query)
			if err != nil {
				results <- nil
				return
			}
			results <- r
		}(engine)
	}

	var all []SearchResult
	deadline := time.After(time.Duration(timeoutSecs) * time.Second)
	for i := 0; i < len(s.engines); i++ {
		select {
		case r := <-results:
			if r != nil {
				all = append(all, r...)
			}
		case <-deadline:
			goto done
		case <-ctx.Done():
			goto done
		}
	}
done:
	return deduplicateResults(all)
}

func (s *PanSearchService) searchEngine(ctx context.Context, engine SearchEngine, query string) ([]SearchResult, error) {
	reqURL := fmt.Sprintf("%s?keyword=%s", engine.URL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return parseSearchResults(body, engine.Name), nil
}

func parseSearchResults(body []byte, source string) []SearchResult {
	var results []SearchResult
	text := string(body)

	// Basic extraction: look for magnet links and file info
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "magnet:") || strings.Contains(line, "pan.115.com") {
			results = append(results, SearchResult{
				Title:  extractField(line, "title"),
				URL:    extractField(line, "url"),
				Size:   extractField(line, "size"),
				Source: source,
				Magnet: extractMagnet(line),
			})
		}
	}
	return results
}

func extractField(text, field string) string {
	return ""
}

func extractMagnet(text string) string {
	idx := strings.Index(text, "magnet:?")
	if idx < 0 {
		return ""
	}
	end := strings.IndexAny(text[idx:], " \t\n\"'<>")
	if end < 0 {
		return text[idx:]
	}
	return text[idx : idx+end]
}

func deduplicateResults(results []SearchResult) []SearchResult {
	seen := make(map[string]bool)
	var unique []SearchResult
	for _, r := range results {
		key := r.Title + r.Size
		if !seen[key] {
			seen[key] = true
			unique = append(unique, r)
		}
	}
	return unique
}

// Ensure context import
var _ = context.Background

// Ensure url import
var _ = url.QueryEscape
