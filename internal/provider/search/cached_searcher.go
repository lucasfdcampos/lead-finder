package search

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"github.com/lucasfdcampos/lead-finder/internal/provider/cache"
)

const searchCacheTTL = 1 * time.Hour

// CachedSearcher is a transparent cache decorator around any domain.WebSearcher.
// Search results are stored in Redis keyed by a SHA-256 hash of the query string.
// On a cache miss, the inner provider is called and the result is persisted.
// If Redis is unavailable the decorator falls through to the inner provider
// silently (Get returns false; Set swallows the error).
type CachedSearcher struct {
	inner  domain.WebSearcher
	client *cache.Client
}

// NewCachedSearcher wraps inner with a Redis-backed 1-hour result cache.
func NewCachedSearcher(inner domain.WebSearcher, c *cache.Client) *CachedSearcher {
	return &CachedSearcher{inner: inner, client: c}
}

// Name satisfies domain.DiscoveryProvider; it prepends "cached:" to the inner name.
func (cs *CachedSearcher) Name() string { return "cached:" + cs.inner.Name() }

// Search looks up the query in Redis first. On a hit, the cached slice is
// returned immediately. On a miss, the inner provider is queried and—if it
// returns results—they are persisted with a 1-hour TTL.
func (cs *CachedSearcher) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	key := fmt.Sprintf("search:%x", sha256.Sum256([]byte(query)))

	var cached []domain.SearchResult
	if cs.client.Get(ctx, key, &cached) {
		return cached, nil
	}

	results, err := cs.inner.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		cs.client.Set(ctx, key, results, searchCacheTTL)
	}
	return results, nil
}
