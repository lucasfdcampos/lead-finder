// Package config holds the application configuration structs.
package config

// Config is the root configuration for the lead-finder service.
type Config struct {
	Port      string // PORT (default "8080")
	MongoURI  string // MONGO_URI (default "mongodb://mongo:27017")
	RedisAddr string // REDIS_ADDR — empty means no Redis cache

	Search    SearchConfig
	Geo       GeoConfig
	Providers ExternalProviders
}

// SearchConfig holds API keys for optional web-search backends.
// Priority (highest wins): Google Custom Search > SerpAPI > Brave > DuckDuckGo + Bing (free).
// Serper is disabled.
type SearchConfig struct {
	SerperKey       string // SERPER_API_KEY  — disabled; ignored even if set
	SerpAPIKey      string // SERPAPI_KEY
	GoogleSearchKey string // GOOGLE_SEARCH_API_KEY
	GoogleCX        string // GOOGLE_CX — must be set together with GoogleSearchKey
	BraveKey        string // BRAVE_SEARCH_KEY — Brave Search API, 2000 req/month free
	GroqKey         string // GROQ_API_KEY     — Llama 3.3 70B via Groq (30 req/min free)
}

// GeoConfig holds API keys for geocoding providers.
type GeoConfig struct {
	LocationIQKey string // LOCATIONIQ_API_KEY
}

// ExternalProviders holds API keys for optional place/POI providers.
type ExternalProviders struct {
	FoursquareKey string // FOURSQUARE_API_KEY
	GeoapifyKey   string // GEOAPIFY_API_KEY — free 90k req/month
	TomTomKey     string // TOMTOM_API_KEY   — free 75k req/month
}
