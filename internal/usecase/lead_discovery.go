// Package usecase orchestrates the full lead-generation pipeline.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	timeoutEnrich    = 15 * time.Second
	timeoutCNPJList  = 20 * time.Second
	timeoutCNPJFetch = 10 * time.Second
	timeoutSocial    = 15 * time.Second
	maxCNPJsToEnrich = 20
)

// Dependencies holds all provider interfaces the use-case depends on.
type Dependencies struct {
	Enricher          domain.QueryEnricher
	CNPJPrimary       domain.CNPJSearcher
	CNPJFallback      domain.CNPJSearcher
	InstagramSearcher domain.InstagramSearcher
	WhatsAppSearcher  domain.WhatsAppSearcher
}

// LeadDiscoveryUseCase orchestrates the full lead-generation pipeline.
type LeadDiscoveryUseCase struct {
	deps   Dependencies
	logger *slog.Logger
}

func New(deps Dependencies, logger *slog.Logger) *LeadDiscoveryUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeadDiscoveryUseCase{deps: deps, logger: logger}
}

// Execute runs the full discovery pipeline for the given request.
func (uc *LeadDiscoveryUseCase) Execute(ctx context.Context, req domain.SearchRequest) ([]domain.Lead, error) {
	uc.logger.Info("pipeline started", "query", req.Query, "location", req.Location)

	enrichCtx, enrichCancel := context.WithTimeout(ctx, timeoutEnrich)
	defer enrichCancel()

	searchCtx, err := uc.deps.Enricher.Enrich(enrichCtx, req.Query, req.Location)
	if err != nil {
		return nil, fmt.Errorf("usecase: enrichment failed: %w", err)
	}
	uc.logger.Info("enrichment done", "cnae", searchCtx.CNAE, "terms", searchCtx.SearchTerms)

	city, state := parseLocation(req.Location)

	var partialLeads []domain.Lead
	if req.SearchCNPJ {
		partialLeads, err = uc.fetchCNPJList(ctx, searchCtx.CNAE, city, state)
		if err != nil {
			uc.logger.Warn("cnpj list failed", "err", err)
		}
		uc.logger.Info("cnpj list done", "count", len(partialLeads))
	}

	if len(partialLeads) == 0 {
		partialLeads = []domain.Lead{{
			Name:     req.Query,
			CNAE:     searchCtx.CNAE,
			CNAEDesc: searchCtx.CNAEDescription,
			Source:   []string{"heuristic"},
		}}
	}

	enrichedLeads := uc.enrichLeads(ctx, partialLeads, req.SearchCNPJ)
	finalLeads := uc.enrichSocial(ctx, enrichedLeads, req)

	uc.logger.Info("pipeline finished", "total_leads", len(finalLeads))
	return finalLeads, nil
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
