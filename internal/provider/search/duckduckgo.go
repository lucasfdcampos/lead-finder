// Package search provides DuckDuckGo Lite HTML-scraping WebSearcher implementation.
// DDG Lite (lite.duckduckgo.com) is a lightweight, JavaScript-free endpoint
// designed for basic browsers. It returns plain HTML with no bot detection,
// has no API key requirement and no hard rate limit.
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
	ddgLiteBase = "https://lite.duckduckgo.com/lite/"
	ddgTimeout  = 12 * time.Second
	maxResults  = 10
)

// DuckDuckGoProvider implements domain.WebSearcher using the DDG Lite endpoint.
type DuckDuckGoProvider struct {
	client *http.Client
}

// New creates a new DuckDuckGoProvider (Lite endpoint).
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

func (p *DuckDuckGoProvider) Name() string { return "duckduckgo-lite" }

// Search performs a web search via DDG Lite and returns up to maxResults results.
func (p *DuckDuckGoProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, ddgTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("q", query)
	params.Set("kl", "br-pt") // Brazilian Portuguese results

	reqURL := ddgLiteBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo-lite: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo-lite: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo-lite: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo-lite: HTTP %d", resp.StatusCode)
	}

	results := parseDDGLiteHTML(string(body))
	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}

	return results, nil
}

// DDG Lite HTML structure (single-quoted class attributes):
//
//	<a rel="nofollow" href="//duckduckgo.com/l/?uddg=ENCODED_URL&amp;rut=..." class='result-link'>TITLE</a>
//	<td class='result-snippet'>SNIPPET TEXT (may contain <b> tags)</td>
var (
	// Note: DDG Lite uses single quotes for class attributes.
	reDDGLiteLink    = regexp.MustCompile(`<a[^>]+class='result-link'[^>]*href="([^"]+)"[^>]*>([^<]+)</a>|<a[^>]+href="([^"]+)"[^>]+class='result-link'[^>]*>([^<]+)</a>`)
	reDDGLiteSnippet = regexp.MustCompile(`(?s)<td[^>]+class='result-snippet'[^>]*>\s*(.*?)\s*</td>`)
	reHTMLTags       = regexp.MustCompile(`<[^>]+>`)
	reSpaces         = regexp.MustCompile(`\s+`)
)

func parseDDGLiteHTML(body string) []domain.SearchResult {
	// Extract all result-link anchors (title + href with uddg param)
	links := reDDGLiteLink.FindAllStringSubmatch(body, maxResults)
	// Extract all snippets
	snippets := reDDGLiteSnippet.FindAllStringSubmatch(body, maxResults)

	var results []domain.SearchResult
	for i, m := range links {
		// Two capture groups due to attribute order alternation
		var rawHref, title string
		if m[1] != "" {
			rawHref, title = m[1], m[2]
		} else {
			rawHref, title = m[3], m[4]
		}

		title = strings.TrimSpace(html.UnescapeString(title))
		realURL := extractDDGLiteURL(rawHref)

		snippet := ""
		if i < len(snippets) && len(snippets[i]) > 1 {
			raw := reHTMLTags.ReplaceAllString(snippets[i][1], " ")
			snippet = strings.TrimSpace(reSpaces.ReplaceAllString(html.UnescapeString(raw), " "))
		}

		if title == "" || realURL == "" {
			continue
		}

		results = append(results, domain.SearchResult{
			Title:   title,
			URL:     realURL,
			Snippet: snippet,
		})
	}

	return results
}

// extractDDGLiteURL decodes the real destination URL from a DDG Lite redirect href.
// DDG Lite hrefs look like: //duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com&amp;rut=...
func extractDDGLiteURL(ddgHref string) string {
	// Unescape HTML entities (&amp; → &)
	ddgHref = html.UnescapeString(ddgHref)
	// Make it a full URL so url.Parse works
	if strings.HasPrefix(ddgHref, "//") {
		ddgHref = "https:" + ddgHref
	}
	parsed, err := url.Parse(ddgHref)
	if err != nil {
		return ddgHref
	}
	if real := parsed.Query().Get("uddg"); real != "" {
		decoded, err := url.QueryUnescape(real)
		if err == nil {
			return decoded
		}
	}
	return ddgHref
}
