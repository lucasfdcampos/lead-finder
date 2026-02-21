package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/lucasfdcampos/lead-finder/internal/config"
	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"github.com/lucasfdcampos/lead-finder/internal/handler"
	cacheprovider "github.com/lucasfdcampos/lead-finder/internal/provider/cache"
	cnaeprovider "github.com/lucasfdcampos/lead-finder/internal/provider/cnae"
	cnpjprovider "github.com/lucasfdcampos/lead-finder/internal/provider/cnpj"
	dirprovider "github.com/lucasfdcampos/lead-finder/internal/provider/directory"
	fsqprovider "github.com/lucasfdcampos/lead-finder/internal/provider/foursquare"
	geoapifyprovider "github.com/lucasfdcampos/lead-finder/internal/provider/geoapify"
	geoprovider "github.com/lucasfdcampos/lead-finder/internal/provider/geocoder"
	igprovider "github.com/lucasfdcampos/lead-finder/internal/provider/instagram"
	placesprovider "github.com/lucasfdcampos/lead-finder/internal/provider/places"
	searchprovider "github.com/lucasfdcampos/lead-finder/internal/provider/search"
	tomtomprovider "github.com/lucasfdcampos/lead-finder/internal/provider/tomtom"
	waprovider "github.com/lucasfdcampos/lead-finder/internal/provider/whatsapp"
	"github.com/lucasfdcampos/lead-finder/internal/usecase"
)

func main() {
	logLevel := slog.LevelInfo
	if strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// ── Configuration ─────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// ── Redis cache (optional) ─────────────────────────────────────────────────
	var redisClient *cacheprovider.Client
	if cfg.RedisAddr != "" {
		redisClient = cacheprovider.NewClient(cfg.RedisAddr)
		defer redisClient.Close()
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := redisClient.Ping(pingCtx); err != nil {
			logger.Warn("redis unreachable — running without cache", "addr", cfg.RedisAddr, "err", err)
			redisClient = nil
		} else {
			logger.Info("redis cache enabled", "addr", cfg.RedisAddr)
		}
		pingCancel()
	}

	// ── CNAE provider (embedded CNAE 2.0 data + MongoDB cache) ────────────────
	cnaeProv, err := cnaeprovider.New(cfg.MongoURI)
	if err != nil {
		logger.Error("failed to initialize CNAE provider", "err", err)
		os.Exit(1)
	}
	defer cnaeProv.Close()

	// DDG is used as a fallback for CNAE lookup when keyword matching scores 0.
	// Injected before EnsureCache so it is available during warm-up if needed.
	cnaeProv.WithWebSearcher(searchprovider.New())

	// Pre-warm CNAE cache: parses embedded cnaes.json on first run, then reads from MongoDB.
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	cnaeProv.EnsureCache(warmCtx)
	warmCancel()

	// ── CNPJ providers ────────────────────────────────────────────────────────
	brasilAPI := cnpjprovider.NewBrasilAPI()
	cnpjBiz := cnpjprovider.NewCNPJBiz()
	openCNPJA := cnpjprovider.NewOpenCNPJA()
	publicaCNPJWS := cnpjprovider.NewPublicaCNPJWS()

	// Fetch chain: BrasilAPI (primary, free, fast) → CNPJBiz (1st fallback, scraper)
	// → openCNPJA (2nd fallback) → publicaCNPJWS (3rd fallback).
	var cnpjPrimary domain.CNPJSearcher = brasilAPI
	var cnpjFallback domain.CNPJSearcher = cnpjBiz
	if redisClient != nil {
		cnpjPrimary = cnpjprovider.NewCachedCNPJ(brasilAPI, redisClient)
		cnpjFallback = cnpjprovider.NewCachedCNPJ(cnpjBiz, redisClient)
	}

	// ── Web search providers ───────────────────────────────────────────────────
	// Priority (highest wins): Google Custom Search > SerpAPI > Brave > DDG + Bing (free).
	// Serper is disabled — key ignored even if present in .env.
	//
	// Free (no key) mode: DDG primary → Bing fallback.
	// This gives two independent scrapers so rate-limiting on one doesn't block both.
	ddgSearcher := searchprovider.New()
	bingSearcher := searchprovider.NewBing()
	var primarySearcher domain.WebSearcher = ddgSearcher
	var fallbackSearcher domain.WebSearcher = bingSearcher // Bing is always available as free fallback

	// Serper disabled — key ignored even if set in .env.
	// if cfg.Search.SerperKey != "" { ... }

	if cfg.Search.BraveKey != "" {
		primarySearcher = searchprovider.NewBrave(cfg.Search.BraveKey)
		fallbackSearcher = ddgSearcher
		logger.Info("brave search enabled (primary, 2000 req/month free)")
	}
	if cfg.Search.SerpAPIKey != "" {
		primarySearcher = searchprovider.NewSerpAPI(cfg.Search.SerpAPIKey)
		fallbackSearcher = ddgSearcher
		logger.Info("serpapi search enabled (primary)")
	}
	if cfg.Search.GoogleSearchKey != "" && cfg.Search.GoogleCX != "" {
		primarySearcher = searchprovider.NewGoogleCustomSearch(cfg.Search.GoogleSearchKey, cfg.Search.GoogleCX)
		fallbackSearcher = ddgSearcher
		logger.Info("google custom search enabled (primary)")
	}
	if primarySearcher == ddgSearcher {
		logger.Info("free search mode: DuckDuckGo (primary) + Bing (fallback)")
	}
	if cfg.Search.GroqKey != "" {
		logger.Info("groq cnpj search enabled (llama-3.3-70b, 30 req/min free)")
	}

	// Wrap search with Redis cache when available.
	if redisClient != nil {
		primarySearcher = searchprovider.NewCachedSearcher(primarySearcher, redisClient)
		if fallbackSearcher != nil {
			fallbackSearcher = searchprovider.NewCachedSearcher(fallbackSearcher, redisClient)
		}
		logger.Info("search result caching enabled (1h TTL)")
	}

	// ── Remaining providers ────────────────────────────────────────────────────
	// Instagram and WhatsApp get a ChainedSearcher so they automatically try
	// Bing when the primary (DDG / paid key) is rate-limited or returns no results.
	socialSearcher := searchprovider.NewChained(primarySearcher, fallbackSearcher)
	igProv := igprovider.New(socialSearcher)
	waProv := waprovider.New(socialSearcher)

	// Directory scrapers: AppLocal + Solutudo + GuiaMais run in parallel, results merged.
	appLocalScraper := dirprovider.NewAppLocalScraper()
	solutudoScraper := dirprovider.NewSolutudoScraper()
	guiaMaisScraper := dirprovider.NewGuiaMaisScraper()
	multiDiscoverer := dirprovider.NewMultiDiscoverer(appLocalScraper, solutudoScraper, guiaMaisScraper)
	logger.Info("directory scrapers ready", "providers", multiDiscoverer.Name())

	var fsqProv *fsqprovider.Provider
	if cfg.Providers.FoursquareKey != "" {
		fsqProv = fsqprovider.New(cfg.Providers.FoursquareKey)
		logger.Info("foursquare enabled")
	}

	// Geoapify Places API — free 90k req/month, no credit card.
	var geoapifyProv *geoapifyprovider.Provider
	if cfg.Providers.GeoapifyKey != "" {
		geoapifyProv = geoapifyprovider.New(cfg.Providers.GeoapifyKey)
		logger.Info("geoapify places enabled (90k req/month free)")
	}

	// TomTom Fuzzy Search API — free 75k req/month, no credit card.
	var tomtomProv *tomtomprovider.Provider
	if cfg.Providers.TomTomKey != "" {
		tomtomProv = tomtomprovider.New(cfg.Providers.TomTomKey)
		logger.Info("tomtom places enabled (75k req/month free)")
	}

	// Build a MultiPlacesSearcher: Geoapify → TomTom → Foursquare (order = free first).
	// Only non-nil providers are added; if all are nil the searcher returns nil and
	// the use-case skips the places phase entirely.
	var placesProv domain.PlacesSearcher
	multiPlaces := placesprovider.NewMultiSearcher(geoapifyProv, tomtomProv, fsqProv)
	if len(multiPlaces.Providers()) > 0 {
		placesProv = multiPlaces
		logger.Info("places search ready", "providers", multiPlaces.Name())
	}

	var geoProv *geoprovider.Adapter
	if cfg.Geo.LocationIQKey != "" {
		geoProv = geoprovider.NewAdapter(cfg.Geo.LocationIQKey)
		logger.Info("locationiq geocoder enabled")
	}

	// ── Use-case wiring ───────────────────────────────────────────────────────
	deps := usecase.Dependencies{
		CNAEEnricher:        cnaeProv,
		Geocoder:            geoProv,
		BusinessDiscoverer:  multiDiscoverer,
		PlacesSearcher:      placesProv,
		WebSearcher:         primarySearcher,
		WebSearcherFallback: fallbackSearcher,
		CNPJPrimary:         cnpjPrimary,
		CNPJFallback:        cnpjFallback,
		CNPJTertiary:        openCNPJA,
		CNPJQuaternary:      publicaCNPJWS,
		CNPJListSearcher:    cnpjBiz, // CNPJBiz is the only provider with a working SearchByCNAE
		// CNPJNameSearcher chains providers in order — first match wins:
		//   1. CasaDosDados — free REST API, no key, no scraping
		//   2. CNPJBiz      — cnpj.biz HTML scraper
		//   3. EmpresaQui   — empresaqui.com.br HTML scraper
		//   4. Groq         — Llama 3.3 70B; free 30 req/min (GROQ_API_KEY required)
		CNPJNameSearcher: cnpjprovider.NewMultiNameSearcher(
			cnpjprovider.NewCasaDosDados(),           // free REST API, no key
			cnpjBiz,                                  // cnpj.biz scraper
			cnpjprovider.NewEmpresaQui(),             // empresaqui.com.br scraper
			cnpjprovider.NewGroq(cfg.Search.GroqKey), // Llama 3.3 70B; nil when key absent
		),
		InstagramSearcher: igProv,
		WhatsAppSearcher:  waProv,
	}

	uc := usecase.New(deps, logger)

	// ── HTTP ──────────────────────────────────────────────────────────────────
	leadHandler := handler.NewLeadHandler(uc)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(290 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	r.Post("/leads", leadHandler.ServeHTTP)

	addr := ":" + cfg.Port
	logger.Info("server starting", "addr", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
