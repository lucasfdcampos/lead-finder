// Package search – Serper.dev Google search provider.
// Serper wraps Google Search results via a simple POST JSON API.
// Free tier: 2 500 searches/month (no credit card required).
// Signup: https://serper.dev/
// Set SERPER_API_KEY in .env to enable.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	serperAPIBase    = "https://google.serper.dev/search"
	serperAPITimeout = 10 * time.Second
)

// SerperProvider implements domain.WebSearcher using Serper.dev (Google results).
type SerperProvider struct {
	apiKey string
	client *http.Client
}

// NewSerper creates a new SerperProvider.
func NewSerper(apiKey string) *SerperProvider {
	return &SerperProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: serperAPITimeout},
	}
}

func (p *SerperProvider) Name() string { return "serper" }

// Search queries Serper.dev for Google search results.
func (p *SerperProvider) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, serperAPITimeout)
	defer cancel()

	payload := map[string]interface{}{
		"q":   query,
		"gl":  "br",
		"hl":  "pt-br",
		"num": 10,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("serper: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serperAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("serper: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serper: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("serper: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serper: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	return parseSerperResponse(respBody)
}

// serperResponse is a partial mapping of the Serper.dev JSON response.
type serperResponse struct {
	Organic []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"organic"`
	Error string `json:"error,omitempty"`
}

func parseSerperResponse(body []byte) ([]domain.SearchResult, error) {
	var data serperResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("serper: parse JSON: %w", err)
	}
	if data.Error != "" {
		return nil, fmt.Errorf("serper: API error: %s", data.Error)
	}
	if len(data.Organic) == 0 {
		return nil, domain.ErrNoResults
	}

	results := make([]domain.SearchResult, 0, len(data.Organic))
	for _, r := range data.Organic {
		if r.Link == "" {
			continue
		}
		results = append(results, domain.SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}

	if len(results) == 0 {
		return nil, domain.ErrNoResults
	}
	return results, nil
}
