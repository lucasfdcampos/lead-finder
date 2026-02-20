// Package usecase orchestrates the full lead-generation pipeline.
//
// Pipeline (in order):
//   1. Gemini      — enriches the query: returns CNAE, UseFoursquare flag, search terms
//   2. Discovery   — GoogleMapsShadowScraper pages through DDG results to harvest
//                    real business names from Google Maps organic listings
//   3. Dedup       — normalizes names (lowercase + no accents + sorted word-bag)
//                    to merge duplicates like "Padaria Silva" / "Silva Padaria Ltda"
//   4. CNPJ enrich — for each unique name: DDG search → extract CNPJ →
//                    BrasilAPI (JSON, fast) → CNPJBiz (scraper, fallback)
//   5. Social      — Instagram (heuristics + scraper) + WhatsApp (regex + Gemini)
//                    run concurrently per lead
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
	timeoutEnrich       = 15 * time.Second
	timeoutDiscover     = 45 * time.Second // up to 3 paginated DDG pages
	timeoutCNPJList     = 20 * time.Second
	timeoutCNPJFetch    = 10 * time.Second
	timeoutCNPJByName   = 10 * time.Second // DDG search to find CNPJ for a name
	timeoutSocial       = 15 * time.Second
	maxCNPJsToEnrich   = 20
	cnpjEnrichWorkers  = 5 // max concurrent CNPJ-by-name DDG searches
)

// Dependencies holds all provider interfaces the use-case depends on.
// BusinessDiscoverer and WebSearcher are optional — set to nil to skip those phases.
type Dependencies struct {
	Enricher           domain.QueryEnricher
	BusinessDiscoverer domain.BusinessDiscoverer // GoogleMapsShadowScraper; optional
	WebSearcher        domain.WebSearcher        // DDG; used for CNPJ-by-name lookups
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

	// ── Phase 2: Discovery — Google Maps shadow scraper ───────────────────────
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

	// ── Phase 3: Deduplication ────────────────────────────────────────────────
	uniqueNames := deduplicateNames(rawNames)
	uc.logger.Info("deduplication done", "unique_names", len(uniqueNames))

	// ── Phase 4: Build partial leads + CNPJ enrichment ───────────────────────
	var partialLeads []domain.Lead

	if len(uniqueNames) > 0 {
		// Seed one lead per discovered name.
		for _, name := range uniqueNames {
			partialLeads = append(partialLeads, domain.Lead{
				Name:     name,
				CNAE:     searchCtx.CNAE,
				CNAEDesc: searchCtx.CNAEDescription,
				Address:  domain.Address{City: city, State: state},
				Source:   []string{"googlemaps-shadow"},
			})
		}

		// Enrich each name-lead with its CNPJ (concurrent DDG lookups).
		if req.SearchCNPJ {
			partialLeads = uc.enrichLeadsWithCNPJByName(ctx, partialLeads, city, state)
		}
	}

	// Fallback: CNAE-based CNPJ list when shadow scraper found nothing.
	if len(partialLeads) == 0 && req.SearchCNPJ {
		cnpjList, listErr := uc.fetchCNPJList(ctx, searchCtx.CNAE, city, state)
		if listErr != nil {
			uc.logger.Warn("cnpj fallback list failed", "err", listErr)
		} else {
			partialLeads = uc.enrichLeads(ctx, cnpjList, true)
		}
	}

	// Last resort: synthetic lead so the response is never empty.
	if len(partialLeads) == 0 {
		partialLeads = []domain.Lead{{
			Name:     req.Query,
			CNAE:     searchCtx.CNAE,
			CNAEDesc: searchCtx.CNAEDescription,
			Address:  domain.Address{City: city, State: state},
			Source:   []string{"heuristic"},
		}}
	}

	// ── Phase 5: Social enrichment (Instagram + WhatsApp) ────────────────────
	finalLeads := uc.enrichSocial(ctx, partialLeads, req)

	uc.logger.Info("pipeline finished", "total_leads", len(finalLeads))
	return finalLeads, nil
}

// ─── Phase 4 helpers ──────────────────────────────────────────────────────────

// enrichLeadsWithCNPJByName fans out CNPJ-by-name lookups concurrently
// (capped at cnpjEnrichWorkers goroutines) and merges the results.
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

// enrichSingleLeadWithCNPJ searches DDG for a CNPJ, then fetches full company data.
func (uc *LeadDiscoveryUseCase) enrichSingleLeadWithCNPJ(ctx context.Context, lead domain.Lead, city, state string) domain.Lead {
	cnpj := uc.findCNPJByName(ctx, lead.Name, city, state)
	if cnpj == "" {
		return lead // keep name-only lead
	}

	fullLead, err := uc.fetchSingleCNPJ(ctx, cnpj)
	if err != nil {
		uc.logger.Warn("cnpj fetch failed for discovered name",
			"name", lead.Name, "cnpj", cnpj, "err", err)
		// Partial enrichment: at least store the raw CNPJ.
		lead.CNPJ = sanitizeCNPJDigits(cnpj)
		return lead
	}

	// Preserve the Google Maps discovery source alongside the CNPJ source.
	fullLead.Source = appendUnique(fullLead.Source, "googlemaps-shadow")
	return *fullLead
}

var reCNPJInText = regexp.MustCompile(`[0-9]{2}\.[0-9]{3}\.[0-9]{3}/[0-9]{4}-[0-9]{2}`)

// findCNPJByName uses DuckDuckGo to search for a CNPJ associated with a business name.
func (uc *LeadDiscoveryUseCase) findCNPJByName(ctx context.Context, name, city, state string) string {
	if uc.deps.WebSearcher == nil {
		return ""
	}

	findCtx, cancel := context.WithTimeout(ctx, timeoutCNPJByName)
	defer cancel()

	query := fmt.Sprintf(`"%s" CNPJ %s %s`, name, city, state)
	results, err := uc.deps.WebSearcher.Search(findCtx, query)
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

// ─── Phase 2 helpers (CNAE-based fallback) ────────────────────────────────────

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

// ─── Phase 5: Social enrichment ───────────────────────────────────────────────

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
			followers, _, err := uc.deps.InstagramSearcher.ValidateProfile(socialCtx, handle)
			if err != nil {
				uc.logger.Debug("instagram validate failed", "handle", handle, "err", err)
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
	// reLegalDedup removes common Brazilian legal-form tokens from a name before
	// comparing, so "Padaria Silva Ltda" and "Padaria Silva" collapse to the same key.
	reLegalDedup = regexp.MustCompile(`(?i)\b(ltda|eireli|epp|me|s/?a\.?|cia|companhia|grupo)\b\.?`)

	// reNonWordDedup splits a normalized string into word tokens.
	reNonWordDedup = regexp.MustCompile(`[^a-z0-9]+`)

	// reAccentDedup replaces Portuguese-accented characters with their ASCII equivalents.
	reAccentDedup = strings.NewReplacer(
		"a\u0301", "a", "a\u0300", "a", "\u00e1", "a", "\u00e0", "a", "\u00e2", "a", "\u00e3", "a",
		"\u00e9", "e", "\u00ea", "e",
		"\u00ed", "i", "\u00ee", "i",
		"\u00f3", "o", "\u00f4", "o", "\u00f5", "o",
		"\u00fa", "u", "\u00fb", "u",
		"\u00e7", "c",
	)
)

// deduplicateNames returns a new slice with duplicate business names removed.
// Two names are considered duplicates when their normalizeForDedup keys match.
// Example: "Padaria Silva" and "Silva Padaria Ltda" → same key → first one wins.
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

// normalizeForDedup produces a canonical sort-key for a business name:
//  1. Lowercase + remove accents
//  2. Strip legal suffixes (Ltda, S/A, Eireli, …)
//  3. Tokenize → sort words alphabetically → join with "|"
//
// Sorting the words makes "Padaria Silva" and "Silva Padaria" identical.
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

// parseLocation splits "Arapongas-PR" into ("Arapongas", "PR").
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
