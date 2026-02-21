// Package directory provides BusinessDiscoverer implementations backed by
// Brazilian business directories.
//
// This file implements the Solutudo.com.br scraper.
//
// URL pattern: https://www.solutudo.com.br/empresas/{state}/{city}/{category}/
//
// Business names are embedded as JSON-LD LocalBusiness objects directly in
// the page HTML — no JavaScript rendering required.
// No API key required. No rate limits observed.
package directory

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	solutudoBase       = "https://www.solutudo.com.br/empresas"
	solutudoTimeout    = 12 * time.Second
	solutudoMaxWorkers = 4
)

// SolutudoScraper implements domain.BusinessDiscoverer using Solutudo.com.br.
type SolutudoScraper struct {
	client *http.Client
}

// NewSolutudoScraper creates a new SolutudoScraper.
func NewSolutudoScraper() *SolutudoScraper {
	return &SolutudoScraper{
		client: &http.Client{Timeout: solutudoTimeout},
	}
}

// Name satisfies the DiscoveryProvider interface.
func (s *SolutudoScraper) Name() string { return "solutudo" }

// DiscoverBusinessNames fetches Solutudo category pages for the given query
// and location, returning deduplicated business name strings.
func (s *SolutudoScraper) DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error) {
	city, state := parseLocation(location)
	if city == "" || state == "" {
		return nil, fmt.Errorf("solutudo: could not parse city/state from %q", location)
	}

	citySlug := slugify(city)
	stateSlug := strings.ToLower(state)
	querySlug := slugify(query)

	urls := buildSolutudoURLs(solutudoBase, citySlug, stateSlug, querySlug)
	if len(urls) == 0 {
		return nil, domain.ErrNoResults
	}

	// Fetch all URLs concurrently.
	type result struct{ names []string }
	results := make([]result, len(urls))
	sem := make(chan struct{}, solutudoMaxWorkers)
	var wg sync.WaitGroup

	for i, u := range urls {
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pageCtx, cancel := context.WithTimeout(ctx, solutudoTimeout)
			names, _ := s.fetchPage(pageCtx, rawURL)
			cancel()
			results[idx] = result{names: names}
		}(i, u)
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

// fetchPage fetches one Solutudo listing page and extracts business names
// from the inline JSON-LD LocalBusiness objects.
func (s *SolutudoScraper) fetchPage(ctx context.Context, rawURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("solutudo: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("solutudo: request %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // category doesn't exist for this city
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("solutudo: HTTP %d for %s", resp.StatusCode, rawURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("solutudo: read body: %w", err)
	}

	return parseSolutudoNames(string(body)), nil
}

// ─── HTML parsing ─────────────────────────────────────────────────────────────

// reSolutudoName matches business names in Solutudo's inline JSON-LD:
// {"@type":"LocalBusiness","name":"BUSINESS NAME","logo":...}
var reSolutudoName = regexp.MustCompile(`"@type":"LocalBusiness","name":"([^"]+)"`)

func parseSolutudoNames(body string) []string {
	seen := make(map[string]bool)
	var names []string

	for _, m := range reSolutudoName.FindAllStringSubmatch(body, 200) {
		name := strings.TrimSpace(m[1])
		if len([]rune(name)) < 3 {
			continue
		}
		lower := strings.ToLower(name)
		if !seen[lower] {
			seen[lower] = true
			names = append(names, name)
		}
	}
	return names
}

// ─── URL construction ─────────────────────────────────────────────────────────

// solutudoCategoryMap maps common Portuguese query words to Solutudo category slugs.
// Solutudo URL: /empresas/{state}/{city}/{category}/
// Note: Solutudo uses PLURAL slugs (restaurantes, lanchonetes, etc.)
var solutudoCategoryMap = map[string][]string{
	"loja de roupas": {"confeccoes", "roupas-e-acessorios"},
	"roupa":          {"confeccoes", "roupas-e-acessorios"},
	"roupas":         {"confeccoes", "roupas-e-acessorios"},
	"vestuario":      {"confeccoes", "roupas-e-acessorios"},
	"vestuário":      {"confeccoes", "roupas-e-acessorios"},
	"boutique":       {"roupas-e-acessorios"},
	"moda":           {"confeccoes", "roupas-e-acessorios"},

	"restaurante":  {"restaurantes", "churrascarias", "lanchonetes"},
	"restaurantes": {"restaurantes", "churrascarias"},
	"lanchonete":   {"lanchonetes", "restaurantes"},
	"lanchonetes":  {"lanchonetes"},
	"pizzaria":     {"pizzarias", "lanchonetes"},
	"padaria":      {"padarias"},
	"bar":          {"bares"},
	"churrascaria": {"churrascarias", "restaurantes"},

	"farmacia":  {"farmacias"},
	"academia":  {"academias"},
	"autopecas": {"autopecas"},
	"autopeças": {"autopecas"},
	"calcado":   {"calcados"},
	"calçado":   {"calcados"},
	"sapato":    {"calcados"},

	"salao de beleza": {"saloes-de-beleza"},
	"salão de beleza": {"saloes-de-beleza"},
	"barbearia":       {"barbearias"},
	"cabeleireiro":    {"cabeleireiros"},
	"manicure":        {"manicures-e-pedicures"},

	// Automotive
	"mecanica":            {"mecanicas", "autopecas"},
	"mecânica":            {"mecanicas", "autopecas"},
	"oficina":             {"mecanicas"},
	"funilaria":           {"funilarias"},
	"vidracaria":          {"vidracarias"},
	"eletrica automotiva": {"eletrica-automotiva"},
	"borracharia":         {"borracharias"},
	"lava jato":           {"lava-jatos"},
	"concessionaria":      {"concessionarias"},
	"concessionária":      {"concessionarias"},

	// Professional services
	"advocacia":     {"advocacias", "escritorios-de-advocacia"},
	"advogado":      {"advocacias"},
	"contabilidade": {"contabilidades"},
	"contador":      {"contabilidades"},
	"imobiliaria":   {"imobiliarias"},
	"imobiliária":   {"imobiliarias"},
	"despachante":   {"despachantes"},
	"seguradora":    {"seguradoras"},
	"cartorio":      {"cartorios"},
	"cartório":      {"cartorios"},
	"consultoria":   {"consultorias"},

	// Construction & home
	"construcao":             {"construcoes", "materiais-de-construcao"},
	"construção":             {"construcoes", "materiais-de-construcao"},
	"construtora":            {"construtoras"},
	"eletricista":            {"eletricistas"},
	"encanador":              {"encanadores"},
	"pintura":                {"pinturas"},
	"marcenaria":             {"marcenarias"},
	"serralheria":            {"serralherias"},
	"dedetizacao":            {"dedetizacoes"},
	"dedetização":            {"dedetizacoes"},
	"material de construcao": {"materiais-de-construcao"},
	"material de construção": {"materiais-de-construcao"},

	// Education
	"escola":       {"escolas"},
	"colegio":      {"colegios"},
	"colégio":      {"colegios"},
	"faculdade":    {"faculdades"},
	"universidade": {"universidades"},
	"idiomas":      {"cursos-de-idiomas"},
	"curso":        {"cursos"},
	"autoescola":   {"autoescolas"},

	// Health & wellness
	"clinica":      {"clinicas", "clinicas-medicas"},
	"clínica":      {"clinicas", "clinicas-medicas"},
	"laboratorio":  {"laboratorios"},
	"laboratório":  {"laboratorios"},
	"fisioterapia": {"fisioterapias"},
	"nutricao":     {"nutricoes"},
	"nutrição":     {"nutricoes"},
	"psicologia":   {"psicologias"},
	"ortopedia":    {"ortopedias"},
	"dermatologia": {"dermatologias"},
	"oftalmologia": {"oftalmologias"},

	// Food & specialty
	"sorveteria":   {"sorveteiras", "lanchonetes"},
	"doceria":      {"doceiras", "confeitarias"},
	"confeitaria":  {"confeitarias"},
	"hortifruti":   {"hortifrutis"},
	"mercearia":    {"mercearias"},
	"acougue":      {"acougues"},
	"açougue":      {"acougues"},
	"cafeteria":    {"cafeterias", "lanchonetes"},
	"hamburgueria": {"hamburguerias", "lanchonetes"},
	"marmitaria":   {"marmitarias", "restaurantes"},

	// Commerce / retail
	"supermercado": {"supermercados"},
	"papelaria":    {"papelarias"},
	"livraria":     {"livrarias"},
	"informatica":  {"informaticas"},
	"informática":  {"informaticas"},
	"celular":      {"celulares"},
	"optica":       {"oticas"},
	"ótica":        {"oticas"},
	"floricultura": {"floriculturas"},
	"brinquedos":   {"brinquedos"},

	// Hospitality / events
	"hotel":      {"hoteis"},
	"pousada":    {"pousadas"},
	"buffet":     {"buffets"},
	"fotografia": {"fotografias", "fotografos"},
}

func buildSolutudoURLs(base, citySlug, stateSlug, querySlug string) []string {
	query := strings.ReplaceAll(querySlug, "-", " ")
	slugs, ok := solutudoCategoryMap[query]
	if !ok {
		// Try pluralizing the slug as a best-effort fallback.
		slugs = []string{querySlug + "s", querySlug}
	}

	var urls []string
	for _, slug := range slugs {
		urls = append(urls, fmt.Sprintf("%s/%s/%s/%s/", base, stateSlug, citySlug, slug))
	}
	return urls
}
