// Package cnae provides CNAE code lookup using data from CNAE 2.0
// (IBGE/CONCLA), embedded directly in the binary from cnaes.json.
// Results are cached in MongoDB so repeated queries are instant.
package cnae

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

//go:embed cnaes.json
var cnaesJSON []byte

const (
	mongoDatabase   = "leadfinder"
	mongoCollection = "cnaes"
)

// cnaeDoc is the MongoDB/embedded document structure for a CNAE record.
type cnaeDoc struct {
	Codigo          string `bson:"codigo"            json:"codigo"`
	Descricao       string `bson:"descricao"         json:"descricao"`
	CodigoFormatado string `bson:"codigo_formatado"  json:"codigo_formatado"`
	// Foursquare signals that this CNAE category has good POI coverage on
	// Foursquare.  Stored in MongoDB so operators can tune it per-CNAE without
	// redeploying.  Populated automatically on first seed via isFoursquareCNAE().
	Foursquare bool `bson:"foursquare"        json:"foursquare"`
}

// Provider implements domain.CNAEEnricher.
type Provider struct {
	mongoClient *mongo.Client
	webSearcher domain.WebSearcher // optional DDG fallback for CNAE lookup
	cnaes       []cnaeDoc
	mu          sync.RWMutex
	loaded      bool
}

// WithWebSearcher injects an optional web searcher used as a fallback when
// no CNAE match is found in the embedded/MongoDB list (e.g. DuckDuckGo).
// It returns the receiver so calls can be chained.
func (p *Provider) WithWebSearcher(ws domain.WebSearcher) *Provider {
	p.webSearcher = ws
	return p
}

// New creates a new CNAE Provider.
// mongoURI is the MongoDB connection string (e.g. "mongodb://mongo:27017").
// If empty or unreachable, MongoDB caching is skipped — the embedded JSON is used directly.
func New(mongoURI string) (*Provider, error) {
	p := &Provider{}

	if mongoURI != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
		if err != nil {
			fmt.Printf("cnae: MongoDB connect warning: %v\n", err)
		} else if err = client.Ping(ctx, nil); err != nil {
			fmt.Printf("cnae: MongoDB ping warning: %v — running without cache\n", err)
			_ = client.Disconnect(context.Background())
		} else {
			p.mongoClient = client
			fmt.Println("cnae: MongoDB connected")
		}
	}

	return p, nil
}

// Close disconnects the MongoDB client.
func (p *Provider) Close() {
	if p.mongoClient != nil {
		_ = p.mongoClient.Disconnect(context.Background())
	}
}

// Name satisfies domain.DiscoveryProvider.
func (p *Provider) Name() string { return "cnae-xls" }

// EnsureCache populates the in-memory CNAE list.
// Priority:
//  1. Already loaded in memory — return immediately.
//  2. MongoDB has documents — load from there.
//  3. Parse embedded cnaes.json and persist to MongoDB.
//
// Safe to call concurrently and multiple times (executes once).
func (p *Provider) EnsureCache(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.loaded {
		return
	}

	// 1. Try MongoDB cache (populated on previous runs).
	if p.mongoClient != nil {
		coll := p.mongoClient.Database(mongoDatabase).Collection(mongoCollection)
		count, err := coll.CountDocuments(ctx, bson.M{})
		if err == nil && count > 0 {
			cursor, err := coll.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "codigo", Value: 1}}))
			if err == nil {
				var docs []cnaeDoc
				if err = cursor.All(ctx, &docs); err == nil && len(docs) > 0 {
					// Schema migration: if no document has foursquare=true the collection
					// was seeded with the old schema (no foursquare field).  Force a
					// re-seed so the field is populated from isFoursquareCNAE().
					hasFlag := false
					for _, d := range docs {
						if d.Foursquare {
							hasFlag = true
							break
						}
					}
					if hasFlag {
						p.cnaes = docs
						p.loaded = true
						fmt.Printf("cnae: loaded %d CNAEs from MongoDB cache\n", len(docs))
						return
					}
					// Old schema detected — fall through to re-seed.
					fmt.Println("cnae: old schema detected (no foursquare field) — re-seeding MongoDB")
				}
			}
		}
	}

	// 2. Parse the embedded JSON (CNAE 2.0 — 699 classes from IBGE/CONCLA).
	var docs []cnaeDoc
	if err := json.Unmarshal(cnaesJSON, &docs); err != nil {
		fmt.Printf("cnae: failed to parse embedded cnaes.json: %v\n", err)
		p.loaded = true // don't retry
		return
	}
	// Populate the Foursquare flag for each CNAE from Go logic.
	// This is the authoritative seed value; MongoDB becomes the source of truth
	// after the first run (operators can override individual docs in Mongo).
	for i := range docs {
		docs[i].Foursquare = isFoursquareCNAE(docs[i].Codigo)
	}
	fmt.Printf("cnae: loaded %d CNAEs from embedded data\n", len(docs))

	// 3. Persist to MongoDB for next startup (avoids JSON parse cost).
	if p.mongoClient != nil && len(docs) > 0 {
		coll := p.mongoClient.Database(mongoDatabase).Collection(mongoCollection)
		_, _ = coll.DeleteMany(ctx, bson.M{})
		ifaces := make([]interface{}, len(docs))
		for i, d := range docs {
			ifaces[i] = d
		}
		if _, err := coll.InsertMany(ctx, ifaces); err == nil {
			// Create index on codigo for faster queries
			idxModel := mongo.IndexModel{
				Keys:    bson.D{{Key: "codigo", Value: 1}},
				Options: options.Index().SetUnique(true),
			}
			_, _ = coll.Indexes().CreateOne(ctx, idxModel)
			fmt.Printf("cnae: saved %d CNAEs to MongoDB (schema v2 with foursquare field)\n", len(docs))
		} else {
			fmt.Printf("cnae: MongoDB insert warning: %v\n", err)
		}
	}

	p.cnaes = docs
	p.loaded = true
}

// reCNAEFormatted matches a formatted CNAE code (e.g. "47.81-4" or "56.11-2") in free text.
var reCNAEFormatted = regexp.MustCompile(`\b(\d{2})\.(\d{2})[-–](\d)\b`)

// FindCNAE finds the best matching CNAE for the given query string.
// Always returns a non-nil SearchContext (degrades gracefully if no match).
// If the keyword-matching score is zero, it falls back to a DuckDuckGo web
// search (when a WebSearcher is configured) to recover the correct CNAE from
// search snippets.
func (p *Provider) FindCNAE(ctx context.Context, query string) (*domain.SearchContext, error) {
	p.EnsureCache(ctx)

	p.mu.RLock()
	cnaes := p.cnaes
	p.mu.RUnlock()

	if len(cnaes) == 0 {
		return defaultSearchContext(query), nil
	}

	best := findBestMatch(query, cnaes)

	// Fallback: if no keyword matched, try to infer CNAE via web search.
	if best.Codigo == "" && p.webSearcher != nil {
		best = p.findCNAEViaDDG(ctx, query, cnaes)
	}

	if best.Codigo == "" {
		return defaultSearchContext(query), nil
	}

	// UseFoursquare is read from the MongoDB field (populated on seed from
	// isFoursquareCNAE; operators can override individual docs in MongoDB).
	return &domain.SearchContext{
		CNAE:            best.CodigoFormatado,
		CNAEDescription: best.Descricao,
		SearchTerms:     generateSearchTerms(query, best),
		UseFoursquare:   best.Foursquare,
	}, nil
}

// findCNAEViaDDG performs a web search for "{query} CNAE" and tries to extract
// a formatted CNAE code from result snippets/URLs, then looks it up in cnaes.
func (p *Provider) findCNAEViaDDG(ctx context.Context, query string, cnaes []cnaeDoc) cnaeDoc {
	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	results, err := p.webSearcher.Search(searchCtx, fmt.Sprintf(`"%s" CNAE Brasil`, query))
	if err != nil || len(results) == 0 {
		return cnaeDoc{}
	}

	// Build a lookup map: raw codigo (digits only) → cnaeDoc
	byCode := make(map[string]cnaeDoc, len(cnaes))
	for _, c := range cnaes {
		byCode[c.Codigo] = c
	}

	for _, r := range results {
		text := r.Title + " " + r.Snippet + " " + r.URL
		matches := reCNAEFormatted.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			// m[0]="47.81-4", m[1]="47", m[2]="81", m[3]="4"
			raw := m[1] + m[2] + m[3] // e.g. "47814"
			// Pad to 7 digits (CNAE codes) — try both with and without leading zero on sub-class
			for _, code := range []string{raw, "0" + raw} {
				if doc, ok := byCode[code]; ok {
					fmt.Printf("cnae: DDG fallback matched %s → %s\n", query, doc.CodigoFormatado)
					return doc
				}
			}
		}
	}

	// Second pass: try keyword matching on broader description from first result
	if len(results) > 0 {
		expanded := results[0].Title + " " + results[0].Snippet
		return findBestMatch(expanded, cnaes)
	}
	return cnaeDoc{}
}

// ─── Keyword matching ─────────────────────────────────────────────────────────

var accentNorm = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
	"é", "e", "è", "e", "ê", "e",
	"í", "i", "ì", "i", "î", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o",
	"ú", "u", "ù", "u", "û", "u",
	"ç", "c",
)

var stopWordsSet = map[string]bool{
	"de": true, "do": true, "da": true, "dos": true, "das": true,
	"e": true, "ou": true, "o": true, "a": true, "os": true, "as": true,
	"em": true, "no": true, "na": true, "nos": true, "nas": true,
	"para": true, "por": true, "com": true, "sem": true, "que": true,
	"nao": true, "especificados": true, "especificadas": true,
	"anteriormente": true, "outros": true, "outras": true,
}

// queryExpansion maps common Portuguese business terms to CNAE vocabulary.
var queryExpansion = map[string][]string{
	// Retail signals — keep "varejista" so scoring prefers comércio over fabricação
	"loja":     {"varejista", "comercio"},
	"lojas":    {"varejista", "comercio"},
	"comercio": {"varejista", "comercio"},
	"varejo":   {"varejista"},

	// Vestuário
	"roupas":   {"vestuario", "varejista", "confeccoes"},
	"roupa":    {"vestuario", "varejista"},
	"moda":     {"vestuario", "confeccoes"},
	"vestido":  {"vestuario"},
	"boutique": {"vestuario", "varejista"},

	// Calçados
	"calcados": {"calcados", "varejista"},
	"calcado":  {"calcados", "varejista"},
	"sapatos":  {"calcados", "varejista"},
	"sapato":   {"calcados", "varejista"},

	// Saúde / Farmácia
	"farmacia":  {"farmaceuticos", "varejista", "drogarias"},
	"farmacias": {"farmaceuticos", "varejista", "drogarias"},
	"drogaria":  {"farmaceuticos", "varejista", "drogarias"},
	"remedio":   {"medicamentos", "farmaceuticos"},
	"remedios":  {"medicamentos", "farmaceuticos"},

	// Alimentação
	"mercado":      {"supermercados", "alimentos", "alimentar"},
	"supermercado": {"supermercados"},
	"padaria":      {"panificadora", "confeitaria"},
	"confeitaria":  {"confeitaria", "panificadora"},
	"restaurante":  {"restaurantes", "alimentacao", "alimentar"},
	"lanchonete":   {"lanchonetes", "alimentacao"},
	"pizzaria":     {"restaurantes", "alimentacao"},
	"bar":          {"bares", "bebidas"},

	// Hospedagem
	"hotel":   {"hoteis", "hospedagem", "alojamento"},
	"pousada": {"pousadas", "hospedagem", "alojamento"},
	"hostel":  {"hospedagem", "alojamento"},

	// Fitness / Bem-estar
	"academia": {"condicionamento", "fisico", "ginastica"},
	"ginasio":  {"condicionamento", "fisico"},

	// Beleza
	"beleza":       {"cabeleireiros", "estetica", "cosmeticos"},
	"cabeleireiro": {"cabeleireiros"},
	"salao":        {"cabeleireiros", "estetica"},
	"estetica":     {"estetica", "cabeleireiros"},

	// Pet
	"pet":         {"veterinarias"},
	"veterinaria": {"veterinarias"},
	"petshop":     {"veterinarias"},

	// Joias
	"joias":     {"joalherias", "bijuterias", "relogios"},
	"joalheria": {"joalherias"},

	// Veículos
	"autopecas": {"autopecas", "veiculos", "automoveis"},
	"pecas":     {"autopecas", "veiculos"},

	// Casa / Móveis
	"moveis":    {"moveis", "decoracao"},
	"decoracao": {"moveis", "decoracao"},

	// Tecnologia
	"informatica":      {"informatica", "computadores", "equipamentos"},
	"celular":          {"telecomunicacoes", "equipamentos"},
	"eletronicos":      {"eletronicos", "equipamentos"},
	"eletrodomesticos": {"eletrodomesticos"},

	// Construção
	"construcao": {"construcao", "material"},

	// Livraria / Papelaria
	"papelaria":  {"papelaria", "livros"},
	"livraria":   {"livros"},
	"brinquedos": {"brinquedos"},

	// Ótica
	"optica":   {"opticas", "oculos"},
	"otica":    {"opticas", "oculos"}, // "ótica" sem p normaliza para "otica"
	"oculos":   {"opticas", "oculos"},
	"relogios": {"relogios", "joalherias"},

	// Cosméticos
	"perfume":    {"cosmeticos", "perfumaria"},
	"cosmeticos": {"cosmeticos", "perfumaria", "higiene"},
	"higiene":    {"higiene", "cosmeticos"},
}

func normalizeText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = accentNorm.Replace(s)
	// Replace any non-alphanumeric character with a space so word-boundary
	// matching works correctly (e.g. "padaria, laticínios" → "padaria laticionios").
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func tokenize(s string) []string {
	s = normalizeText(s)
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	var tokens []string
	for _, p := range parts {
		if len(p) > 1 && !stopWordsSet[p] {
			tokens = append(tokens, p)
		}
	}
	return tokens
}

func expandTokens(tokens []string) []string {
	seen := make(map[string]bool)
	var expanded []string
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			expanded = append(expanded, t)
		}
		if synonyms, ok := queryExpansion[t]; ok {
			for _, syn := range synonyms {
				for _, s := range strings.Fields(syn) {
					if !seen[s] {
						seen[s] = true
						expanded = append(expanded, s)
					}
				}
			}
		}
	}
	return expanded
}

func findBestMatch(query string, cnaes []cnaeDoc) cnaeDoc {
	queryTokens := tokenize(query)
	expandedTokens := expandTokens(queryTokens)

	var best cnaeDoc
	bestScore := 0

	for _, c := range cnaes {
		// Pad description with spaces so word-boundary checks work with HasPrefix/Contains.
		descNorm := " " + normalizeText(c.Descricao) + " "
		score := 0
		for _, t := range expandedTokens {
			// Word-boundary aware: check for " token " to avoid "pet" matching "petróleo".
			if strings.Contains(descNorm, " "+t+" ") {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best = c
		}
	}

	return best
}

func generateSearchTerms(query string, best cnaeDoc) []string {
	terms := []string{query}
	if best.Descricao != "" {
		words := tokenize(best.Descricao)
		if len(words) > 0 {
			terms = append(terms, words[0])
		}
	}
	return terms
}

// isFoursquareCNAE returns true for CNAEs whose business type is well-covered
// by the Foursquare POI database. Uses 2-digit CNAE section prefixes so that
// the entire economic section is captured without listing every sub-code.
//
// Covered sections (aligned with Foursquare's main categories):
//
//	47 – Comércio varejista (lojas, mercados, farmácias, padarias …)
//	55 – Alojamento (hotéis, pousadas, hostels …)
//	56 – Alimentação (restaurantes, bares, lanchonetes, cafeterias …)
//	59 – Audiovisual / cinema
//	90 – Artes, cultura, entretenimento, casas noturnas
//	93 – Esporte, recreação e lazer (academias, parques, estádios …)
//	96 – Serviços pessoais (salões, barbearias, estética, lavanderia …)
func isFoursquareCNAE(codigo string) bool {
	foursquareSections := []string{
		"47", // Comércio varejista — inclui padarias, mercados, farmácias, roupas
		"55", // Alojamento — hotéis, pousadas, albergues
		"56", // Alimentação — restaurantes, bares, lanchonetes, catering
		"59", // Atividades audiovisuais / cinema
		"90", // Artes, cultura, esporte e recreação (gestão)
		"93", // Atividades esportivas e de lazer — academias, parques
		"96", // Outras atividades de serviços pessoais — salões, barbearias
	}
	for _, section := range foursquareSections {
		if strings.HasPrefix(codigo, section) {
			return true
		}
	}
	return false
}

func defaultSearchContext(query string) *domain.SearchContext {
	return &domain.SearchContext{
		SearchTerms: []string{query},
	}
}
