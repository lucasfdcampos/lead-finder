// Package search – Bing Web Search provider via RapidAPI.
// Implements domain.WebSearcher as a fallback when DuckDuckGo returns no results.
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
	bingRapidAPIURL  = "https://bing-web-search1.p.rapidapi.com/search"
	bingTimeout      = 10 * time.Second
	bingMaxResults   = 10
)

// BingProvider implements domain.WebSearcher using the Bing Web Search API via RapidAPI.
type BingProvider struct {
	apiKey string
	client *http.Client
}

// NewBing creates a new BingProvider.
func NewBing(apiKey string) *BingProvider {
	return &BingProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: bingTimeout},
	}
}

func (p *BingProvider) Name() string { return "bing" }

// Search queries Bing via RapidAPI and returns generic SearchResult items.
func (p *BingProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, bingTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", bingMaxResults))
	params.Set("mkt", "pt-BR")
	params.Set("safeSearch", "Off")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		bingRapidAPIURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("bing: build request: %w", err)
	}
	req.Header.Set("x-rapidapi-key", p.apiKey)
	req.Header.Set("x-rapidapi-host", "bing-web-search1.p.rapidapi.com")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bing: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 403 {
		return nil, domain.ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bing: read body: %w", err)
	}

	var bingResp bingSearchResponse
	if err := json.Unmarshal(body, &bingResp); err != nil {
		return nil, fmt.Errorf("bing: unmarshal: %w", err)
	}

	var results []domain.SearchResult
	for _, v := range bingResp.WebPages.Value {
		results = append(results, domain.SearchResult{
			Title:   v.Name,
			URL:     v.URL,
			Snippet: v.Snippet,
		})
	}

	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}
	return results, nil
}

// ─── JSON structs ─────────────────────────────────────────────────────────────

type bingSearchResponse struct {
	WebPages struct {
		Value []struct {
			Name    string `json:"name"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"value"`
	} `json:"webPages"`
}
