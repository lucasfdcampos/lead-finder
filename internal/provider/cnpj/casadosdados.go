// Package cnpj – CasaDosDados CNPJ name search provider.
//
// CasaDosDados (casadosdados.com.br) provides a completely free public REST API
// for searching Brazilian companies by name and city. No API key required.
// Returns structured JSON with CNPJ, address, phone, email and QSA.
//
// Endpoint: POST https://api.casadosdados.com.br/v2/public/cnpj/pesquisa
//
// Implements domain.CNPJNameSearcher.
package cnpj

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	casaDosDadosBase    = "https://api.casadosdados.com.br/v2/public/cnpj/pesquisa"
	casaDosDadosTimeout = 15 * time.Second
)

// CasaDosDadosProvider searches CNPJ by company name + city using the free CasaDosDados API.
type CasaDosDadosProvider struct {
	client *http.Client
}

// NewCasaDosDados creates a new CasaDosDadosProvider.
func NewCasaDosDados() *CasaDosDadosProvider {
	return &CasaDosDadosProvider{client: &http.Client{Timeout: casaDosDadosTimeout}}
}

func (p *CasaDosDadosProvider) Name() string { return "casadosdados" }

// SearchByName searches CasaDosDados for a company by name and city,
// returning the 14-digit CNPJ of the best match.
func (p *CasaDosDadosProvider) SearchByName(ctx context.Context, name, city string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, casaDosDadosTimeout)
	defer cancel()

	// Normalize city: CasaDosDados uses uppercase, no accents.
	cityUpper := strings.ToUpper(city)

	payload := casaDosDadosRequest{
		Query: casaDosDadosQuery{
			RazaoSocial: []string{name},
		},
		Extras: casaDosDadosExtras{},
		Page:   1,
	}
	if city != "" {
		payload.Query.Municipio = []string{cityUpper}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("casadosdados: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, casaDosDadosBase, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("casadosdados: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Origin", "https://casadosdados.com.br")
	req.Header.Set("Referer", "https://casadosdados.com.br/")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("casadosdados: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return "", domain.ErrRateLimited
	case http.StatusOK:
		// ok
	default:
		return "", fmt.Errorf("casadosdados: HTTP %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("casadosdados: read body: %w", err)
	}

	var result casaDosDadosResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("casadosdados: unmarshal: %w", err)
	}

	if len(result.Data.CNPJ) == 0 {
		return "", domain.ErrNoResults
	}

	// Pick the best matching company by name similarity.
	searchWords := significantWords(name)
	bestCNPJ := ""
	bestScore := -1

	for _, item := range result.Data.CNPJ {
		cnpjDigits := sanitizeCNPJ(item.CNPJ)
		if len(cnpjDigits) != 14 {
			continue
		}
		score := matchScore(strings.ToLower(item.RazaoSocial), searchWords)
		if score > bestScore {
			bestScore = score
			bestCNPJ = cnpjDigits
		}
	}

	if bestCNPJ == "" {
		// Fallback: take first result's CNPJ
		bestCNPJ = sanitizeCNPJ(result.Data.CNPJ[0].CNPJ)
	}

	if len(bestCNPJ) != 14 {
		return "", domain.ErrNoResults
	}

	return bestCNPJ, nil
}

// ─── CasaDosDados API types ───────────────────────────────────────────────────

type casaDosDadosRequest struct {
	Query  casaDosDadosQuery  `json:"query"`
	RangeQ casaDosDadosRange  `json:"range_query"`
	Extras casaDosDadosExtras `json:"extras"`
	Page   int                `json:"page"`
}

type casaDosDadosQuery struct {
	RazaoSocial []string `json:"razao_social,omitempty"`
	Municipio   []string `json:"municipio,omitempty"`
}

type casaDosDadosRange struct {
	DataAbertura casaDosDadosDateRange `json:"data_abertura"`
}

type casaDosDadosDateRange struct {
	Lte *string `json:"lte"`
	Gte *string `json:"gte"`
}

type casaDosDadosExtras struct {
	SomenteMEI              bool `json:"somente_mei"`
	ExcluirMEI              bool `json:"excluir_mei"`
	ComEmail                bool `json:"com_email"`
	InclAtividadeSecundaria bool `json:"incluir_atividade_secundaria"`
	ComContatoTelefonico    bool `json:"com_contato_telefonico"`
	SomenteFixo             bool `json:"somente_fixo"`
	SomenteCelular          bool `json:"somente_celular"`
	SomenteMatriz           bool `json:"somente_matriz"`
	SomenteFilial           bool `json:"somente_filial"`
}

type casaDosDadosResponse struct {
	Success bool `json:"success"`
	Data    struct {
		CNPJ []casaDosDadosItem `json:"cnpj"`
	} `json:"data"`
}

type casaDosDadosItem struct {
	CNPJ         string `json:"cnpj"`
	RazaoSocial  string `json:"razao_social"`
	NomeFantasia string `json:"nome_fantasia"`
	Municipio    string `json:"municipio"`
	UF           string `json:"uf"`
}
