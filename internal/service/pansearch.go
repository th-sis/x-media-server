package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── PanSearch Aggregation Engine ──

type SearchEngine struct {
	Name string
	URL  string
}

type SearchResult struct {
	Title   string
	URL     string
	Size    string
	Source  string
	Quality string
	Magnet  string
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
	for i := 0; i < len(s.engines); i++ {
		select {
		case r := <-results:
			if r != nil {
				all = append(all, r...)
			}
		case <-ctx.Done():
			return deduplicateResults(all)
		}
	}
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

func extractField(text, field string) string { return "" }

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
