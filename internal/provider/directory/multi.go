package directory

import (
	"context"
	"strings"
	"sync"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// MultiDiscoverer runs multiple BusinessDiscoverer implementations in parallel
// and returns the deduplicated union of all results.
//
// If all discoverers fail or return no results, domain.ErrNoResults is returned.
type MultiDiscoverer struct {
	discoverers []domain.BusinessDiscoverer
}

// NewMultiDiscoverer creates a MultiDiscoverer that aggregates the given
// discoverers. Nil entries are silently ignored.
func NewMultiDiscoverer(discoverers ...domain.BusinessDiscoverer) *MultiDiscoverer {
	var valid []domain.BusinessDiscoverer
	for _, d := range discoverers {
		if d != nil {
			valid = append(valid, d)
		}
	}
	return &MultiDiscoverer{discoverers: valid}
}

// Name returns a comma-joined list of the underlying discoverer names.
func (m *MultiDiscoverer) Name() string {
	names := make([]string, len(m.discoverers))
	for i, d := range m.discoverers {
		names[i] = d.Name()
	}
	return strings.Join(names, ",")
}

// DiscoverBusinessNames runs all discoverers concurrently and merges results.
func (m *MultiDiscoverer) DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error) {
	type result struct {
		names []string
		err   error
	}

	results := make([]result, len(m.discoverers))
	var wg sync.WaitGroup

	for i, d := range m.discoverers {
		wg.Add(1)
		go func(idx int, disc domain.BusinessDiscoverer) {
			defer wg.Done()
			names, err := disc.DiscoverBusinessNames(ctx, query, location)
			results[idx] = result{names: names, err: err}
		}(i, d)
	}
	wg.Wait()

	seen := make(map[string]bool)
	var names []string

	for _, r := range results {
		for _, name := range r.names {
			key := strings.ToLower(name)
			if !seen[key] {
				seen[key] = true
				names = append(names, name)
			}
		}
	}

	if len(names) == 0 {
		return nil, domain.ErrNoResults
	}
	return names, nil
}
