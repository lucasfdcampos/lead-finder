// Package search – SerpAPI web search provider.
// Free tier: 100 searches/month.
// Sign up at https://serpapi.com/ to get an API key.
// Set SERPAPI_KEY in .env.
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
	serpAPIBase    = "https://serpapi.com/search.json"
	serpAPITimeout = 12 * time.Second
)

// SerpAPIProvider implements domain.WebSearcher using SerpAPI.
type SerpAPIProvider struct {
	apiKey string
	client *http.Client
}

// NewSerpAPI creates a new SerpAPIProvider.
func NewSerpAPI(apiKey string) *SerpAPIProvider {
	return &SerpAPIProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: serpAPITimeout},
	}
}

func (p *SerpAPIProvider) Name() string { return "serpapi" }

// Search queries SerpAPI for Google search results.
func (p *SerpAPIProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, serpAPITimeout)
	defer cancel()

	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("engine", "google")
	params.Set("q", query)
	params.Set("num", "10")
	params.Set("gl", "br")
	params.Set("hl", "pt-br")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		serpAPIBase+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("serpapi: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serpapi: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("serpapi: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serpapi: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseSerpAPIResponse(body)
}

// serpAPIResponse is a partial mapping of the SerpAPI JSON response.
type serpAPIResponse struct {
	OrganicResults []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"organic_results"`
	Error string `json:"error"`
}

func parseSerpAPIResponse(body []byte) ([]domain.SearchResult, error) {
	var data serpAPIResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("serpapi: parse JSON: %w", err)
	}
	if data.Error != "" {
		return nil, fmt.Errorf("serpapi: API error: %s", data.Error)
	}
	if len(data.OrganicResults) == 0 {
		return nil, domain.ErrNoResults
	}

	results := make([]domain.SearchResult, 0, len(data.OrganicResults))
	for _, r := range data.OrganicResults {
		results = append(results, domain.SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}
	return results, nil
}
