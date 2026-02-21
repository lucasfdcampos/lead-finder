// Package search – Bing HTML-scraping WebSearcher implementation.
//
// Bing search is used as a free fallback when DuckDuckGo is rate-limited.
// No API key required. Scrapes the standard search results page.
// Targets pt-BR results via mkt=pt-BR&cc=BR query params.
package search

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	bingBase    = "https://www.bing.com/search"
	bingTimeout = 12 * time.Second
)

// BingProvider implements domain.WebSearcher using Bing HTML scraping.
type BingProvider struct {
	client *http.Client
}

// NewBing creates a new BingProvider.
func NewBing() *BingProvider {
	return &BingProvider{
		client: &http.Client{
			Timeout: bingTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("bing: too many redirects")
				}
				return nil
			},
		},
	}
}

func (p *BingProvider) Name() string { return "bing" }

// Search queries Bing and returns up to 10 organic results.
func (p *BingProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, bingTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("q", query)
	params.Set("mkt", "pt-BR")
	params.Set("cc", "BR")
	params.Set("setlang", "pt-BR")
	params.Set("count", "10")
	params.Set("form", "QBLH")

	reqURL := bingBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bing: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://www.bing.com/")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bing: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bing: read body: %w", err)
	}

	results := parseBingHTML(string(body))
	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}
	return results, nil
}

// Bing organic result structure (simplified):
//
//	<li class="b_algo">
//	  <h2><a href="https://...">Title</a></h2>
//	  <div class="b_caption"><p>Snippet text...</p></div>
//	</li>
var (
	// Extract href + title from the main heading anchor inside b_algo items.
	reBingLink = regexp.MustCompile(`(?i)<h2[^>]*>\s*<a[^>]+href="(https?://[^"]+)"[^>]*>([\s\S]*?)</a>`)
	// Extract the first <p> inside b_caption (snippet).
	reBingSnippet = regexp.MustCompile(`(?i)<div[^>]+class="b_caption"[^>]*>[\s\S]*?<p[^>]*>([\s\S]*?)</p>`)
	// Strip any remaining HTML tags.
	reBingHTMLTags = regexp.MustCompile(`<[^>]+>`)
	reBingSpaces   = regexp.MustCompile(`\s+`)
)

func parseBingHTML(body string) []domain.SearchResult {
	links := reBingLink.FindAllStringSubmatch(body, 15)
	snippets := reBingSnippet.FindAllStringSubmatch(body, 15)

	var results []domain.SearchResult
	for i, m := range links {
		rawURL := m[1]
		rawTitle := m[2]

		// Skip Bing internal / ad URLs.
		if strings.Contains(rawURL, "bing.com") || strings.Contains(rawURL, "microsoft.com") {
			continue
		}

		title := cleanBingText(rawTitle)
		snippet := ""
		if i < len(snippets) && len(snippets[i]) > 1 {
			snippet = cleanBingText(snippets[i][1])
		}

		if title == "" || rawURL == "" {
			continue
		}

		// Decode HTML entities in URL.
		decodedURL := html.UnescapeString(rawURL)

		results = append(results, domain.SearchResult{
			Title:   title,
			URL:     decodedURL,
			Snippet: snippet,
		})

		if len(results) >= 10 {
			break
		}
	}
	return results
}

func cleanBingText(s string) string {
	s = reBingHTMLTags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reBingSpaces.ReplaceAllString(strings.TrimSpace(s), " ")
	return s
}
