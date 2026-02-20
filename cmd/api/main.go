package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"

	geminiprovider "github.com/lucasfdcampos/lead-finder/internal/provider/gemini"
	cnpjprovider "github.com/lucasfdcampos/lead-finder/internal/provider/cnpj"
	mapsprovider "github.com/lucasfdcampos/lead-finder/internal/provider/maps"
	searchprovider "github.com/lucasfdcampos/lead-finder/internal/provider/search"
	igprovider "github.com/lucasfdcampos/lead-finder/internal/provider/instagram"
	waprovider "github.com/lucasfdcampos/lead-finder/internal/provider/whatsapp"
	"github.com/lucasfdcampos/lead-finder/internal/usecase"
	"github.com/lucasfdcampos/lead-finder/internal/handler"
)

func main() {
	// Load .env (ignore error if file doesn't exist — rely on real env vars in production)
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Configuration ─────────────────────────────────────────────────────
	geminiKey := mustEnv("GEMINI_API_KEY")
	port := envOrDefault("PORT", "8080")

	// ── Wire providers ────────────────────────────────────────────────────
	geminiProv, err := geminiprovider.New(geminiKey)
	if err != nil {
		logger.Error("failed to initialize Gemini provider", "err", err)
		os.Exit(1)
	}
	defer geminiProv.Close()

	brasilAPI := cnpjprovider.NewBrasilAPI()
	cnpjBiz := cnpjprovider.NewCNPJBiz()
	ddg := searchprovider.New()
	igProv := igprovider.New(ddg, geminiProv)
	waProv := waprovider.New(ddg, geminiProv)
	shadowScraper := mapsprovider.NewGoogleMapsShadowScraper(3)

	// ── Wire use-case ─────────────────────────────────────────────────────
	deps := usecase.Dependencies{
		Enricher:           geminiProv,
		CNPJPrimary:        brasilAPI,
		CNPJFallback:       cnpjBiz,
		BusinessDiscoverer: shadowScraper,
		WebSearcher:        ddg,
		InstagramSearcher:  igProv,
		WhatsAppSearcher:   waProv,
	}

	uc := usecase.New(deps, logger)

	// ── Wire HTTP ─────────────────────────────────────────────────────────
	leadHandler := handler.NewLeadHandler(uc)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(130 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	r.Post("/leads", leadHandler.ServeHTTP)

	addr := ":" + port
	logger.Info("server starting", "addr", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 135 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
