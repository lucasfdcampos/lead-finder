// Package cnpj provides BrasilAPIProvider (primary) and CNPJBizProvider (fallback).
package cnpj

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	brasilAPIBase        = "https://brasilapi.com.br/api"
	brasilAPITimeout     = 10 * time.Second
	cnpjBizBase          = "https://www.cnpj.biz"
	cnpjBizTimeout       = 20 * time.Second
	openCNPJABase        = "https://open.cnpja.com"
	openCNPJATimeout     = 10 * time.Second
	publicaCNPJWSBase    = "https://publica.cnpj.ws"
	publicaCNPJWSTimeout = 10 * time.Second
)

// BrasilAPIProvider fetches CNPJ data from the free Brasil API.
type BrasilAPIProvider struct {
	client *http.Client
}

func NewBrasilAPI() *BrasilAPIProvider {
	return &BrasilAPIProvider{client: &http.Client{Timeout: brasilAPITimeout}}
}

func (p *BrasilAPIProvider) Name() string { return "brasilapi" }

func (p *BrasilAPIProvider) FetchByCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	cnpj = sanitizeCNPJ(cnpj)
	if len(cnpj) != 14 {
		return nil, domain.ErrInvalidCNPJ
	}

	ctx, cancel := context.WithTimeout(ctx, brasilAPITimeout)
	defer cancel()

	url := fmt.Sprintf("%s/cnpj/v1/%s", brasilAPIBase, cnpj)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brasilapi: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, domain.ErrNotFound
	case http.StatusTooManyRequests:
		return nil, domain.ErrRateLimited
	case http.StatusOK:
		// ok
	default:
		return nil, fmt.Errorf("brasilapi: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw brasilAPICompany
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("brasilapi: unmarshal error: %w", err)
	}

	return raw.toDomainLead(), nil
}

func (p *BrasilAPIProvider) SearchByCNAE(_ context.Context, _, _, _ string) ([]domain.Lead, error) {
	return nil, domain.ErrNoResults
}

// CNPJBizProvider scrapes cnpj.biz as a fallback.
type CNPJBizProvider struct {
	client *http.Client
}

func NewCNPJBiz() *CNPJBizProvider {
	return &CNPJBizProvider{client: &http.Client{Timeout: cnpjBizTimeout}}
}

func (p *CNPJBizProvider) Name() string { return "cnpjbiz" }

func (p *CNPJBizProvider) FetchByCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	cnpj = sanitizeCNPJ(cnpj)
	if len(cnpj) != 14 {
		return nil, domain.ErrInvalidCNPJ
	}

	ctx, cancel := context.WithTimeout(ctx, cnpjBizTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s", cnpjBizBase, cnpj)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cnpjbiz: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cnpjbiz: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseCNPJBizHTML(cnpj, string(body)), nil
}

// SearchByName queries cnpj.biz for a company by name+city and returns the first
// matching 14-digit CNPJ string. It does NOT require a web-search engine.
func (p *CNPJBizProvider) SearchByName(ctx context.Context, name, city string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cnpjBizTimeout)
	defer cancel()

	query := name + " " + city
	reqURL := fmt.Sprintf("%s/busca?q=%s", cnpjBizBase, strings.ReplaceAll(query, " ", "+"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cnpjbiz name search: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	leads := parseCNPJBizSearchHTML(string(body))
	if len(leads) == 0 {
		return "", domain.ErrNoResults
	}
	return leads[0].CNPJ, nil
}

func (p *CNPJBizProvider) SearchByCNAE(ctx context.Context, cnae, city, state string) ([]domain.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, cnpjBizTimeout)
	defer cancel()

	query := fmt.Sprintf("%s %s %s", cnae, city, state)
	url := fmt.Sprintf("%s/busca?q=%s", cnpjBizBase, strings.ReplaceAll(query, " ", "+"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cnpjbiz: search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseCNPJBizSearchHTML(string(body)), nil
}

// ─── OpenCNPJA Provider ──────────────────────────────────────────────────────

// OpenCNPJAProvider fetches CNPJ data from open.cnpja.com (no auth required).
type OpenCNPJAProvider struct {
	client *http.Client
}

func NewOpenCNPJA() *OpenCNPJAProvider {
	return &OpenCNPJAProvider{client: &http.Client{Timeout: openCNPJATimeout}}
}

func (p *OpenCNPJAProvider) Name() string { return "opencnpja" }

func (p *OpenCNPJAProvider) FetchByCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	cnpj = sanitizeCNPJ(cnpj)
	if len(cnpj) != 14 {
		return nil, domain.ErrInvalidCNPJ
	}

	ctx, cancel := context.WithTimeout(ctx, openCNPJATimeout)
	defer cancel()

	url := fmt.Sprintf("%s/office/%s", openCNPJABase, cnpj)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencnpja: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, domain.ErrNotFound
	case http.StatusTooManyRequests:
		return nil, domain.ErrRateLimited
	case http.StatusOK:
		// ok
	default:
		return nil, fmt.Errorf("opencnpja: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw openCNPJAOffice
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("opencnpja: unmarshal error: %w", err)
	}

	return raw.toDomainLead(), nil
}

func (p *OpenCNPJAProvider) SearchByCNAE(_ context.Context, _, _, _ string) ([]domain.Lead, error) {
	return nil, domain.ErrNoResults // open.cnpja.com has no CNAE search endpoint
}

type openCNPJAOffice struct {
	TaxID   string `json:"taxId"`
	Alias   string `json:"alias"`
	Company struct {
		Name    string `json:"name"`
		Members []struct {
			Person struct {
				Name string `json:"name"`
			} `json:"person"`
			Role struct {
				Text string `json:"text"`
			} `json:"role"`
		} `json:"members"`
	} `json:"company"`
	Address struct {
		City  string `json:"city"`
		State string `json:"state"`
	} `json:"address"`
	Phones []struct {
		Area   string `json:"area"`
		Number string `json:"number"`
	} `json:"phones"`
}

func (r *openCNPJAOffice) toDomainLead() *domain.Lead {
	var partners []domain.Partner
	for _, m := range r.Company.Members {
		partners = append(partners, domain.Partner{
			Name:          m.Person.Name,
			Qualification: m.Role.Text,
		})
	}

	var phone string
	if len(r.Phones) > 0 {
		p := r.Phones[0]
		num := p.Number
		if len(num) >= 8 {
			phone = fmt.Sprintf("(%s) %s-%s", p.Area, num[:len(num)-4], num[len(num)-4:])
		} else if num != "" {
			phone = fmt.Sprintf("(%s) %s", p.Area, num)
		}
	}

	return &domain.Lead{
		Name:      r.Company.Name,
		TradeName: r.Alias,
		CNPJ:      r.TaxID,
		Partners:  partners,
		Phone:     phone,
		Address: domain.Address{
			City:  r.Address.City,
			State: r.Address.State,
		},
		Source: []string{"opencnpja"},
	}
}

// ─── BrasilAPI Provider ──────────────────────────────────────────────────────

type brasilAPICompany struct {
	CNPJ                string             `json:"cnpj"`
	RazaoSocial         string             `json:"razao_social"`
	NomeFantasia        string             `json:"nome_fantasia"`
	DataAbertura        string             `json:"data_inicio_atividade"`
	CNAEFiscalDescricao string             `json:"cnae_fiscal_descricao"`
	CNAEFiscal          int                `json:"cnae_fiscal"`
	Logradouro          string             `json:"logradouro"`
	Numero              string             `json:"numero"`
	Complemento         string             `json:"complemento"`
	Bairro              string             `json:"bairro"`
	Municipio           string             `json:"municipio"`
	UF                  string             `json:"uf"`
	CEP                 string             `json:"cep"`
	Telefone1           string             `json:"ddd_telefone_1"`
	Telefone2           string             `json:"ddd_telefone_2"`
	Email               string             `json:"email"`
	QSA                 []brasilAPIPartner `json:"qsa"`
}

type brasilAPIPartner struct {
	NomeSocio         string `json:"nome_socio"`
	QualificacaoSocio string `json:"qualificacao_socio"`
}

func (c *brasilAPICompany) toDomainLead() *domain.Lead {
	var partners []domain.Partner
	for _, q := range c.QSA {
		partners = append(partners, domain.Partner{
			Name:          q.NomeSocio,
			Qualification: q.QualificacaoSocio,
		})
	}

	phone := strings.TrimSpace(c.Telefone1)
	if phone == "" {
		phone = strings.TrimSpace(c.Telefone2)
	}

	// Detect mobile numbers: in Brazil, mobiles have 11 digits (DDD + 9 + 8 digits).
	// Mobile = WhatsApp candidate. Format stored as "55"+DDD+number (E.164-style).
	var whatsapp string
	if digits := reNonDigit.ReplaceAllString(phone, ""); len(digits) == 11 {
		whatsapp = "55" + digits
	}

	return &domain.Lead{
		Name:        c.RazaoSocial,
		TradeName:   c.NomeFantasia,
		CNPJ:        c.CNPJ,
		CNAEDesc:    c.CNAEFiscalDescricao,
		CNAE:        fmt.Sprintf("%d", c.CNAEFiscal),
		OpeningDate: c.DataAbertura,
		Address: domain.Address{
			Street:       c.Logradouro,
			Number:       c.Numero,
			Complement:   c.Complemento,
			Neighborhood: c.Bairro,
			City:         c.Municipio,
			State:        c.UF,
			ZipCode:      c.CEP,
		},
		Phone:    phone,
		WhatsApp: whatsapp,
		Email:    strings.ToLower(strings.TrimSpace(c.Email)),
		Partners: partners,
		Source:   []string{"brasilapi"},
	}
}

var (
	reCNPJPattern = regexp.MustCompile(`[0-9]{2}\.[0-9]{3}\.[0-9]{3}/[0-9]{4}-[0-9]{2}`)
	reName        = regexp.MustCompile(`(?i)<h1[^>]*>\s*([^<]+)\s*</h1>`)
	rePhoneHTML   = regexp.MustCompile(`\(?[0-9]{2}\)?\s*[0-9]{4,5}-?[0-9]{4}`)
	reNonDigit    = regexp.MustCompile(`\D`)
)

func parseCNPJBizHTML(cnpj, html string) *domain.Lead {
	lead := &domain.Lead{CNPJ: cnpj, Source: []string{"cnpjbiz"}}
	if m := reName.FindStringSubmatch(html); len(m) > 1 {
		lead.Name = strings.TrimSpace(m[1])
	}
	if m := rePhoneHTML.FindString(html); m != "" {
		lead.Phone = m
	}
	return lead
}

func parseCNPJBizSearchHTML(html string) []domain.Lead {
	cnpjs := reCNPJPattern.FindAllString(html, 50)
	seen := make(map[string]bool)
	var leads []domain.Lead
	for _, c := range cnpjs {
		clean := sanitizeCNPJ(c)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		leads = append(leads, domain.Lead{CNPJ: clean, Source: []string{"cnpjbiz"}})
	}
	return leads
}

func sanitizeCNPJ(cnpj string) string {
	return reNonDigit.ReplaceAllString(cnpj, "")
}

// ─── PublicaCNPJWS Provider ───────────────────────────────────────────────────

// PublicaCNPJWSProvider fetches CNPJ data from publica.cnpj.ws (free, no auth required).
type PublicaCNPJWSProvider struct {
	client *http.Client
}

func NewPublicaCNPJWS() *PublicaCNPJWSProvider {
	return &PublicaCNPJWSProvider{client: &http.Client{Timeout: publicaCNPJWSTimeout}}
}

func (p *PublicaCNPJWSProvider) Name() string { return "publicacnpjws" }

func (p *PublicaCNPJWSProvider) FetchByCNPJ(ctx context.Context, cnpj string) (*domain.Lead, error) {
	cnpj = sanitizeCNPJ(cnpj)
	if len(cnpj) != 14 {
		return nil, domain.ErrInvalidCNPJ
	}

	ctx, cancel := context.WithTimeout(ctx, publicaCNPJWSTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/cnpj/%s", publicaCNPJWSBase, cnpj)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("publicacnpjws: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, domain.ErrNotFound
	case http.StatusTooManyRequests:
		return nil, domain.ErrRateLimited
	case http.StatusOK:
		// ok
	default:
		return nil, fmt.Errorf("publicacnpjws: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw publicaCNPJWSResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("publicacnpjws: unmarshal error: %w", err)
	}

	return raw.toDomainLead(), nil
}

func (p *PublicaCNPJWSProvider) SearchByCNAE(_ context.Context, _, _, _ string) ([]domain.Lead, error) {
	return nil, domain.ErrNoResults
}

// ─── JSON structs for publica.cnpj.ws ─────────────────────────────────────────

type publicaCNPJWSResponse struct {
	RazaoSocial string `json:"razao_social"`
	Socios      []struct {
		Nome              string `json:"nome"`
		QualificacaoSocio struct {
			Descricao string `json:"descricao"`
		} `json:"qualificacao_socio"`
	} `json:"socios"`
	Estabelecimento struct {
		CNPJ         string          `json:"cnpj"`
		NomeFantasia string          `json:"nome_fantasia"`
		DDD1         string          `json:"ddd1"`
		Telefone1    string          `json:"telefone1"`
		DDD2         string          `json:"ddd2"`
		Telefone2    string          `json:"telefone2"`
		Email        string          `json:"email"`
		Logradouro   string          `json:"logradouro"`
		Numero       string          `json:"numero"`
		Bairro       string          `json:"bairro"`
		CEP          string          `json:"cep"`
		Cidade       json.RawMessage `json:"cidade"`
		Estado       json.RawMessage `json:"estado"`
	} `json:"estabelecimento"`
}

func (r *publicaCNPJWSResponse) toDomainLead() *domain.Lead {
	est := r.Estabelecimento

	var partners []domain.Partner
	for _, s := range r.Socios {
		partners = append(partners, domain.Partner{
			Name:          s.Nome,
			Qualification: s.QualificacaoSocio.Descricao,
		})
	}

	// Build phone from DDD + number (e.g. "11" + "98765432" or "43" + "996095612")
	var phone, whatsapp string
	if est.DDD1 != "" && est.Telefone1 != "" {
		allDigits := est.DDD1 + est.Telefone1
		n := len(est.Telefone1)
		if n >= 4 {
			phone = fmt.Sprintf("(%s) %s-%s", est.DDD1, est.Telefone1[:n-4], est.Telefone1[n-4:])
		} else {
			phone = fmt.Sprintf("(%s) %s", est.DDD1, est.Telefone1)
		}
		// 11 digits (DDD + mobile 9XXXXXXXX) → WhatsApp candidate
		if len(allDigits) == 11 {
			whatsapp = "55" + allDigits
		}
	}

	// cidade can be a string or {"nome":"..."} object
	city := publicaCNPJWSString(est.Cidade, "nome")
	// estado can be a string or {"sigla":"..."} object
	state := publicaCNPJWSString(est.Estado, "sigla")

	return &domain.Lead{
		Name:      r.RazaoSocial,
		TradeName: est.NomeFantasia,
		CNPJ:      est.CNPJ,
		Partners:  partners,
		Phone:     phone,
		WhatsApp:  whatsapp,
		Email:     strings.ToLower(strings.TrimSpace(est.Email)),
		Address: domain.Address{
			Street:       est.Logradouro,
			Number:       est.Numero,
			Neighborhood: est.Bairro,
			ZipCode:      est.CEP,
			City:         city,
			State:        state,
		},
		Source: []string{"publicacnpjws"},
	}
}

// publicaCNPJWSString decodes a json.RawMessage that may be either a plain string
// or a JSON object. For objects, it returns the value at the given field key.
func publicaCNPJWSString(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) == nil {
		if v, ok := obj[field]; ok {
			if sv, ok := v.(string); ok {
				return sv
			}
		}
	}
	return ""
}
