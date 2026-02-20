package domain

import "context"

// DiscoveryProvider is the base interface every provider must satisfy.
type DiscoveryProvider interface {
	Name() string
}

// QueryEnricher translates a raw user query into structured search context.
type QueryEnricher interface {
	DiscoveryProvider
	Enrich(ctx context.Context, query, location string) (*GeminiSearchContext, error)
}

// CNPJSearcher finds CNPJ records that match a search context.
type CNPJSearcher interface {
	DiscoveryProvider
	SearchByCNAE(ctx context.Context, cnae, city, state string) ([]Lead, error)
	FetchByCNPJ(ctx context.Context, cnpj string) (*Lead, error)
}

// InstagramSearcher finds and validates Instagram profiles for a business.
type InstagramSearcher interface {
	DiscoveryProvider
	FindHandle(ctx context.Context, businessName, location string) (string, error)
	ValidateProfile(ctx context.Context, handle string) (followers int, ok bool, err error)
}

// WhatsAppSearcher finds WhatsApp numbers for a business.
type WhatsAppSearcher interface {
	DiscoveryProvider
	FindNumber(ctx context.Context, businessName, location string) (string, error)
}

// WebSearcher performs generic web searches.
type WebSearcher interface {
	DiscoveryProvider
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// SearchResult is a generic web-search result row.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// BusinessDiscoverer discovers raw business names from web data sources
// (e.g. Google Maps via DuckDuckGo shadow-scraping).
type BusinessDiscoverer interface {
	DiscoveryProvider
	// DiscoverBusinessNames returns raw business name strings for the given
	// query and location, potentially across multiple paginated requests.
	DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error)
}
