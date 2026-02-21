package cnpj

import (
	"context"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// MultiNameSearcher tries multiple CNPJNameSearcher providers in order and returns
// the first result that succeeds. Useful for chaining cnpj.biz + empresaqui.com.br.
type MultiNameSearcher struct {
	providers []domain.CNPJNameSearcher
}

// NewMultiNameSearcher creates a MultiNameSearcher from the given providers (tried left-to-right).
func NewMultiNameSearcher(providers ...domain.CNPJNameSearcher) *MultiNameSearcher {
	return &MultiNameSearcher{providers: providers}
}

func (m *MultiNameSearcher) Name() string { return "multi-cnpj-name" }

// SearchByName tries each provider in order and returns the first non-empty CNPJ.
func (m *MultiNameSearcher) SearchByName(ctx context.Context, name, city string) (string, error) {
	for _, p := range m.providers {
		cnpj, err := p.SearchByName(ctx, name, city)
		if err == nil && cnpj != "" {
			return cnpj, nil
		}
	}
	return "", domain.ErrNoResults
}
