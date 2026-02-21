package cnpj

import (
	"context"
	"fmt"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"github.com/lucasfdcampos/lead-finder/internal/provider/cache"
)

const cnpjCacheTTL = 24 * time.Hour

// CachedCNPJProvider is a transparent cache decorator around any domain.CNPJSearcher.
// Only FetchByCNPJ results are cached (keyed by the sanitised 14-digit CNPJ number
// with a 24-hour TTL). SearchByCNAE is always forwarded to the inner provider because
// its result set can change as new companies are registered.
type CachedCNPJProvider struct {
	inner  domain.CNPJSearcher
	client *cache.Client
}

// NewCachedCNPJ wraps inner with a Redis-backed 24-hour FetchByCNPJ cache.
func NewCachedCNPJ(inner domain.CNPJSearcher, c *cache.Client) *CachedCNPJProvider {
	return &CachedCNPJProvider{inner: inner, client: c}
}

// Name satisfies domain.DiscoveryProvider.
func (cp *CachedCNPJProvider) Name() string { return "cached:" + cp.inner.Name() }

// SearchByCNAE is forwarded directly; results are not cached.
func (cp *CachedCNPJProvider) SearchByCNAE(ctx context.Context, cnae, city, state string) ([]domain.Lead, error) {
	return cp.inner.SearchByCNAE(ctx, cnae, city, state)
}

// FetchByCNPJ checks Redis before hitting the upstream API.
// A successful response is cached for 24 hours. Errors from the upstream are
// never cached so a transient failure will be retried on the next request.
func (cp *CachedCNPJProvider) FetchByCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	key := fmt.Sprintf("cnpj:%s", sanitizeCNPJ(cnpj))

	var lead domain.Lead
	if cp.client.Get(ctx, key, &lead) {
		return &lead, nil
	}

	result, err := cp.inner.FetchByCNPJ(ctx, cnpj)
	if err != nil {
		return nil, err
	}
	if result != nil {
		cp.client.Set(ctx, key, result, cnpjCacheTTL)
	}
	return result, nil
}
