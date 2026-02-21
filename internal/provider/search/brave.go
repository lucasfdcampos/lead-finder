// Package search – Brave Search API provider.
//
// Brave Search offers a Web Search API with a free tier of 2 000 queries/month
// (no credit card required). Results come from Brave's independent index,
// not Google/Bing, making it a quality alternative when other providers fail.
//
// Sign up at https://api.search.brave.com/
// Set BRAVE_SEARCH_KEY in .env to enable.
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
	braveAPIBase    = "https://api.search.brave.com/res/v1/web/search"
	braveAPITimeout = 10 * time.Second
)

// BraveProvider implements domain.WebSearcher using the Brave Search API.
type BraveProvider struct {
	apiKey string
	client *http.Client
}

// NewBrave creates a new BraveProvider.
func NewBrave(apiKey string) *BraveProvider {
	return &BraveProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: braveAPITimeout},
	}
}

func (p *BraveProvider) Name() string { return "brave" }

// Search performs a web search via Brave Search API.
func (p *BraveProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, braveAPITimeout)
	defer cancel()

	params := url.Values{}
	params.Set("q", query)
	params.Set("country", "BR")
	params.Set("search_lang", "pt")
	params.Set("ui_lang", "pt-BR")
	params.Set("count", "10")
	params.Set("safesearch", "off")

	reqURL := braveAPIBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("brave: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return nil, domain.ErrRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("brave: invalid API key (HTTP %d)", resp.StatusCode)
	case http.StatusOK:
		// ok
	default:
		return nil, fmt.Errorf("brave: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("brave: read body: %w", err)
	}

	var response braveSearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("brave: unmarshal: %w", err)
	}

	var results []domain.SearchResult
	for _, r := range response.Web.Results {
		snippet := r.Description
		if snippet == "" {
			snippet = r.ExtraSnippets
		}
		results = append(results, domain.SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: snippet,
		})
	}

	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}
	return results, nil
}

// ─── Brave API response types ────────────────────────────────────────────────

type braveSearchResponse struct {
	Web struct {
		Results []braveWebResult `json:"results"`
	} `json:"web"`
}

type braveWebResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Description   string `json:"description"`
	ExtraSnippets string `json:"extra_snippets"`
}
