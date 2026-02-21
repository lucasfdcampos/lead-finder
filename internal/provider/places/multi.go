// Package places provides a MultiPlacesSearcher that fans out to multiple
// PlacesSearcher providers in parallel and merges the deduplicated results.
package places

import (
	"context"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// MultiSearcher runs all registered PlacesSearcher providers in parallel and
// returns a merged, name-deduplicated list of leads.
type MultiSearcher struct {
	providers []domain.PlacesSearcher
}

// NewMultiSearcher creates a MultiSearcher from the given providers.
// Nil providers are silently ignored.
func NewMultiSearcher(providers ...domain.PlacesSearcher) *MultiSearcher {
	var valid []domain.PlacesSearcher
	for _, p := range providers {
		if p != nil {
			valid = append(valid, p)
		}
	}
	return &MultiSearcher{providers: valid}
}

func (m *MultiSearcher) Name() string {
	names := make([]string, len(m.providers))
	for i, p := range m.providers {
		names[i] = p.Name()
	}
	return "multi_places[" + strings.Join(names, "+") + "]"
}

// Providers returns the registered provider list (may be empty).
func (m *MultiSearcher) Providers() []domain.PlacesSearcher {
	return m.providers
}

// SearchPlaces queries all providers concurrently and returns merged results.
// It does not stop on the first success — all providers are always called to
// maximise coverage. Results are deduplicated by normalised name.
func (m *MultiSearcher) SearchPlaces(ctx context.Context, query, latlon string, radiusM int) ([]domain.Lead, error) {
	if len(m.providers) == 0 {
		return nil, domain.ErrNoResults
	}

	type batch struct {
		leads []domain.Lead
	}

	ch := make(chan batch, len(m.providers))
	var wg sync.WaitGroup

	for _, p := range m.providers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			leads, err := p.SearchPlaces(ctx, query, latlon, radiusM)
			if err != nil || len(leads) == 0 {
				return
			}
			ch <- batch{leads: leads}
		}()
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	seen := make(map[string]bool)
	var merged []domain.Lead

	for b := range ch {
		for _, lead := range b.leads {
			key := normalizeName(lead.Name)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, lead)
		}
	}

	if len(merged) == 0 {
		return nil, domain.ErrNoResults
	}
	return merged, nil
}

// normalizeName lowercases, strips accents and collapses spaces for dedup.
func normalizeName(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, s)
	result = strings.ToLower(result)
	result = strings.Join(strings.Fields(result), " ")
	return result
}
