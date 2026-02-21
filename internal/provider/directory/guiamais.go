// Package directory provides a BusinessDiscoverer backed by GuiaMais.com.br,
// Brazil's largest yellow-pages directory (~40 M listings).
//
// URL pattern:
//
//	https://www.guiamais.com.br/{query-slug}/{city-slug}-{state}/lista/1
//
// Business names are embedded as JSON-LD Organization/LocalBusiness objects
// and also in itemprop="name" span tags.
// No API key required. No aggressive bot detection observed.
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
	guiaMaisBase       = "https://www.guiamais.com.br"
	guiaMaisTimeout    = 15 * time.Second
	guiaMaisMaxPages   = 3 // each page has up to 25 listings
	guiaMaisMaxWorkers = 4
)

// guiaMaisCategoryMap maps Portuguese query terms to GuiaMais category slugs.
// GuiaMais URL: /empresas/{category}/{city}-{state} or /{query-slug}/{city}-{state}/lista/{page}
// When a query is not in the map the slug is used directly.
var guiaMaisCategoryMap = map[string][]string{
	// Fashion
	"loja de roupas": {"lojas-de-roupas", "confeccoes"},
	"roupa":          {"lojas-de-roupas", "confeccoes"},
	"roupas":         {"lojas-de-roupas", "confeccoes"},
	"vestuario":      {"lojas-de-roupas", "confeccoes"},
	"vestuário":      {"lojas-de-roupas", "confeccoes"},
	"boutique":       {"boutiques", "lojas-de-roupas"},
	"moda":           {"lojas-de-roupas"},
	"calcado":        {"lojas-de-calcados"},
	"calçado":        {"lojas-de-calcados"},
	"sapato":         {"lojas-de-calcados"},
	"joalheria":      {"joalherias"},
	"bijouteria":     {"bijuterias"},

	// Food & drink
	"restaurante":  {"restaurantes"},
	"restaurantes": {"restaurantes"},
	"lanchonete":   {"lanchonetes", "restaurantes"},
	"pizzaria":     {"pizzarias", "lanchonetes"},
	"padaria":      {"padarias"},
	"bar":          {"bares"},
	"churrascaria": {"churrascarias", "restaurantes"},
	"sorveteria":   {"sorveterias", "lanchonetes"},
	"doceria":      {"doceiras", "confeitarias"},
	"confeitaria":  {"confeitarias"},
	"cafeteria":    {"cafeterias"},
	"hamburgueria": {"hamburguerias"},
	"marmitaria":   {"marmitarias"},
	"acougue":      {"acougues"},
	"açougue":      {"acougues"},
	"padarias":     {"padarias"},
	"hortifruti":   {"hortifruticulturas", "mercados"},
	"mercearia":    {"mercearias", "mercados"},

	// Health
	"farmacia":     {"farmacias"},
	"farmácia":     {"farmacias"},
	"clinica":      {"clinicas", "clinicas-medicas"},
	"clínica":      {"clinicas", "clinicas-medicas"},
	"laboratorio":  {"laboratorios-de-analises-clinicas"},
	"laboratório":  {"laboratorios-de-analises-clinicas"},
	"hospital":     {"hospitais"},
	"dentista":     {"clinicas-odontologicas", "dentistas"},
	"fisioterapia": {"clinicas-de-fisioterapia"},
	"nutricao":     {"nutricionistas"},
	"nutrição":     {"nutricionistas"},
	"psicologia":   {"psicologos"},
	"ortopedia":    {"ortopedistas"},
	"oftalmologia": {"oftalmologistas"},
	"otica":        {"oticas"},
	"ótica":        {"oticas"},
	"optica":       {"oticas"},

	// Beauty
	"academia":        {"academias"},
	"salao de beleza": {"saloes-de-beleza"},
	"salão de beleza": {"saloes-de-beleza"},
	"barbearia":       {"barbearias"},
	"cabeleireiro":    {"cabeleireiros"},
	"manicure":        {"manicures-e-pedicures"},

	// Automotive
	"autopecas":      {"autopecas"},
	"autopeças":      {"autopecas"},
	"mecanica":       {"mecanicas"},
	"mecânica":       {"mecanicas"},
	"oficina":        {"mecanicas"},
	"funilaria":      {"funilarias"},
	"borracharia":    {"borracharias"},
	"lava jato":      {"lava-rapido"},
	"concessionaria": {"concessionarias"},
	"concessionária": {"concessionarias"},

	// Commerce
	"supermercado": {"supermercados"},
	"mercado":      {"supermercados", "mercados"},
	"papelaria":    {"papelarias"},
	"livraria":     {"livrarias"},
	"informatica":  {"informatica"},
	"informática":  {"informatica"},
	"celular":      {"celulares-e-smartphones"},
	"eletronicos":  {"eletronicos"},
	"eletrônicos":  {"eletronicos"},
	"floricultura": {"floriculturas"},
	"brinquedos":   {"lojas-de-brinquedos"},
	"petshop":      {"petshops"},
	"pet shop":     {"petshops"},

	// Construction
	"material de construcao": {"materiais-de-construcao"},
	"material de construção": {"materiais-de-construcao"},
	"construtora":            {"construtoras"},
	"eletricista":            {"eletricistas"},
	"encanador":              {"encanadores"},
	"marcenaria":             {"marcenarias"},
	"serralheria":            {"serralherias"},
	"vidracaria":             {"vidracarias"},
	"pintura":                {"pinturas-em-geral"},
	"dedetizacao":            {"dedetizadoras"},
	"dedetização":            {"dedetizadoras"},

	// Professional services
	"advocacia":     {"advogados", "escritorios-de-advocacia"},
	"advogado":      {"advogados"},
	"contabilidade": {"contabilidades"},
	"imobiliaria":   {"imobiliarias"},
	"imobiliária":   {"imobiliarias"},
	"despachante":   {"despachantes"},
	"cartorio":      {"cartorios"},
	"cartório":      {"cartorios"},
	"seguradora":    {"seguradoras"},
	"consultoria":   {"consultorias"},

	// Education
	"escola":       {"escolas"},
	"colegio":      {"colegios"},
	"colégio":      {"colegios"},
	"faculdade":    {"faculdades"},
	"universidade": {"universidades"},
	"idiomas":      {"cursos-de-idiomas"},
	"autoescola":   {"autoescolas"},
	"curso":        {"cursos"},

	// Hospitality / events
	"hotel":       {"hoteis"},
	"pousada":     {"pousadas"},
	"buffet":      {"buffets"},
	"fotografia":  {"fotografos"},
	"veterinario": {"veterinarios"},
	"veterinário": {"veterinarios"},
}

// GuiaMaisScraper implements domain.BusinessDiscoverer using GuiaMais.com.br.
type GuiaMaisScraper struct {
	client *http.Client
}

// NewGuiaMaisScraper creates a new GuiaMaisScraper.
func NewGuiaMaisScraper() *GuiaMaisScraper {
	return &GuiaMaisScraper{
		client: &http.Client{Timeout: guiaMaisTimeout},
	}
}

// Name satisfies the DiscoveryProvider interface.
func (s *GuiaMaisScraper) Name() string { return "guiamais" }

// DiscoverBusinessNames fetches GuiaMais listing pages for the given query
// and location, returning deduplicated business name strings.
func (s *GuiaMaisScraper) DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error) {
	city, state := parseLocation(location)
	if city == "" || state == "" {
		return nil, fmt.Errorf("guiamais: could not parse city/state from %q", location)
	}

	citySlug := slugify(city)
	stateSlug := strings.ToLower(state)
	querySlug := slugify(query)

	urls := buildGuiaMaisURLs(citySlug, stateSlug, querySlug)
	if len(urls) == 0 {
		return nil, domain.ErrNoResults
	}

	type result struct{ names []string }
	results := make([]result, len(urls))
	sem := make(chan struct{}, guiaMaisMaxWorkers)
	var wg sync.WaitGroup

	for i, u := range urls {
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pageCtx, cancel := context.WithTimeout(ctx, guiaMaisTimeout)
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

// fetchPage downloads one GuiaMais listing page and extracts business names.
func (s *GuiaMaisScraper) fetchPage(ctx context.Context, rawURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("guiamais: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://www.guiamais.com.br/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("guiamais: request %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // category/city not found — try next candidate
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("guiamais: HTTP %d for %s", resp.StatusCode, rawURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("guiamais: read body: %w", err)
	}

	return parseGuiaMaisNames(string(body)), nil
}

// ─── HTML parsing ─────────────────────────────────────────────────────────────

// reGuiaMaisJSONLD matches business names from JSON-LD LocalBusiness/Organization blobs.
var reGuiaMaisJSONLD = regexp.MustCompile(`"@type"\s*:\s*"(?:LocalBusiness|Organization)"[^}]*?"name"\s*:\s*"([^"]+)"`)

// reGuiaMaisItemprop matches names from <span itemprop="name">NAME</span>.
var reGuiaMaisItemprop = regexp.MustCompile(`itemprop="name"[^>]*>([A-ZÁÉÍÓÚÀÂÃÊÔÕÜÇ][^<]{3,80})</`)

// reGuiaMaisH2 matches names from result-card h2 headings.
var reGuiaMaisH2 = regexp.MustCompile(`<h2[^>]*class="[^"]*company[^"]*"[^>]*>\s*([A-ZÁÉÍÓÚÀÂÃÊÔÕÜÇ][^<]{3,80})\s*</h2>`)

func parseGuiaMaisNames(body string) []string {
	seen := make(map[string]bool)
	var names []string

	add := func(name string) {
		name = strings.TrimSpace(name)
		if len([]rune(name)) < 3 {
			return
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}

	// Primary: JSON-LD (most reliable, structured data)
	for _, m := range reGuiaMaisJSONLD.FindAllStringSubmatch(body, 200) {
		add(m[1])
	}

	// Secondary: itemprop="name" spans
	for _, m := range reGuiaMaisItemprop.FindAllStringSubmatch(body, 200) {
		add(m[1])
	}

	// Tertiary: h2 with company class
	for _, m := range reGuiaMaisH2.FindAllStringSubmatch(body, 200) {
		add(m[1])
	}

	return names
}

// ─── URL construction ─────────────────────────────────────────────────────────

func buildGuiaMaisURLs(citySlug, stateSlug, querySlug string) []string {
	query := strings.ReplaceAll(querySlug, "-", " ")
	slugs, ok := guiaMaisCategoryMap[query]
	if !ok {
		// Best-effort fallback: use query slug directly and also try plural form.
		slugs = []string{querySlug, querySlug + "s"}
	}

	location := citySlug + "-" + stateSlug

	var urls []string
	seen := make(map[string]bool)
	for _, slug := range slugs {
		for page := 1; page <= guiaMaisMaxPages; page++ {
			u := fmt.Sprintf("%s/%s/%s/lista/%d", guiaMaisBase, slug, location, page)
			if !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	return urls
}
