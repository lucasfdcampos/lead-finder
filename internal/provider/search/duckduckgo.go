// Package search provides a DuckDuckGo HTML-scraping WebSearcher implementation.
package search

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	ddgBase    = "https://html.duckduckgo.com/html/"
	ddgTimeout = 12 * time.Second
	maxResults = 10
)

// DuckDuckGoProvider implements domain.WebSearcher using DDG HTML endpoint.
type DuckDuckGoProvider struct {
	client *http.Client
}

// New creates a new DuckDuckGoProvider.
func New() *DuckDuckGoProvider {
	return &DuckDuckGoProvider{
		client: &http.Client{
			Timeout: ddgTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func (p *DuckDuckGoProvider) Name() string { return "duckduckgo" }

// Search performs a web search and returns up to maxResults results.
func (p *DuckDuckGoProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, ddgTimeout)
	defer cancel()

	formData := url.Values{}
	formData.Set("q", query)
	formData.Set("b", "")
	formData.Set("kl", "br-pt")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ddgBase,
		strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: read body: %w", err)
	}

	results := parseDDGHTML(string(body))
	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}

	return results, nil
}

var (
	reResultLink    = regexp.MustCompile(`<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>([^<]+)</a>`)
	reResultSnippet = regexp.MustCompile(`<a[^>]+class="result__snippet"[^>]*>([\s\S]*?)</a>`)
	reHTMLTags      = regexp.MustCompile(`<[^>]+>`)
)

func parseDDGHTML(html string) []domain.SearchResult {
	links := reResultLink.FindAllStringSubmatch(html, maxResults)
	snippets := reResultSnippet.FindAllStringSubmatch(html, maxResults)

	var results []domain.SearchResult
	for i, m := range links {
		if len(m) < 3 {
			continue
		}
		rawURL := m[1]
		title := strings.TrimSpace(reHTMLTags.ReplaceAllString(m[2], ""))
		realURL := extractRealURL(rawURL)

		snippet := ""
		if i < len(snippets) && len(snippets[i]) > 1 {
			snippet = strings.TrimSpace(reHTMLTags.ReplaceAllString(snippets[i][1], " "))
			snippet = regexp.MustCompile(`\s+`).ReplaceAllString(snippet, " ")
		}

		results = append(results, domain.SearchResult{
			Title:   title,
			URL:     realURL,
			Snippet: snippet,
		})
	}

	return results
}

func extractRealURL(ddgURL string) string {
	if strings.Contains(ddgURL, "uddg=") {
		parsed, err := url.Parse("https:" + ddgURL)
		if err == nil {
			if real := parsed.Query().Get("uddg"); real != "" {
				decoded, err := url.QueryUnescape(real)
				if err == nil {
					return decoded
				}
			}
		}
	}
	if strings.HasPrefix(ddgURL, "//") {
		return "https:" + ddgURL
	}
	return ddgURL
}
