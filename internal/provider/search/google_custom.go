// Package search – Google Custom Search JSON API provider.
// Free tier: 100 queries/day.
// Requires a Google API key with "Custom Search API" enabled and a CX (Programmable
// Search Engine) ID configured to search the whole web.
//
// Setup:
//  1. Go to https://programmablesearchengine.google.com/ → Create search engine
//     → tick "Search the entire web"  → copy the CX id.
//  2. Enable "Custom Search API" in Google Cloud Console for the same project
//     that owns your GEMINI_API_KEY (or create a new API key there).
//  3. Set GOOGLE_SEARCH_API_KEY and GOOGLE_CX in .env.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	googleCSEBase    = "https://www.googleapis.com/customsearch/v1"
	googleCSETimeout = 10 * time.Second
	googleCSEMax     = 10 // max results per request (API hard limit)
)

// GoogleCustomSearch implements domain.WebSearcher using Google Custom Search JSON API.
type GoogleCustomSearch struct {
	apiKey string
	cx     string
	client *http.Client
}

// NewGoogleCustomSearch creates a new GoogleCustomSearch provider.
// apiKey is a Google Cloud API key with Custom Search API enabled.
// cx is the Programmable Search Engine ID.
func NewGoogleCustomSearch(apiKey, cx string) *GoogleCustomSearch {
	return &GoogleCustomSearch{
		apiKey: apiKey,
		cx:     cx,
		client: &http.Client{Timeout: googleCSETimeout},
	}
}

func (p *GoogleCustomSearch) Name() string { return "google-custom-search" }

// Search queries Google Custom Search and returns up to 10 results.
func (p *GoogleCustomSearch) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, googleCSETimeout)
	defer cancel()

	params := url.Values{}
	params.Set("key", p.apiKey)
	params.Set("cx", p.cx)
	params.Set("q", query)
	params.Set("num", fmt.Sprintf("%d", googleCSEMax))
	params.Set("lr", "lang_pt")
	params.Set("gl", "br")
	params.Set("hl", "pt-BR")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		googleCSEBase+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("google-cse: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google-cse: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("google-cse: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google-cse: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseGoogleCSEResponse(body)
}

// googleCSEResponse is a partial mapping of the Custom Search API JSON response.
type googleCSEResponse struct {
	Items []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"items"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseGoogleCSEResponse(body []byte) ([]domain.SearchResult, error) {
	var data googleCSEResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("google-cse: parse JSON: %w", err)
	}
	if data.Error != nil {
		return nil, fmt.Errorf("google-cse: API error %d: %s", data.Error.Code, data.Error.Message)
	}
	if len(data.Items) == 0 {
		return nil, domain.ErrNoResults
	}

	results := make([]domain.SearchResult, 0, len(data.Items))
	for _, item := range data.Items {
		results = append(results, domain.SearchResult{
			Title:   item.Title,
			URL:     item.Link,
			Snippet: item.Snippet,
		})
	}
	return results, nil
}

// truncate limits a string to n bytes for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
