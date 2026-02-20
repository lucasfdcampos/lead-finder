// Package usecase orchestrates the full lead-generation pipeline.
//
// Pipeline (in order):
//  1. Gemini      — enriches query: returns CNAE, UseFoursquare flag, search terms
//  2. Geocode     — LocationIQ converts location string → lat/lon
//  3. Discovery   — GoogleMapsShadowScraper (DDG→Google Maps) harvests business names
//  4. Foursquare  — ONLY when UseFoursquare=true: POI search around geocoded lat/lon
//  5. Dedup       — normalizes names (lowercase + no accents + sorted word-bag)
//  6. CNPJ enrich — DDG/Bing → extract CNPJ → BrasilAPI → CNPJBiz
//  7. Social      — Instagram (bio-confirmed) + WhatsApp, concurrently per lead
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
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	timeoutEnrich      = 15 * time.Second
	timeoutGeocode     = 8 * time.Second
	timeoutDiscover    = 45 * time.Second
	timeoutFoursquare  = 15 * time.Second
	timeoutCNPJList    = 20 * time.Second
	timeoutCNPJFetch   = 10 * time.Second
	timeoutCNPJByName  = 10 * time.Second
	timeoutSocial      = 15 * time.Second
	maxCNPJsToEnrich   = 20
	cnpjEnrichWorkers  = 5
	fsqDefaultRadius   = 5000 // metres
)

// igWithHints is a narrow extension used inside the use-case to pass bio hints.
type igWithHints interface {
	domain.InstagramSearcher
	ValidateProfileWithHints(ctx context.Context, handle, city, phone string) (int, bool, error)
}

// Dependencies holds all provider interfaces the use-case depends on.
type Dependencies struct {
	Enricher           domain.QueryEnricher
	Geocoder           domain.Geocoder           // LocationIQ; optional
	BusinessDiscoverer domain.BusinessDiscoverer  // GoogleMapsShadowScraper; optional
	PlacesSearcher     domain.PlacesSearcher     // Foursquare; optional, used when UseFoursquare=true
	WebSearcher        domain.WebSearcher        // DDG primary
	WebSearcherFallback domain.WebSearcher       // Bing fallback; optional
	CNPJPrimary        domain.CNPJSearcher       // BrasilAPI
	CNPJFallback       domain.CNPJSearcher       // CNPJBiz
	InstagramSearcher  domain.InstagramSearcher
	WhatsAppSearcher   domain.WhatsAppSearcher
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

	// ── Phase 1: Gemini enrichment ────────────────────────────────────────────
	enrichCtx, enrichCancel := context.WithTimeout(ctx, timeoutEnrich)
	defer enrichCancel()

	searchCtx, err := uc.deps.Enricher.Enrich(enrichCtx, req.Query, req.Location)
	if err != nil {
		return nil, fmt.Errorf("usecase: enrichment failed: %w", err)
	}
	uc.logger.Info("enrichment done",
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

	// ── Phase 4: Foursquare (POI queries only) ────────────────────────────────
	var fsqLeads []domain.Lead
	if searchCtx.UseFoursquare && uc.deps.PlacesSearcher != nil {
		fsqCtx, fsqCancel := context.WithTimeout(ctx, timeoutFoursquare)
		fsqLeads, err = uc.deps.PlacesSearcher.SearchPlaces(fsqCtx, req.Query, latlon, fsqDefaultRadius)
		fsqCancel()
		if err != nil {
			uc.logger.Warn("foursquare search failed", "err", err)
		} else {
			uc.logger.Info("foursquare done", "results", len(fsqLeads))
			// Collect FSQ names for deduplication
			for _, l := range fsqLeads {
				rawNames = append(rawNames, l.Name)
			}
		}
	}

	// ── Phase 5: Deduplication ────────────────────────────────────────────────
	uniqueNames := deduplicateNames(rawNames)
	uc.logger.Info("deduplication done", "unique_names", len(uniqueNames))

	// ── Phase 6: Build partial leads + CNPJ enrichment ───────────────────────
	var partialLeads []domain.Lead

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
		if fsqSeen[normalizeForDedup(name)] {
			continue
		}
		partialLeads = append(partialLeads, domain.Lead{
			Name:     name,
			CNAE:     searchCtx.CNAE,
			CNAEDesc: searchCtx.CNAEDescription,
			Address:  domain.Address{City: city, State: state},
			Source:   []string{"googlemaps-shadow"},
		})
	}

	if req.SearchCNPJ && len(partialLeads) > 0 {
		partialLeads = uc.enrichLeadsWithCNPJByName(ctx, partialLeads, city, state)
	}

	// Fallback: CNAE-based CNPJ list when no leads found yet
	if len(partialLeads) == 0 && req.SearchCNPJ {
		cnpjList, listErr := uc.fetchCNPJList(ctx, searchCtx.CNAE, city, state)
		if listErr != nil {
			uc.logger.Warn("cnpj fallback list failed", "err", listErr)
		} else {
			partialLeads = uc.enrichLeads(ctx, cnpjList, true)
		}
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

// ─── Phase 6 helpers ──────────────────────────────────────────────────────────

func (uc *LeadDiscoveryUseCase) enrichLeadsWithCNPJByName(ctx context.Context, leads []domain.Lead, city, state string) []domain.Lead {
	if len(leads) > maxCNPJsToEnrich {
		leads = leads[:maxCNPJsToEnrich]
	}

	result := make([]domain.Lead, len(leads))
	sem := make(chan struct{}, cnpjEnrichWorkers)
	var wg sync.WaitGroup

	for i, lead := range leads {
		wg.Add(1)
		go func(idx int, l domain.Lead) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result[idx] = uc.enrichSingleLeadWithCNPJ(ctx, l, city, state)
		}(i, lead)
	}
	wg.Wait()
	return result
}

func (uc *LeadDiscoveryUseCase) enrichSingleLeadWithCNPJ(ctx context.Context, lead domain.Lead, city, state string) domain.Lead {
	cnpj := uc.findCNPJByName(ctx, lead.Name, city, state)
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
	fullLead.Source = appendUnique(fullLead.Source, "googlemaps-shadow")
	return *fullLead
}

var reCNPJInText = regexp.MustCompile(`[0-9]{2}\.[0-9]{3}\.[0-9]{3}/[0-9]{4}-[0-9]{2}`)

func (uc *LeadDiscoveryUseCase) findCNPJByName(ctx context.Context, name, city, state string) string {
	findCtx, cancel := context.WithTimeout(ctx, timeoutCNPJByName)
	defer cancel()

	query := fmt.Sprintf(`"%s" CNPJ %s %s`, name, city, state)
	results, err := uc.webSearch(findCtx, query)
	if err != nil {
		return ""
	}

	for _, r := range results {
		text := r.Snippet + " " + r.URL + " " + r.Title
		if m := reCNPJInText.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

func sanitizeCNPJDigits(cnpj string) string {
	return regexp.MustCompile(`\D`).ReplaceAllString(cnpj, "")
}

func (uc *LeadDiscoveryUseCase) fetchCNPJList(ctx context.Context, cnae, city, state string) ([]domain.Lead, error) {
	listCtx, cancel := context.WithTimeout(ctx, timeoutCNPJList)
	defer cancel()

	leads, err := uc.deps.CNPJPrimary.SearchByCNAE(listCtx, cnae, city, state)
	if err == nil && len(leads) > 0 {
		return leads, nil
	}
	if err != nil && !errors.Is(err, domain.ErrNoResults) {
		uc.logger.Warn("brasilapi cnae search failed", "err", err)
	}

	leads, err = uc.deps.CNPJFallback.SearchByCNAE(listCtx, cnae, city, state)
	if err != nil {
		return nil, fmt.Errorf("cnpj list: both providers failed: %w", err)
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

	lead, err := uc.deps.CNPJPrimary.FetchByCNPJ(fetchCtx, cnpj)
	if err == nil {
		return lead, nil
	}
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrRateLimited) {
		uc.logger.Warn("brasilapi fetch failed, trying cnpjbiz", "cnpj", cnpj, "err", err)
	}
	lead, err = uc.deps.CNPJFallback.FetchByCNPJ(fetchCtx, cnpj)
	if err != nil {
		return nil, fmt.Errorf("both providers failed for CNPJ %s: %w", cnpj, err)
	}
	return lead, nil
}

// ─── Phase 7: Social enrichment ───────────────────────────────────────────────

func (uc *LeadDiscoveryUseCase) enrichSocial(ctx context.Context, leads []domain.Lead, req domain.SearchRequest) []domain.Lead {
	result := make([]domain.Lead, len(leads))
	var wg sync.WaitGroup
	for i, lead := range leads {
		wg.Add(1)
		go func(idx int, l domain.Lead) {
			defer wg.Done()
			result[idx] = uc.enrichSingleLeadSocial(ctx, l, req)
		}(i, lead)
	}
	wg.Wait()
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
			if lead.Phone != "" {
				waCh <- lead.Phone
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
	reLegalDedup = regexp.MustCompile(`(?i)\b(ltda|eireli|epp|me|s/?a\.?|cia|companhia|grupo)\b\.?`)
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
