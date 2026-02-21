// Package directory provides a BusinessDiscoverer backed by AppLocal.com.br,
// a Brazilian business directory with plain-HTML listings (no bot detection).
//
// URL pattern: https://applocal.com.br/empresas/{city}-{state}/{cat}/{subcat}/
//
// Business names are embedded as `title` attributes on listing-card anchors.
// No API key required. No rate limits observed.
package directory

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	appLocalBase       = "https://applocal.com.br/empresas"
	appLocalTimeout    = 12 * time.Second
	appLocalMaxPages   = 5 // pages per subcategory (page 1 … page 5, ~36 per page)
	appLocalMaxWorkers = 8 // concurrent HTTP fetches
)

// AppLocalScraper implements domain.BusinessDiscoverer using AppLocal.com.br.
type AppLocalScraper struct {
	client *http.Client
}

// NewAppLocalScraper creates a new AppLocalScraper.
func NewAppLocalScraper() *AppLocalScraper {
	return &AppLocalScraper{
		client: &http.Client{Timeout: appLocalTimeout},
	}
}

// Name satisfies the DiscoveryProvider interface.
func (s *AppLocalScraper) Name() string { return "applocal" }

// DiscoverBusinessNames fetches AppLocal category pages for the given query
// and location, returning deduplicated business name strings.
//
// It tries ALL matching subcategories (up to appLocalMaxPages pages each),
// fetching them concurrently via a semaphore-limited worker pool.
func (s *AppLocalScraper) DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error) {
	city, state := parseLocation(location)
	if city == "" || state == "" {
		return nil, fmt.Errorf("applocal: could not parse city/state from %q", location)
	}

	citySlug := slugify(city)
	stateSlug := strings.ToLower(state)
	querySlug := slugify(query)

	urls := buildAllURLs(appLocalBase, citySlug, stateSlug, querySlug, appLocalMaxPages)

	// Fetch all URLs concurrently, limited to appLocalMaxWorkers goroutines.
	type result struct {
		names []string
	}
	results := make([]result, len(urls))
	sem := make(chan struct{}, appLocalMaxWorkers)
	var wg sync.WaitGroup

	for i, u := range urls {
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pageCtx, cancel := context.WithTimeout(ctx, appLocalTimeout)
			names, _ := s.fetchPage(pageCtx, rawURL)
			cancel()
			results[idx] = result{names: names}
		}(i, u)
	}
	wg.Wait()

	// Merge all results, deduplicating by lowercase key.
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

// fetchPage fetches one AppLocal listing page and extracts business names
// from `title` attributes on company-card elements.
func (s *AppLocalScraper) fetchPage(ctx context.Context, rawURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("applocal: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("applocal: request %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // page doesn't exist — try next candidate
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("applocal: HTTP %d for %s", resp.StatusCode, rawURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("applocal: read body: %w", err)
	}

	return parseAppLocalTitles(string(body)), nil
}

// ─── HTML parsing ─────────────────────────────────────────────────────────────

// reTitle matches business names embedded as title= attributes on listing cards.
// AppLocal embeds names as title="NAME" on listing-card anchors.
// The first ~40 KB is CSS; real content starts after that.
var reTitle = regexp.MustCompile(`title="([A-ZÁÉÍÓÚÀÂÃÊÔÕÜÇ][^"]{4,80})"`)

// uiTitles are title= values that belong to navigation/UI, not businesses.
var uiTitles = map[string]bool{
	"termos de uso":                     true,
	"página inicial":                    true,
	"politica de privacidade":           true,
	"política de privacidade":           true,
	"sobre o applocal":                  true,
	"realizar uma nova pesquisa":        true,
	"cadastre sua empresa":              true,
	"applocal":                          true,
	"mapa":                              true,
	"whatsapp":                          true,
	"facebook":                          true,
	"instagram":                         true,
	"contato com o applocal":            true,
	"encontre empresas por cidade":      true,
	"cadastro gratis de empresa":        true,
	"cadastro grátis de empresa":        true,
	"solicitar exclusao de uma empresa": true,
	"solicitar exclusão de uma empresa": true,
	"concordar e fechar":                true,
	"ver mais empresas":                 true,
	"proxima pagina":                    true,
	"próxima página":                    true,
}

func parseAppLocalTitles(body string) []string {
	// Skip the inline CSS block (~40 KB) — actual listings start after.
	const cssOffset = 40_000
	content := body
	if len(body) > cssOffset {
		content = body[cssOffset:]
	}

	seen := make(map[string]bool)
	var names []string

	for _, m := range reTitle.FindAllStringSubmatch(content, 200) {
		name := html.UnescapeString(strings.TrimSpace(m[1]))
		lower := strings.ToLower(name)
		if uiTitles[lower] {
			continue
		}
		if !seen[lower] {
			seen[lower] = true
			names = append(names, name)
		}
	}
	return names
}

// ─── URL construction ─────────────────────────────────────────────────────────

// categoryMap maps common Portuguese query words to one or more AppLocal
// (category, subcategory) slug pairs.
// AppLocal URL: /empresas/{city}-{state}/{category}/{subcategory}/
var categoryMap = map[string][][2]string{
	"loja de roupas": {
		{"moda-e-vestuario", "loja-de-roupas"},
		{"moda-e-vestuario", "confeccao"},
		{"moda-e-vestuario", "boutique"},
		{"moda-e-vestuario", "brecho"},
		{"moda-e-vestuario", "vestuario"},
		{"moda-e-vestuario", "moda"},
	},
	"roupa":      {{"moda-e-vestuario", "loja-de-roupas"}, {"moda-e-vestuario", "confeccao"}, {"moda-e-vestuario", "vestuario"}},
	"roupas":     {{"moda-e-vestuario", "loja-de-roupas"}, {"moda-e-vestuario", "confeccao"}, {"moda-e-vestuario", "vestuario"}},
	"vestuario":  {{"moda-e-vestuario", "loja-de-roupas"}, {"moda-e-vestuario", "vestuario"}, {"moda-e-vestuario", "confeccao"}},
	"vestuário":  {{"moda-e-vestuario", "loja-de-roupas"}, {"moda-e-vestuario", "vestuario"}, {"moda-e-vestuario", "confeccao"}},
	"boutique":   {{"moda-e-vestuario", "boutique"}, {"moda-e-vestuario", "loja-de-roupas"}},
	"moda":       {{"moda-e-vestuario", "moda"}, {"moda-e-vestuario", "loja-de-roupas"}},
	"calcado":    {{"moda-e-vestuario", "calcado"}},
	"calçado":    {{"moda-e-vestuario", "calcado"}},
	"sapato":     {{"moda-e-vestuario", "calcado"}},
	"joalheria":  {{"moda-e-vestuario", "joalheria"}},
	"bijouteria": {{"moda-e-vestuario", "bijuteria"}},

	"restaurante":  {{"alimentacao", "restaurante"}, {"alimentacao", "churrascaria"}},
	"restaurantes": {{"alimentacao", "restaurante"}, {"alimentacao", "churrascaria"}},
	"lanchonete":   {{"alimentacao", "lanchonete"}, {"alimentacao", "restaurante"}},
	"pizzaria":     {{"alimentacao", "pizzaria"}, {"alimentacao", "lanchonete"}},
	"padaria":      {{"alimentacao", "padaria"}},
	"bar":          {{"alimentacao", "bar"}},
	"confeitaria":  {{"alimentacao", "confeitaria"}},
	"churrascaria": {{"alimentacao", "churrascaria"}, {"alimentacao", "restaurante"}},

	"academia": {{"saude", "academia"}},
	"farmacia": {{"saude", "farmacia"}},
	"hospital": {{"saude", "hospital"}},
	"dentista": {{"saude", "dentista"}},
	"otica":    {{"saude", "otica"}},

	"cabeleireiro":    {{"beleza", "cabeleireiro"}},
	"salao de beleza": {{"beleza", "salao-de-beleza"}, {"beleza", "cabeleireiro"}},
	"salão de beleza": {{"beleza", "salao-de-beleza"}, {"beleza", "cabeleireiro"}},
	"barbearia":       {{"beleza", "barbearia"}},
	"manicure":        {{"beleza", "manicure"}},

	"autopecas":   {{"automoveis", "autopecas"}},
	"autopeças":   {{"automoveis", "autopecas"}},
	"borracharia": {{"automoveis", "borracharia"}},
	"lavagem":     {{"automoveis", "lavagem"}},

	"supermercado": {{"comercio", "supermercado"}},
	"mercado":      {{"comercio", "mercado"}, {"comercio", "supermercado"}},

	"petshop":     {{"animais", "petshop"}},
	"pet shop":    {{"animais", "petshop"}},
	"veterinario": {{"animais", "veterinario"}},
	"veterinário": {{"animais", "veterinario"}},

	"creche": {{"educacao", "creche"}},

	"hotel": {{"turismo", "hotel"}},

	"movel":                  {{"casa-e-jardim", "movel"}},
	"móvel":                  {{"casa-e-jardim", "movel"}},
	"moveis":                 {{"casa-e-jardim", "movel"}},
	"móveis":                 {{"casa-e-jardim", "movel"}},
	"eletrodomestico":        {{"casa-e-jardim", "eletrodomestico"}},
	"eletrodoméstico":        {{"casa-e-jardim", "eletrodomestico"}},
	"material de construcao": {{"construcao", "material-de-construcao"}},
	"material de construção": {{"construcao", "material-de-construcao"}},

	// Automotive
	"mecanica":            {{"automoveis", "mecanica"}, {"automoveis", "autopecas"}},
	"mecânica":            {{"automoveis", "mecanica"}, {"automoveis", "autopecas"}},
	"oficina":             {{"automoveis", "mecanica"}},
	"funilaria":           {{"automoveis", "funilaria"}, {"automoveis", "mecanica"}},
	"vidracaria":          {{"automoveis", "vidracaria"}, {"construcao", "vidracaria"}},
	"eletrica automotiva": {{"automoveis", "eletrica-automotiva"}, {"automoveis", "mecanica"}},
	"elétrica automotiva": {{"automoveis", "eletrica-automotiva"}, {"automoveis", "mecanica"}},
	"lava jato":           {{"automoveis", "lava-jato"}, {"automoveis", "lavagem"}},
	"concessionaria":      {{"automoveis", "concessionaria"}},
	"concessionária":      {{"automoveis", "concessionaria"}},

	// Construction & home
	"construcao":        {{"construcao", "material-de-construcao"}, {"construcao", "construtora"}},
	"construção":        {{"construcao", "material-de-construcao"}, {"construcao", "construtora"}},
	"construtora":       {{"construcao", "construtora"}},
	"eletricista":       {{"construcao", "eletricista"}, {"servicos", "eletricista"}},
	"encanador":         {{"construcao", "encanador"}, {"servicos", "encanador"}},
	"pintura":           {{"construcao", "pintura"}, {"servicos", "pintura"}},
	"marcenaria":        {{"construcao", "marcenaria"}, {"casa-e-jardim", "marcenaria"}},
	"serralheria":       {{"construcao", "serralheria"}},
	"marmoraria":        {{"construcao", "marmoraria"}},
	"dedetizacao":       {{"servicos", "dedetizacao"}},
	"dedetização":       {{"servicos", "dedetizacao"}},
	"impermeabilizacao": {{"construcao", "impermeabilizacao"}},
	"impermeabilização": {{"construcao", "impermeabilizacao"}},

	// Professional services
	"advocacia":     {{"servicos", "advocacia"}, {"servicos", "escritorio-de-advocacia"}},
	"advogado":      {{"servicos", "advocacia"}},
	"contabilidade": {{"servicos", "contabilidade"}, {"financeiro", "contabilidade"}},
	"contador":      {{"servicos", "contabilidade"}},
	"imobiliaria":   {{"servicos", "imobiliaria"}, {"imoveis", "imobiliaria"}},
	"imobiliária":   {{"servicos", "imobiliaria"}, {"imoveis", "imobiliaria"}},
	"despachante":   {{"servicos", "despachante"}},
	"seguradora":    {{"servicos", "seguradora"}, {"financeiro", "seguradora"}},
	"cartorio":      {{"servicos", "cartorio"}},
	"cartório":      {{"servicos", "cartorio"}},
	"consultoria":   {{"servicos", "consultoria"}},
	"coworking":     {{"servicos", "coworking"}},

	// Education
	"escola":       {{"educacao", "escola"}, {"educacao", "colegio"}},
	"colegio":      {{"educacao", "colegio"}, {"educacao", "escola"}},
	"colégio":      {{"educacao", "colegio"}, {"educacao", "escola"}},
	"faculdade":    {{"educacao", "faculdade"}, {"educacao", "universidade"}},
	"universidade": {{"educacao", "universidade"}, {"educacao", "faculdade"}},
	"pre-escola":   {{"educacao", "pre-escola"}, {"educacao", "creche"}},
	"pre escola":   {{"educacao", "pre-escola"}, {"educacao", "creche"}},
	"idiomas":      {{"educacao", "idiomas"}, {"educacao", "curso-de-idiomas"}},
	"curso":        {{"educacao", "curso"}},
	"autoescola":   {{"educacao", "autoescola"}},

	// Health & wellness
	"clinica":        {{"saude", "clinica"}, {"saude", "clinica-medica"}},
	"clínica":        {{"saude", "clinica"}, {"saude", "clinica-medica"}},
	"laboratorio":    {{"saude", "laboratorio"}, {"saude", "laboratorio-clinico"}},
	"laboratório":    {{"saude", "laboratorio"}, {"saude", "laboratorio-clinico"}},
	"fisioterapia":   {{"saude", "fisioterapia"}},
	"nutricao":       {{"saude", "nutricao"}, {"saude", "nutricionista"}},
	"nutrição":       {{"saude", "nutricao"}, {"saude", "nutricionista"}},
	"psicologia":     {{"saude", "psicologia"}, {"saude", "psicologo"}},
	"psicólogo":      {{"saude", "psicologia"}},
	"fonoaudiologia": {{"saude", "fonoaudiologia"}},
	"ortopedia":      {{"saude", "ortopedia"}, {"saude", "clinica"}},
	"dermatologia":   {{"saude", "dermatologia"}, {"saude", "clinica"}},
	"oftalmologia":   {{"saude", "oftalmologia"}, {"saude", "clinica"}},
	"cardiologia":    {{"saude", "cardiologia"}, {"saude", "clinica"}},

	// Food & specialty
	"sorveteria":   {{"alimentacao", "sorveteria"}, {"alimentacao", "lanchonete"}},
	"doceria":      {{"alimentacao", "doceria"}, {"alimentacao", "confeitaria"}},
	"hortifruti":   {{"alimentacao", "hortifruti"}, {"comercio", "hortifruti"}},
	"mercearia":    {{"alimentacao", "mercearia"}, {"comercio", "mercearia"}},
	"acougue":      {{"alimentacao", "acougue"}, {"comercio", "acougue"}},
	"açougue":      {{"alimentacao", "acougue"}, {"comercio", "acougue"}},
	"peixaria":     {{"alimentacao", "peixaria"}, {"comercio", "peixaria"}},
	"cafeteria":    {{"alimentacao", "cafeteria"}, {"alimentacao", "lanchonete"}},
	"hamburgueria": {{"alimentacao", "hamburgueria"}, {"alimentacao", "lanchonete"}},
	"sushi":        {{"alimentacao", "restaurante-japones"}, {"alimentacao", "restaurante"}},
	"marmitaria":   {{"alimentacao", "marmitaria"}, {"alimentacao", "restaurante"}},

	// Commerce / retail
	"papelaria":               {{"comercio", "papelaria"}},
	"livraria":                {{"comercio", "livraria"}},
	"informatica":             {{"tecnologia", "informatica"}, {"comercio", "informatica"}},
	"informática":             {{"tecnologia", "informatica"}, {"comercio", "informatica"}},
	"celular":                 {{"tecnologia", "celular"}, {"comercio", "celular"}},
	"eletronicos":             {{"tecnologia", "eletronicos"}, {"comercio", "eletronicos"}},
	"eletrônicos":             {{"tecnologia", "eletronicos"}, {"comercio", "eletronicos"}},
	"farmacia de manipulacao": {{"saude", "farmacia-de-manipulacao"}, {"saude", "farmacia"}},
	"farmácia de manipulação": {{"saude", "farmacia-de-manipulacao"}, {"saude", "farmacia"}},
	"optica":                  {{"saude", "optica"}, {"comercio", "optica"}},
	"ótica":                   {{"saude", "optica"}, {"comercio", "optica"}},
	"joias":                   {{"moda-e-vestuario", "joalheria"}, {"comercio", "joalheria"}},
	"flores":                  {{"comercio", "floricultura"}, {"casa-e-jardim", "floricultura"}},
	"floricultura":            {{"comercio", "floricultura"}, {"casa-e-jardim", "floricultura"}},
	"instrumentos musicais":   {{"comercio", "instrumentos-musicais"}},
	"brinquedos":              {{"comercio", "brinquedos"}, {"comercio", "brinquedoteca"}},

	// Hospitality / events
	"pousada":           {{"turismo", "pousada"}, {"turismo", "hotel"}},
	"hostel":            {{"turismo", "hostel"}, {"turismo", "hotel"}},
	"buffet":            {{"eventos", "buffet"}, {"alimentacao", "buffet"}},
	"espaco de eventos": {{"eventos", "espaco-de-eventos"}},
	"espaço de eventos": {{"eventos", "espaco-de-eventos"}},
	"salao de festas":   {{"eventos", "salao-de-festas"}, {"eventos", "espaco-de-eventos"}},
	"fotografia":        {{"eventos", "fotografia"}, {"servicos", "fotografia"}},
}

// buildAllURLs returns every AppLocal URL to fetch:
//   - one URL per matching subcategory
//   - × maxPages pagination URLs (/pagina/2/, /pagina/3/, …)
//
// If the query is not in categoryMap, falls back to the slug itself.
func buildAllURLs(base, citySlug, stateSlug, querySlug string, maxPages int) []string {
	query := strings.ReplaceAll(querySlug, "-", " ")
	pairs, ok := categoryMap[query]
	if !ok {
		pairs = [][2]string{
			{querySlug, querySlug},
			{querySlug + "s", querySlug},
		}
	}

	var urls []string
	for _, pair := range pairs {
		cat, sub := pair[0], pair[1]
		urls = append(urls, fmt.Sprintf("%s/%s-%s/%s/%s/", base, citySlug, stateSlug, cat, sub))
		for p := 2; p <= maxPages; p++ {
			urls = append(urls, fmt.Sprintf("%s/%s-%s/%s/%s/pagina/%d/", base, citySlug, stateSlug, cat, sub, p))
		}
	}
	return urls
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// slugify converts a string to a lowercase URL slug (removes diacritics,
// replaces whitespace/punctuation with hyphens).
func slugify(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, strings.ToLower(strings.TrimSpace(s)))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	result = re.ReplaceAllString(result, "-")
	return strings.Trim(result, "-")
}

// parseLocation splits "City-State" or "City, State" into (city, state).
func parseLocation(location string) (city, state string) {
	location = strings.TrimSpace(location)

	// "Arapongas-PR"
	if idx := strings.LastIndex(location, "-"); idx > 0 {
		c := strings.TrimSpace(location[:idx])
		s := strings.TrimSpace(location[idx+1:])
		if len(s) == 2 {
			return c, s
		}
	}

	// "Arapongas, PR"
	if idx := strings.LastIndex(location, ","); idx > 0 {
		c := strings.TrimSpace(location[:idx])
		s := strings.TrimSpace(location[idx+1:])
		if len(s) >= 2 {
			return c, strings.TrimSpace(s)
		}
	}

	return location, ""
}
