// Package search – ChainedSearcher tries multiple WebSearcher providers in order.
//
// When the primary searcher fails (rate-limited, network error, no results),
// the next provider in the chain is tried automatically. This makes Instagram
// and WhatsApp providers resilient to individual provider failures without
// requiring changes to their internal logic.
package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// ChainedSearcher implements domain.WebSearcher by trying a list of providers
// in order and returning the first successful non-empty result.
type ChainedSearcher struct {
	providers []domain.WebSearcher
}

// NewChained creates a ChainedSearcher from the given providers (tried left to right).
// Nil providers are silently ignored.
func NewChained(providers ...domain.WebSearcher) *ChainedSearcher {
	var valid []domain.WebSearcher
	for _, p := range providers {
		if p != nil {
			valid = append(valid, p)
		}
	}
	return &ChainedSearcher{providers: valid}
}

func (c *ChainedSearcher) Name() string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return fmt.Sprintf("chained(%s)", strings.Join(names, "→"))
}

// Search tries each provider in order and returns the first non-empty result.
// If all providers fail it returns the last error.
func (c *ChainedSearcher) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	var lastErr error
	for _, p := range c.providers {
		results, err := p.Search(ctx, query)
		if err == nil && len(results) > 0 {
			return results, nil
		}
		if err != nil {
			lastErr = err
		}
		// Check context before trying next provider.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, domain.ErrNoResults
}
