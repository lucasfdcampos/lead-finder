package domain

import "context"

// DiscoveryProvider is the base interface every provider must satisfy.
type DiscoveryProvider interface {
	Name() string
}

// CNAEEnricher translates a raw user query into CNAE-based search context.
// It uses listacnae.com.br (cached in MongoDB) to find the best matching CNAE code.
type CNAEEnricher interface {
	DiscoveryProvider
	FindCNAE(ctx context.Context, query string) (*SearchContext, error)
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
	// ValidateProfileWithHints validates and confirms the profile using city/phone bio hints.
	ValidateProfileWithHints(ctx context.Context, handle, city, phone string) (followers int, ok bool, err error)
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

// CNPJNameSearcher looks up a CNPJ by company name and city.
// It goes directly to a CNPJ database, bypassing web-search rate limits.
type CNPJNameSearcher interface {
	DiscoveryProvider
	// SearchByName returns the 14-digit CNPJ string for the best matching company,
	// or ("", nil) when not found, or an error on provider failure.
	SearchByName(ctx context.Context, name, city string) (string, error)
}

// BusinessDiscoverer discovers raw business names from web data sources
// (e.g. Google Maps via DuckDuckGo shadow-scraping).
type BusinessDiscoverer interface {
	DiscoveryProvider
	// DiscoverBusinessNames returns raw business name strings for the given
	// query and location, potentially across multiple paginated requests.
	DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error)
}

// PlacesSearcher queries a POI database (e.g. Foursquare) for venue leads.
// It is only invoked when Gemini sets UseFoursquare=true for the query.
type PlacesSearcher interface {
	DiscoveryProvider
	// SearchPlaces returns business leads for a query around a lat/lon coordinate.
	// radiusM is the search radius in metres; 0 lets the provider choose a default.
	SearchPlaces(ctx context.Context, query, latlon string, radiusM int) ([]Lead, error)
}

// Geocoder converts a human-readable location string into lat/lon coordinates.
type Geocoder interface {
	DiscoveryProvider
	Geocode(ctx context.Context, location string) (latlon string, err error)
}
