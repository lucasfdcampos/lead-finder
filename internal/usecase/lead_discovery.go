// Package usecase orchestrates the full lead-generation pipeline.
//
// Pipeline (in order):
//  1. CNAEEnricher — finds CNAE code for the query via listacnae.com.br (MongoDB-cached)
//  2. Geocode     — LocationIQ converts location string → lat/lon
//  3. Discovery   — AppLocal/Solutudo directory scrapers harvest business names
//  4. Foursquare  — ONLY when UseFoursquare=true: POI search around geocoded lat/lon
//  5. Dedup       — normalizes names (lowercase + no accents + sorted word-bag)
//  6. CNPJ enrich — BrasilAPI / open.cnpja.com → partners + phone
//  7. Social      — Instagram (bio-confirmed) + WhatsApp (BrasilAPI phone or DDG)
package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	timeoutCNAE       = 45 * time.Second // allow first-run API fetch
	timeoutGeocode    = 8 * time.Second
	timeoutDiscover   = 90 * time.Second
	timeoutFoursquare = 15 * time.Second
	timeoutWebDisc    = 12 * time.Second
	timeoutCNPJList   = 20 * time.Second
	timeoutCNPJFetch  = 10 * time.Second
	timeoutCNPJByName = 12 * time.Second  // per-lead web-search budget (5 strategies: 2 full-name + 3 short-name)
	timeoutCNPJBatch  = 120 * time.Second // hard cap for the entire by-name enrichment run
	timeoutSocial     = 45 * time.Second  // more time for chained searcher fallback
	maxCNPJsToEnrich  = 500
	cnpjEnrichWorkers = 20
	maxSocialWorkers  = 20 // concurrent Instagram/WhatsApp lookups
	// circuit-breaker: allow many more misses before disabling web-search
	// (Bing fallback means a single miss is no longer a true failure)
	cnpjWebSearchCBLimit = 500
	defaultLimit         = 500
	fsqDefaultRadius     = 5000 // metres
)

// igWithHints is a narrow extension used inside the use-case to pass bio hints.
type igWithHints interface {
	domain.InstagramSearcher
	ValidateProfileWithHints(ctx context.Context, handle, city, phone string) (int, bool, error)
}

// Dependencies holds all provider interfaces the use-case depends on.
type Dependencies struct {
	CNAEEnricher        domain.CNAEEnricher       // listacnae.com.br + MongoDB cache
	Geocoder            domain.Geocoder           // LocationIQ; optional
	BusinessDiscoverer  domain.BusinessDiscoverer // AppLocal + Solutudo; optional
	PlacesSearcher      domain.PlacesSearcher     // Foursquare; optional, used when UseFoursquare=true
	WebSearcher         domain.WebSearcher        // DDG primary
	WebSearcherFallback domain.WebSearcher        // Bing fallback; optional
	CNPJPrimary         domain.CNPJSearcher       // BrasilAPI — FetchByCNPJ (primary)
	CNPJFallback        domain.CNPJSearcher       // CNPJBiz — FetchByCNPJ (1st fallback)
	CNPJTertiary        domain.CNPJSearcher       // open.cnpja.com — FetchByCNPJ (2nd fallback)
	CNPJQuaternary      domain.CNPJSearcher       // publica.cnpj.ws — FetchByCNPJ (3rd fallback)
	// Note: CNPJListSearcher and CNPJNameSearcher are always cnpjBiz regardless of fetch chain order.
	CNPJListSearcher  domain.CNPJSearcher     // CNPJBiz — SearchByCNAE (CNAE list fetch)
	CNPJNameSearcher  domain.CNPJNameSearcher // CNPJBiz direct name search
	InstagramSearcher domain.InstagramSearcher
	WhatsAppSearcher  domain.WhatsAppSearcher
}

// LeadDiscoveryUseCase orchestrates the full lead-generation pipeline.
type LeadDiscoveryUseCase struct {
	deps   Dependencies
	logger *slog.Logger
}

// New creates a new LeadDiscoveryUseCase.
func New(deps Dependencies, logger *slog.Logger) *LeadDiscoveryUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeadDiscoveryUseCase{deps: deps, logger: logger}
}

// Execute runs the full discovery pipeline for the given request.
func (uc *LeadDiscoveryUseCase) Execute(ctx context.Context, req domain.SearchRequest) ([]domain.Lead, error) {
	uc.logger.Info("pipeline started", "query", req.Query, "location", req.Location)

	// ── Phase 1: CNAE enrichment (listacnae.com.br / MongoDB cache) ───────────────
	cnaeCtx, cnaeCancel := context.WithTimeout(ctx, timeoutCNAE)
	defer cnaeCancel()

	searchCtx, err := uc.deps.CNAEEnricher.FindCNAE(cnaeCtx, req.Query)
	if err != nil {
		return nil, fmt.Errorf("usecase: CNAE enrichment failed: %w", err)
	}
	uc.logger.Info("cnae enrichment done",
		"cnae", searchCtx.CNAE,
		"use_foursquare", searchCtx.UseFoursquare,
		"terms", searchCtx.SearchTerms,
	)

	city, state := parseLocation(req.Location)

	// ── Phase 2: Geocode location ─────────────────────────────────────────────
	var latlon string
	if uc.deps.Geocoder != nil {
		geoCtx, geoCancel := context.WithTimeout(ctx, timeoutGeocode)
		latlon, err = uc.deps.Geocoder.Geocode(geoCtx, req.Location)
		geoCancel()
		if err != nil {
			uc.logger.Warn("geocode failed", "location", req.Location, "err", err)
			latlon = ""
		} else {
			uc.logger.Info("geocoded", "location", req.Location, "latlon", latlon)
		}
	}

	// ── Phase 3: Discovery — Google Maps shadow scraper ───────────────────────
	var rawNames []string
	if uc.deps.BusinessDiscoverer != nil {
		discoverCtx, discoverCancel := context.WithTimeout(ctx, timeoutDiscover)
		names, discErr := uc.deps.BusinessDiscoverer.DiscoverBusinessNames(discoverCtx, req.Query, req.Location)
		discoverCancel()
		if discErr != nil {
			uc.logger.Warn("shadow scraper returned no names", "err", discErr)
		} else {
			rawNames = names
		}
		uc.logger.Info("discovery done", "raw_names", len(rawNames))
	}

	// ── Phase 4: Web discovery fallback ───────────────────────────────────────
	// Run a general DDG/Bing web search when the shadow scraper found nothing.
	if len(rawNames) == 0 {
		webNames := uc.discoverNamesFromWebSearch(ctx, searchCtx.SearchTerms, req.Query, req.Location)
		uc.logger.Info("web discovery done", "raw_names", len(webNames))
		rawNames = webNames
	}

	// ── Phase 5: Places search (Geoapify / TomTom / Foursquare) ────────────────────
	// Always run when a PlacesSearcher is configured — free APIs (Geoapify, TomTom)
	// have generous quotas and provide excellent coverage for all business categories,
	// not only POI types like bars/restaurants.
	var fsqLeads []domain.Lead
	if uc.deps.PlacesSearcher != nil {
		fsqCtx, fsqCancel := context.WithTimeout(ctx, timeoutFoursquare)
		fsqLeads, err = uc.deps.PlacesSearcher.SearchPlaces(fsqCtx, req.Query, latlon, fsqDefaultRadius)
		fsqCancel()
		if err != nil {
			uc.logger.Warn("places search failed", "provider", uc.deps.PlacesSearcher.Name(), "err", err)
		} else {
			uc.logger.Info("places search done", "provider", uc.deps.PlacesSearcher.Name(), "results", len(fsqLeads))
			// Collect names for deduplication; full leads are merged later.
			for _, l := range fsqLeads {
				rawNames = append(rawNames, l.Name)
			}
		}
	}

	// ── Phase 6: Deduplication ────────────────────────────────────────────────────
	uniqueNames := deduplicateNames(rawNames)
	uc.logger.Info("deduplication done", "unique_names", len(uniqueNames))

	// ── Phase 7: Build partial leads + CNPJ enrichment ───────────────────────────
	// Apply limit: cap how many leads we'll enrich (CNPJ + social are expensive).
	// limit=0 means no cap — return all discovered leads.
	enrichLimit := req.Limit
	if enrichLimit <= 0 {
		enrichLimit = defaultLimit // 500 — effectively unlimited for normal use
	}

	var partialLeads []domain.Lead

	// When CNPJ search is requested, fetch the CNAE-based list in parallel with
	// building the discovery-based leads. CNAE list gives us leads that already
	// have CNPJ + partners + phone from BrasilAPI — no web-search needed.
	type cnaeResult struct {
		leads []domain.Lead
		err   error
	}
	cnaeC := make(chan cnaeResult, 1)
	if req.SearchCNPJ && searchCtx.CNAE != "" {
		go func() {
			list, listErr := uc.fetchCNPJList(ctx, searchCtx.CNAE, city, state)
			if listErr != nil {
				cnaeC <- cnaeResult{err: listErr}
				return
			}
			enriched := uc.enrichLeads(ctx, list, true)
			cnaeC <- cnaeResult{leads: enriched}
		}()
	} else {
		cnaeC <- cnaeResult{}
	}

	// Seed from Foursquare results first (they already have address/phone)
	fsqSeen := make(map[string]bool)
	for _, l := range fsqLeads {
		key := normalizeForDedup(l.Name)
		if key != "" && !fsqSeen[key] {
			fsqSeen[key] = true
			l.CNAE = searchCtx.CNAE
			l.CNAEDesc = searchCtx.CNAEDescription
			partialLeads = append(partialLeads, l)
		}
	}

	// Seed shadow-scraper names that are not already in the FSQ set
	for _, name := range uniqueNames {
		if len(partialLeads) >= enrichLimit {
			break
		}
		if fsqSeen[normalizeForDedup(name)] {
			continue
		}
		partialLeads = append(partialLeads, domain.Lead{
			Name:     name,
			CNAE:     searchCtx.CNAE,
			CNAEDesc: searchCtx.CNAEDescription,
			Address:  domain.Address{City: city, State: state},
			Source:   []string{"web-discovery"},
		})
	}

	// Collect CNAE-based leads (primary CNPJ source)
	cnaeRes := <-cnaeC
	if cnaeRes.err != nil {
		uc.logger.Warn("cnpj cnae list failed", "err", cnaeRes.err)
	}

	if len(cnaeRes.leads) > 0 {
		// Build a lookup of CNAE leads by normalized name for dedup merging
		cnaeByName := make(map[string]domain.Lead, len(cnaeRes.leads))
		for _, cl := range cnaeRes.leads {
			cnaeByName[normalizeForDedup(cl.Name)] = cl
			cnaeByName[normalizeForDedup(cl.TradeName)] = cl
		}

		// Replace or supplement discovery leads with CNAE-enriched data
		merged := make([]domain.Lead, 0, len(partialLeads)+len(cnaeRes.leads))
		mergedKeys := make(map[string]bool)

		for _, pl := range partialLeads {
			key := normalizeForDedup(pl.Name)
			if cl, found := cnaeByName[key]; found {
				// Use CNAE lead (has CNPJ) but keep discovery source tag
				cl.Source = appendUnique(cl.Source, "web-discovery")
				merged = append(merged, cl)
				mergedKeys[normalizeForDedup(cl.Name)] = true
				mergedKeys[normalizeForDedup(cl.TradeName)] = true
			} else {
				merged = append(merged, pl)
				mergedKeys[key] = true
			}
		}

		// Add CNAE leads not already in discovery results
		for _, cl := range cnaeRes.leads {
			kName := normalizeForDedup(cl.Name)
			kTrade := normalizeForDedup(cl.TradeName)
			if !mergedKeys[kName] && !mergedKeys[kTrade] {
				merged = append(merged, cl)
				mergedKeys[kName] = true
				mergedKeys[kTrade] = true
			}
		}

		partialLeads = merged
		uc.logger.Info("cnae merge done",
			"discovery_leads", len(partialLeads)-len(cnaeRes.leads),
			"cnae_leads", len(cnaeRes.leads),
			"merged_total", len(partialLeads),
		)
	} else if req.SearchCNPJ && len(partialLeads) > 0 {
		// No CNAE results — fall back to web-search-based CNPJ enrichment
		partialLeads = uc.enrichLeadsWithCNPJByName(ctx, partialLeads, city, state)
	}

	// ── Filter: discard enriched leads that don't match city or CNAE section ─────
	before := len(partialLeads)
	partialLeads = filterLeadsByLocation(partialLeads, city, searchCtx.CNAE)
	if removed := before - len(partialLeads); removed > 0 {
		uc.logger.Info("location+cnae filter", "removed", removed, "remaining", len(partialLeads))
	}

	// Apply final limit
	if len(partialLeads) > enrichLimit {
		partialLeads = partialLeads[:enrichLimit]
	}

	// Last resort: synthetic lead so the response is never empty
	if len(partialLeads) == 0 {
		partialLeads = []domain.Lead{{
			Name:     req.Query,
			CNAE:     searchCtx.CNAE,
			CNAEDesc: searchCtx.CNAEDescription,
			Address:  domain.Address{City: city, State: state},
			Source:   []string{"heuristic"},
		}}
	}

	// ── Phase 7: Social enrichment ────────────────────────────────────────────
	finalLeads := uc.enrichSocial(ctx, partialLeads, req)

	uc.logger.Info("pipeline finished", "total_leads", len(finalLeads))
	return finalLeads, nil
}

// ─── Web search with Bing fallback ───────────────────────────────────────────

func (uc *LeadDiscoveryUseCase) webSearch(ctx context.Context, query string) ([]domain.SearchResult, error) {
	results, err := uc.deps.WebSearcher.Search(ctx, query)
	if err == nil && len(results) > 0 {
		return results, nil
	}
	if uc.deps.WebSearcherFallback != nil {
		uc.logger.Debug("primary web search failed, trying bing", "query", query, "err", err)
		return uc.deps.WebSearcherFallback.Search(ctx, query)
	}
	return nil, err
}

// ─── Phase 4: discoverNamesFromWebSearch ─────────────────────────────────────

// junkTitles are site names / ad patterns that should never become leads.
var junkTitles = []string{
	"amazon", "mercadolivre", "mercado livre", "olx", "shopee", "americanas",
	"submarino", "magalu", "magazine luiza", "casas bahia", "extra", "netshoes",
	"google", "bing", "yahoo", "facebook", "instagram", "twitter",
	"youtube", "wikipedia", "tripadvisor", "ifood", "rappi", "uber eats",
	"linkedin", "yelp", "foursquare",
	"anuncio", "anúncio", "publicidade", "patrocinado",
	"compre", "veja", "encontre", "descubra", "melhores", "top ",
	"resultado", "resultados", "lista", "guia",
}

// reBusinessSep splits a title on common separators to isolate the business name part.
var reBusinessSep = regexp.MustCompile(`\s*[|\-–•·]\s*`)

// reQuotedName finds text inside double quotes in a snippet that looks like a business name.
var reQuotedName = regexp.MustCompile(`"([A-ZÀ-ÿ][^"]{3,50})"`)

// discoverNamesFromWebSearch performs a general web search for each Gemini search
// term + location and extracts candidate business names from result titles.
func (uc *LeadDiscoveryUseCase) discoverNamesFromWebSearch(
	ctx context.Context,
	terms []string,
	query, location string,
) []string {
	seen := make(map[string]bool)
	var names []string

	add := func(name string) {
		name = strings.TrimSpace(name)
		if !isPlausibleBusinessName(name) {
			return
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}

	// Build queries: use Gemini search terms, fallback to raw query.
	queries := make([]string, 0, len(terms)+1)
	for _, t := range terms {
		queries = append(queries, fmt.Sprintf(`"%s" "%s"`, t, location))
	}
	if len(queries) == 0 {
		queries = append(queries, fmt.Sprintf(`"%s" "%s"`, query, location))
	}
	// Run at most 2 queries to keep latency low.
	if len(queries) > 2 {
		queries = queries[:2]
	}

	for _, q := range queries {
		discCtx, cancel := context.WithTimeout(ctx, timeoutWebDisc)
		results, err := uc.webSearch(discCtx, q)
		cancel()
		if err != nil || len(results) == 0 {
			continue
		}
		for _, r := range results {
			// Split title on separators → first segment is usually the business name.
			parts := reBusinessSep.Split(r.Title, 2)
			for _, p := range parts {
				add(p)
			}
			// Also extract quoted names from snippets.
			for _, m := range reQuotedName.FindAllStringSubmatch(r.Snippet, 5) {
				if len(m) > 1 {
					add(m[1])
				}
			}
		}
	}
	return names
}

// isPlausibleBusinessName filters out generic/junk titles.
func isPlausibleBusinessName(name string) bool {
	if len([]rune(name)) < 4 || len([]rune(name)) > 80 {
		return false
	}
	low := strings.ToLower(name)
	for _, junk := range junkTitles {
		if strings.Contains(low, junk) {
			return false
		}
	}
	// Must start with a Unicode letter.
	runes := []rune(name)
	r := runes[0]
	if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
		(r >= '\u00C0' && r <= '\u00FF')) {
		return false
	}
	return true
}

// ─── Phase 7 helpers ──────────────────────────────────────────────────────────

func (uc *LeadDiscoveryUseCase) enrichLeadsWithCNPJByName(ctx context.Context, leads []domain.Lead, city, state string) []domain.Lead {
	if len(leads) > maxCNPJsToEnrich {
		leads = leads[:maxCNPJsToEnrich]
	}

	uc.logger.Info("cnpj enrichment started", "leads", len(leads), "workers", cnpjEnrichWorkers)

	// Hard total budget: no matter how many leads or how slow each provider is,
	// the entire by-name enrichment phase must complete within timeoutCNPJBatch.
	batchCtx, batchCancel := context.WithTimeout(ctx, timeoutCNPJBatch)
	defer batchCancel()

	result := make([]domain.Lead, len(leads))
	sem := make(chan struct{}, cnpjEnrichWorkers)
	var wg sync.WaitGroup

	// circuit-breaker: counts consecutive misses across all goroutines.
	// When it reaches cnpjWebSearchCBLimit, workers skip web-search strategies.
	var consecutiveMisses int64

	for i, lead := range leads {
		// Pre-copy the lead into result so cancellation leaves a valid (if unenriched) lead.
		result[i] = lead
		wg.Add(1)
		go func(idx int, l domain.Lead) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-batchCtx.Done():
				return // budget exhausted — keep original lead
			}
			defer func() { <-sem }()

			// Circuit-breaker: skip web search once too many consecutive misses.
			skipWeb := atomic.LoadInt64(&consecutiveMisses) >= cnpjWebSearchCBLimit
			got := uc.enrichSingleLeadWithCNPJ(batchCtx, l, city, state, skipWeb)
			result[idx] = got
			if got.CNPJ != "" {
				atomic.StoreInt64(&consecutiveMisses, 0)
			} else {
				atomic.AddInt64(&consecutiveMisses, 1)
			}
		}(i, lead)
	}
	wg.Wait()

	enriched := 0
	for _, l := range result {
		if l.CNPJ != "" {
			enriched++
		}
	}
	uc.logger.Info("cnpj enrichment done", "leads", len(result), "with_cnpj", enriched)
	return result
}

func (uc *LeadDiscoveryUseCase) enrichSingleLeadWithCNPJ(ctx context.Context, lead domain.Lead, city, state string, skipWeb bool) domain.Lead {
	cnpj := uc.findCNPJByName(ctx, lead.Name, city, state, skipWeb)
	if cnpj == "" {
		return lead
	}

	fullLead, err := uc.fetchSingleCNPJ(ctx, cnpj)
	if err != nil {
		uc.logger.Warn("cnpj fetch failed for discovered name",
			"name", lead.Name, "cnpj", cnpj, "err", err)
		lead.CNPJ = sanitizeCNPJDigits(cnpj)
		return lead
	}

	// City-match guard: reject the CNPJ if it belongs to a different city.
	// A web-search false positive would overwrite the lead's address and cause
	// it to be removed by the location filter later.
	if fullLead.Address.City != "" && city != "" &&
		!strings.EqualFold(normalizeCity(fullLead.Address.City), normalizeCity(city)) {
		uc.logger.Debug("cnpj city mismatch, rejecting",
			"name", lead.Name, "cnpj", cnpj,
			"expected", city, "got", fullLead.Address.City)
		return lead
	}

	fullLead.Source = appendUnique(fullLead.Source, "googlemaps-shadow")
	return *fullLead
}

var reCNPJInText = regexp.MustCompile(`[0-9]{2}\.[0-9]{3}\.[0-9]{3}/[0-9]{4}-[0-9]{2}`)

// reCNPJInURL matches 14-digit CNPJ strings that appear in URL paths (e.g. cnpj.biz/12345678000190).
var reCNPJInURL = regexp.MustCompile(`/([0-9]{14})(?:[/?#]|$)`)

func (uc *LeadDiscoveryUseCase) findCNPJByName(ctx context.Context, name, city, state string, skipWeb bool) string {
	findCtx, cancel := context.WithTimeout(ctx, timeoutCNPJByName)
	defer cancel()

	// Strategy 0: Direct CNPJBiz name search (no web-search rate limits).
	if uc.deps.CNPJNameSearcher != nil {
		if cnpj, err := uc.deps.CNPJNameSearcher.SearchByName(findCtx, name, city); err == nil && cnpj != "" {
			uc.logger.Debug("cnpj found via cnpjbiz direct search", "name", name, "cnpj", cnpj)
			return cnpj
		}
	}

	// Circuit-breaker: skip all web-search strategies when too many consecutive misses.
	if skipWeb {
		return ""
	}

	// Strategy 0b: Full quoted name — "<name>" "<city>" CNPJ.
	// Most precise signal; works well when the registered name is widely indexed.
	queryFull := fmt.Sprintf(`"%s" "%s" CNPJ`, name, city)
	if results, err := uc.webSearch(findCtx, queryFull); err == nil {
		for _, r := range results {
			text := r.Snippet + " " + r.URL + " " + r.Title
			if m := reCNPJInText.FindString(text); m != "" {
				uc.logger.Debug("cnpj found via full-name web search", "name", name, "cnpj", m)
				return m
			}
		}
	}

	// Strategy 0c: Full quoted name on cnpj.biz — site:cnpj.biz "<name>" "<city>".
	queryCNPJBizFull := fmt.Sprintf(`site:cnpj.biz "%s" "%s"`, name, city)
	if results, err := uc.webSearch(findCtx, queryCNPJBizFull); err == nil {
		for _, r := range results {
			if m := reCNPJInURL.FindStringSubmatch(r.URL); len(m) > 1 {
				uc.logger.Debug("cnpj found via full-name cnpj.biz url", "name", name, "cnpj", m[1])
				return m[1]
			}
			text := r.Snippet + " " + r.Title
			if m := reCNPJInText.FindString(text); m != "" {
				uc.logger.Debug("cnpj found via full-name cnpj.biz snippet", "name", name, "cnpj", m)
				return m
			}
		}
	}

	// Normalize name: strip legal suffixes + stop words for shorter queries.
	short := shortQueryName(name)

	// Strategy 1: Short name — formatted CNPJ in snippets/titles.
	query := fmt.Sprintf(`"%s" CNPJ %s %s`, short, city, state)
	if results, err := uc.webSearch(findCtx, query); err == nil {
		for _, r := range results {
			text := r.Snippet + " " + r.URL + " " + r.Title
			if m := reCNPJInText.FindString(text); m != "" {
				uc.logger.Debug("cnpj found via web search (snippet)", "name", name, "cnpj", m)
				return m
			}
		}
	}

	// Strategy 2: site:cnpj.biz with short name — CNPJ in URL path.
	query2 := fmt.Sprintf(`site:cnpj.biz "%s" %s`, short, city)
	if results, err := uc.webSearch(findCtx, query2); err == nil {
		for _, r := range results {
			if m := reCNPJInURL.FindStringSubmatch(r.URL); len(m) > 1 {
				uc.logger.Debug("cnpj found via cnpj.biz url", "name", name, "cnpj", m[1])
				return m[1]
			}
			text := r.Snippet + " " + r.Title
			if m := reCNPJInText.FindString(text); m != "" {
				uc.logger.Debug("cnpj found via cnpj.biz snippet", "name", name, "cnpj", m)
				return m
			}
		}
	}

	// Strategy 3: broad short-name search — casadosdados, consultacnpj, etc.
	query3 := fmt.Sprintf(`"%s" "%s" %s CNPJ`, short, city, state)
	if results, err := uc.webSearch(findCtx, query3); err == nil {
		for _, r := range results {
			text := r.Snippet + " " + r.URL + " " + r.Title
			if m := reCNPJInText.FindString(text); m != "" {
				uc.logger.Debug("cnpj found via broad web search", "name", name, "cnpj", m)
				return m
			}
		}
	}

	uc.logger.Debug("cnpj not found", "name", name)
	return ""
}

func sanitizeCNPJDigits(cnpj string) string {
	return regexp.MustCompile(`\D`).ReplaceAllString(cnpj, "")
}

// shortQueryName strips legal suffixes, Brazilian stop words, and punctuation from a
// business name, returning a shorter string that improves web-search query quality.
func shortQueryName(name string) string {
	s := strings.ToLower(name)
	// Remove legal suffixes.
	for _, pat := range []string{
		`\bltda\.?\b`, `\beireli\.?\b`, `\bepp\.?\b`, `\bs/?a\.?\b`,
		`\bme\.?\b`, `\bcia\.?\b`, `\bcompanhia\b`, `\bgrupo\b`,
	} {
		re := regexp.MustCompile(`(?i)` + pat)
		s = re.ReplaceAllString(s, " ")
	}
	// Collapse punctuation and whitespace.
	s = regexp.MustCompile(`[\s\-/,&.]+`).ReplaceAllString(strings.TrimSpace(s), " ")
	words := strings.Fields(s)
	// Remove common Portuguese prepositions/articles.
	stop := map[string]bool{
		"de": true, "da": true, "do": true, "dos": true, "das": true,
		"e": true, "em": true, "para": true,
	}
	var keep []string
	for _, w := range words {
		if !stop[w] && len(w) > 1 {
			keep = append(keep, w)
		}
	}
	if len(keep) == 0 {
		return strings.TrimSpace(s)
	}
	// Limit to first 4 significant words to keep queries focused.
	if len(keep) > 4 {
		keep = keep[:4]
	}
	return strings.Join(keep, " ")
}

func (uc *LeadDiscoveryUseCase) fetchCNPJList(ctx context.Context, cnae, city, state string) ([]domain.Lead, error) {
	listCtx, cancel := context.WithTimeout(ctx, timeoutCNPJList)
	defer cancel()

	// CNPJBiz is the only provider with a working CNAE search endpoint.
	// Try it first, then fall through to the generic primary/fallback.
	if uc.deps.CNPJListSearcher != nil {
		leads, err := uc.deps.CNPJListSearcher.SearchByCNAE(listCtx, cnae, city, state)
		if err == nil && len(leads) > 0 {
			return leads, nil
		}
		if err != nil && !errors.Is(err, domain.ErrNoResults) {
			uc.logger.Warn("cnpjbiz cnae search failed", "err", err)
		}
	}

	leads, err := uc.deps.CNPJPrimary.SearchByCNAE(listCtx, cnae, city, state)
	if err == nil && len(leads) > 0 {
		return leads, nil
	}
	if err != nil && !errors.Is(err, domain.ErrNoResults) {
		uc.logger.Warn("brasilapi cnae search failed", "err", err)
	}

	leads, err = uc.deps.CNPJFallback.SearchByCNAE(listCtx, cnae, city, state)
	if err != nil {
		return nil, fmt.Errorf("cnpj list: all providers failed: %w", err)
	}
	return leads, nil
}

func (uc *LeadDiscoveryUseCase) enrichLeads(ctx context.Context, partials []domain.Lead, doCNPJ bool) []domain.Lead {
	if !doCNPJ {
		return partials
	}
	cap := len(partials)
	if cap > maxCNPJsToEnrich {
		cap = maxCNPJsToEnrich
	}
	enriched := make([]domain.Lead, 0, cap)
	for i, partial := range partials {
		if i >= maxCNPJsToEnrich {
			break
		}
		if partial.CNPJ == "" {
			enriched = append(enriched, partial)
			continue
		}
		lead, err := uc.fetchSingleCNPJ(ctx, partial.CNPJ)
		if err != nil {
			uc.logger.Warn("single cnpj fetch failed", "cnpj", partial.CNPJ, "err", err)
			enriched = append(enriched, partial)
			continue
		}
		enriched = append(enriched, *lead)
	}
	return enriched
}

func (uc *LeadDiscoveryUseCase) fetchSingleCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, timeoutCNPJFetch)
	defer cancel()

	providers := []domain.CNPJSearcher{
		uc.deps.CNPJPrimary,
		uc.deps.CNPJFallback,
		uc.deps.CNPJTertiary,
		uc.deps.CNPJQuaternary,
	}
	var lastErr error
	for _, p := range providers {
		if p == nil {
			continue
		}
		lead, err := p.FetchByCNPJ(fetchCtx, cnpj)
		if err == nil {
			return lead, nil
		}
		lastErr = err
		if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrRateLimited) {
			uc.logger.Warn("cnpj provider failed, trying next",
				"provider", p.Name(), "cnpj", cnpj, "err", err)
		}
	}
	return nil, fmt.Errorf("all providers failed for CNPJ %s: %w", cnpj, lastErr)
}

// ─── Phase 7: Social enrichment ───────────────────────────────────────────────

func (uc *LeadDiscoveryUseCase) enrichSocial(ctx context.Context, leads []domain.Lead, req domain.SearchRequest) []domain.Lead {
	uc.logger.Info("social enrichment started",
		"leads", len(leads),
		"search_instagram", req.SearchInstagram,
		"search_whatsapp", req.SearchWhatsApp,
	)
	result := make([]domain.Lead, len(leads))
	sem := make(chan struct{}, maxSocialWorkers)
	var wg sync.WaitGroup
	for i, lead := range leads {
		wg.Add(1)
		go func(idx int, l domain.Lead) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result[idx] = uc.enrichSingleLeadSocial(ctx, l, req)
		}(i, lead)
	}
	wg.Wait()

	ig, wa := 0, 0
	for _, l := range result {
		if l.Instagram != "" {
			ig++
		}
		if l.WhatsApp != "" {
			wa++
		}
	}
	uc.logger.Info("social enrichment done", "with_instagram", ig, "with_whatsapp", wa)
	return result
}

func (uc *LeadDiscoveryUseCase) enrichSingleLeadSocial(ctx context.Context, lead domain.Lead, req domain.SearchRequest) domain.Lead {
	socialCtx, cancel := context.WithTimeout(ctx, timeoutSocial)
	defer cancel()

	displayName := lead.TradeName
	if displayName == "" {
		displayName = lead.Name
	}

	type igResult struct {
		handle    string
		followers int
	}

	igCh := make(chan igResult, 1)
	waCh := make(chan string, 1)

	if req.SearchInstagram && uc.deps.InstagramSearcher != nil {
		go func() {
			handle, err := uc.deps.InstagramSearcher.FindHandle(socialCtx, displayName, req.Location)
			if err != nil {
				uc.logger.Debug("instagram not found", "business", displayName, "err", err)
				igCh <- igResult{}
				return
			}
			// Use bio hints when available for higher-confidence validation
			var followers int
			var ok bool
			if igH, canHint := uc.deps.InstagramSearcher.(igWithHints); canHint {
				followers, ok, err = igH.ValidateProfileWithHints(socialCtx, handle, lead.Address.City, lead.Phone)
			} else {
				followers, ok, err = uc.deps.InstagramSearcher.ValidateProfile(socialCtx, handle)
			}
			if err != nil || !ok {
				uc.logger.Debug("instagram validate failed", "handle", handle, "err", err)
				igCh <- igResult{}
				return
			}
			igCh <- igResult{handle: handle, followers: followers}
		}()
	} else {
		igCh <- igResult{}
	}

	if req.SearchWhatsApp && uc.deps.WhatsAppSearcher != nil {
		go func() {
			// Prefer an already-confirmed WhatsApp/mobile number from CNPJ enrichment.
			// Do NOT forward a raw CNPJ phone (lead.Phone) — it may be a landline.
			if lead.WhatsApp != "" {
				waCh <- lead.WhatsApp
				return
			}
			num, err := uc.deps.WhatsAppSearcher.FindNumber(socialCtx, displayName, req.Location)
			if err != nil {
				uc.logger.Debug("whatsapp not found", "business", displayName, "err", err)
				waCh <- ""
				return
			}
			waCh <- num
		}()
	} else {
		waCh <- ""
	}

	igRes := <-igCh
	waNum := <-waCh

	if igRes.handle != "" {
		lead.Instagram = igRes.handle
		lead.Followers = igRes.followers
		lead.Source = appendUnique(lead.Source, "instagram")
	}
	if waNum != "" {
		lead.WhatsApp = waNum
		lead.Source = appendUnique(lead.Source, "whatsapp")
	}

	return lead
}

// ─── Deduplication ────────────────────────────────────────────────────────────

var (
	reLegalDedup   = regexp.MustCompile(`(?i)\b(ltda|eireli|epp|me|s/?a\.?|cia|companhia|grupo)\b\.?`)
	reNonWordDedup = regexp.MustCompile(`[^a-z0-9]+`)
	reAccentDedup  = strings.NewReplacer(
		"a\u0301", "a", "a\u0300", "a", "\u00e1", "a", "\u00e0", "a", "\u00e2", "a", "\u00e3", "a",
		"\u00e9", "e", "\u00ea", "e",
		"\u00ed", "i", "\u00ee", "i",
		"\u00f3", "o", "\u00f4", "o", "\u00f5", "o",
		"\u00fa", "u", "\u00fb", "u",
		"\u00e7", "c",
	)
)

func deduplicateNames(names []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, name := range names {
		key := normalizeForDedup(name)
		if key == "" {
			continue
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

func normalizeForDedup(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = reAccentDedup.Replace(s)
	s = reLegalDedup.ReplaceAllString(s, " ")
	words := reNonWordDedup.Split(s, -1)
	var clean []string
	for _, w := range words {
		if len(w) > 1 {
			clean = append(clean, w)
		}
	}
	sort.Strings(clean)
	return strings.Join(clean, "|")
}

// ─── Location + CNAE filter ──────────────────────────────────────────────────

// filterLeadsByLocation removes leads that carry CNPJ-verified address data
// but belong to a city or CNAE section (first 2 digits) different from the
// one requested. Leads without CNPJ data are always kept — they haven't been
// verified yet and may still be valid after future enrichment.
func filterLeadsByLocation(leads []domain.Lead, city, expectedCNAE string) []domain.Lead {
	normCity := normalizeCity(city)
	expectedSection := cnaeSectionDigits(expectedCNAE)

	out := leads[:0] // reuse backing array
	for _, l := range leads {
		// Not yet enriched — keep.
		if l.CNPJ == "" || l.Address.City == "" {
			out = append(out, l)
			continue
		}
		// City must match (case + accent insensitive).
		if normalizeCity(l.Address.City) != normCity {
			continue
		}
		// CNAE section must match when both sides are known.
		if expectedSection != "" && l.CNAE != "" {
			if cnaeSectionDigits(l.CNAE) != expectedSection {
				continue
			}
		}
		out = append(out, l)
	}
	return out
}

// normalizeCity lowercases and strips accents from a city name so that
// "ARAPONGAS" and "Arapongas" compare equal.
func normalizeCity(city string) string {
	s := strings.ToLower(strings.TrimSpace(city))
	return reAccentDedup.Replace(s)
}

// cnaeSectionDigits extracts the first 2 numeric digits from any CNAE format.
// Examples: "47.81-4" → "47", "4781" → "47", "5611-2" → "56".
func cnaeSectionDigits(cnae string) string {
	var buf [2]byte
	n := 0
	for i := 0; i < len(cnae) && n < 2; i++ {
		if cnae[i] >= '0' && cnae[i] <= '9' {
			buf[n] = cnae[i]
			n++
		}
	}
	return string(buf[:n])
}

// ─── Utilities ────────────────────────────────────────────────────────────────

func parseLocation(location string) (city, state string) {
	parts := strings.SplitN(location, "-", 2)
	city = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		state = strings.TrimSpace(parts[1])
	}
	return
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}
