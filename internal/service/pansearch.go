package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// SearchEngineDef — loaded from SQLite, user-editable via admin panel
type SearchEngineDef struct {
	ID          int             `json:"id"`
	Name        string          `json:"name"`
	BaseURL     string          `json:"base_url"`
	Method      string          `json:"method"`
	Headers     json.RawMessage `json:"headers"`
	RegexFilter string          `json:"regex_filter"`
	Weight      int             `json:"weight"`
	Enabled     bool            `json:"enabled"`
}

type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Size    string `json:"size"`
	Source  string `json:"source"`
	Quality string `json:"quality"`
	Magnet  string `json:"magnet"`
}

type PanSearchService struct {
	db     *sql.DB
	client *http.Client
	mu     sync.RWMutex
	cache  []SearchEngineDef // in-memory cache, refreshed on read
}

func NewPanSearchService(db *sql.DB) *PanSearchService {
	return &PanSearchService{
		db:     db,
		client: &http.Client{Timeout: 3 * time.Second},
	}
}

// Load engines from SQLite (with in-memory cache)
func (s *PanSearchService) LoadEngines() ([]SearchEngineDef, error) {
	s.mu.RLock()
	if s.cache != nil {
		defer s.mu.RUnlock()
		return s.cache, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, name, base_url, method, headers, regex_filter, weight, enabled FROM search_engines ORDER BY weight DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var engines []SearchEngineDef
	for rows.Next() {
		var e SearchEngineDef
		var enabled int
		var headers string
		rows.Scan(&e.ID, &e.Name, &e.BaseURL, &e.Method, &headers, &e.RegexFilter, &e.Weight, &enabled)
		e.Enabled = enabled == 1
		if headers != "" {
			e.Headers = json.RawMessage(headers)
		}
		if e.Enabled {
			engines = append(engines, e)
		}
	}
	s.cache = engines
	return engines, nil
}

func (s *PanSearchService) InvalidateCache() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// SearchAll across all enabled engines concurrently
func (s *PanSearchService) SearchAll(query string, timeoutSecs int) []SearchResult {
	engines, err := s.LoadEngines()
	if err != nil {
		log.Error().Err(err).Msg("Failed to load search engines")
		return nil
	}

	results := make(chan []SearchResult, len(engines))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	for _, engine := range engines {
		go func(e SearchEngineDef) {
			r := s.searchOne(ctx, e, query)
			results <- r
		}(engine)
	}

	var all []SearchResult
	for i := 0; i < len(engines); i++ {
		select {
		case r := <-results:
			all = append(all, r...)
		case <-ctx.Done():
			return deduplicateResults(all)
		}
	}
	return deduplicateResults(all)
}

func (s *PanSearchService) searchOne(ctx context.Context, engine SearchEngineDef, query string) []SearchResult {
	url := strings.Replace(engine.BaseURL, "{query}", query, -1)
	url = strings.Replace(url, "{keyword}", query, -1)

	method := engine.Method
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil
	}

	// Apply configured headers
	var headers map[string]string
	if engine.Headers != nil {
		json.Unmarshal(engine.Headers, &headers)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		log.Warn().Str("engine", engine.Name).Err(err).Msg("Search engine unreachable")
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return s.extractResults(string(body), engine)
}

func (s *PanSearchService) extractResults(body string, engine SearchEngineDef) []SearchResult {
	var results []SearchResult

	if engine.RegexFilter != "" {
		re, err := regexp.Compile(engine.RegexFilter)
		if err != nil {
			log.Warn().Str("engine", engine.Name).Err(err).Msg("Invalid regex filter")
			return nil
		}
		matches := re.FindAllString(body, -1)
		for _, m := range matches {
			results = append(results, SearchResult{
				URL:    m,
				Source: engine.Name,
				Magnet: m,
			})
		}
	} else {
		// Fallback: basic magnet extraction
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "magnet:") || strings.Contains(line, "pan.115.com") {
				results = append(results, SearchResult{
					URL:    line,
					Source: engine.Name,
					Magnet: line,
				})
			}
		}
	}
	return results
}

func deduplicateResults(results []SearchResult) []SearchResult {
	seen := make(map[string]bool)
	var unique []SearchResult
	for _, r := range results {
		key := r.Magnet
		if key == "" {
			key = r.URL
		}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, r)
		}
	}
	return unique
}

// ── CRUD for admin panel ──

func (s *PanSearchService) AddEngine(name, baseURL, method, headers, regexFilter string, weight int) error {
	_, err := s.db.Exec(`INSERT INTO search_engines (name, base_url, method, headers, regex_filter, weight) VALUES (?, ?, ?, ?, ?, ?)`,
		name, baseURL, method, headers, regexFilter, weight)
	s.InvalidateCache()
	return err
}

func (s *PanSearchService) UpdateEngine(id int, name, baseURL, method, headers, regexFilter string, weight int) error {
	_, err := s.db.Exec(`UPDATE search_engines SET name=?, base_url=?, method=?, headers=?, regex_filter=?, weight=?, updated_at=datetime('now') WHERE id=?`,
		name, baseURL, method, headers, regexFilter, weight, id)
	s.InvalidateCache()
	return err
}

func (s *PanSearchService) ToggleEngine(id int, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE search_engines SET enabled=?, updated_at=datetime('now') WHERE id=?`, v, id)
	s.InvalidateCache()
	return err
}

func (s *PanSearchService) DeleteEngine(id int) error {
	_, err := s.db.Exec(`DELETE FROM search_engines WHERE id=?`, id)
	s.InvalidateCache()
	return err
}

// Ensure context import is used
var _ = context.Background
var _ = fmt.Sprintf
