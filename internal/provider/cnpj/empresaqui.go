// Package cnpj – EmpresaQuiProvider searches CNPJ by company name on empresaqui.com.br.
//
// EmpresaQui is a free Brazilian business directory that allows name-based lookups
// without requiring an API key. CNPJ numbers appear both in URL paths (/cnpj/DIGITS/)
// and as formatted text (XX.XXX.XXX/XXXX-XX) in result pages.
//
// Implements domain.CNPJNameSearcher.
package cnpj

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	empresaQuiBase    = "https://www.empresaqui.com.br"
	empresaQuiTimeout = 15 * time.Second
)

// EmpresaQuiProvider searches CNPJ by company name using empresaqui.com.br.
type EmpresaQuiProvider struct {
	client *http.Client
}

// NewEmpresaQui creates a new EmpresaQuiProvider.
func NewEmpresaQui() *EmpresaQuiProvider {
	return &EmpresaQuiProvider{client: &http.Client{Timeout: empresaQuiTimeout}}
}

func (p *EmpresaQuiProvider) Name() string { return "empresaqui" }

// SearchByName searches empresaqui.com.br for a company and returns the best-matching CNPJ.
// The search term combines name + city to narrow results.
func (p *EmpresaQuiProvider) SearchByName(ctx context.Context, name, city string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, empresaQuiTimeout)
	defer cancel()

	// Build URL-encoded query — empresaqui uses path-based search.
	term := name
	if city != "" {
		term = name + " " + city
	}
	encoded := url.PathEscape(strings.TrimSpace(term))
	reqURL := fmt.Sprintf("%s/busca/%s", empresaQuiBase, encoded)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("empresaqui: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("empresaqui: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return "", domain.ErrNotFound
	case http.StatusTooManyRequests:
		return "", domain.ErrRateLimited
	case http.StatusOK:
		// ok
	default:
		return "", fmt.Errorf("empresaqui: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("empresaqui: read body: %w", err)
	}

	cnpj := parseEmpresaQuiHTML(string(body), name)
	if cnpj == "" {
		return "", domain.ErrNoResults
	}
	return cnpj, nil
}

// empresaqui result URLs contain the 14-digit CNPJ: /cnpj/12345678000190/empresa-name
var reEmpresaQuiCNPJURL = regexp.MustCompile(`/cnpj/(\d{14})/`)

// Formatted CNPJ in body text: XX.XXX.XXX/XXXX-XX
var reEmpresaQuiCNPJText = regexp.MustCompile(`\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}`)

// Company anchor text — used to score match quality.
var reEmpresaQuiName = regexp.MustCompile(`(?i)<a[^>]+href="/cnpj/(\d{14})/[^"]*"[^>]*>([^<]+)</a>`)

// reEmpresaQuiNonAlnum strips non-alphanumeric characters for word normalization.
var reEmpresaQuiNonAlnum = regexp.MustCompile(`[^a-z0-9]`)

func parseEmpresaQuiHTML(body, searchName string) string {
	// Strategy 1: find company name anchors and pick the one whose text best matches the query.
	nameMatches := reEmpresaQuiName.FindAllStringSubmatch(body, 20)
	searchWords := significantWords(searchName)
	bestCNPJ := ""
	bestScore := 0

	for _, m := range nameMatches {
		cnpjDigits := sanitizeCNPJ(m[1])
		companyName := strings.ToLower(strings.TrimSpace(m[2]))
		score := matchScore(companyName, searchWords)
		if score > bestScore {
			bestScore = score
			bestCNPJ = cnpjDigits
		}
	}
	if bestCNPJ != "" && len(bestCNPJ) == 14 {
		return bestCNPJ
	}

	// Strategy 2: take the first CNPJ from a URL path.
	if m := reEmpresaQuiCNPJURL.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}

	// Strategy 3: first formatted CNPJ in body text.
	if m := reEmpresaQuiCNPJText.FindString(body); m != "" {
		return sanitizeCNPJ(m)
	}

	return ""
}

// significantWords returns words longer than 2 characters, lowercase, stripped of stop words.
func significantWords(name string) []string {
	stop := map[string]bool{
		"de": true, "da": true, "do": true, "dos": true, "das": true,
		"e": true, "em": true, "a": true, "o": true, "os": true, "as": true,
		"ltda": true, "me": true, "epp": true, "eireli": true, "sa": true,
	}
	var words []string
	for _, w := range strings.Fields(strings.ToLower(name)) {
		w = reEmpresaQuiNonAlnum.ReplaceAllString(w, "")
		if len(w) > 2 && !stop[w] {
			words = append(words, w)
		}
	}
	return words
}

// matchScore counts how many significant search words appear in the candidate name.
func matchScore(candidate string, searchWords []string) int {
	score := 0
	for _, w := range searchWords {
		if strings.Contains(candidate, w) {
			score++
		}
	}
	return score
}
