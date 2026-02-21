package config

import (
	"os"

	"github.com/joho/godotenv"
)

// Load reads configuration from environment variables (and an optional .env
// file in the current directory). It never returns a hard error — missing keys
// simply produce empty strings, allowing callers to decide what is optional.
func Load() (*Config, error) {
	// Non-fatal: if no .env file is present the process environment is used.
	_ = godotenv.Load()

	return &Config{
		Port:      envOrDefault("PORT", "8080"),
		MongoURI:  envOrDefault("MONGO_URI", "mongodb://mongo:27017"),
		RedisAddr: os.Getenv("REDIS_ADDR"), // empty = Redis disabled

		Search: SearchConfig{
			SerperKey:       os.Getenv("SERPER_API_KEY"),
			SerpAPIKey:      os.Getenv("SERPAPI_KEY"),
			GoogleSearchKey: os.Getenv("GOOGLE_SEARCH_API_KEY"),
			GoogleCX:        os.Getenv("GOOGLE_CX"),
			BraveKey:        os.Getenv("BRAVE_SEARCH_KEY"),
			GroqKey:         os.Getenv("GROQ_API_KEY"),
		},
		Geo: GeoConfig{
			LocationIQKey: os.Getenv("LOCATIONIQ_API_KEY"),
		},
		Providers: ExternalProviders{
			FoursquareKey: os.Getenv("FOURSQUARE_API_KEY"),
			GeoapifyKey:   os.Getenv("GEOAPIFY_API_KEY"),
			TomTomKey:     os.Getenv("TOMTOM_API_KEY"),
		},
	}, nil
}

// envOrDefault returns the value of the environment variable named by key,
// or def if the variable is unset or empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
