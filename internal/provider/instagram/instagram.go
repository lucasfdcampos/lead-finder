// Package instagram provides Instagram handle discovery and profile validation.
package instagram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	igValidateTimeout  = 10 * time.Second
	storynavigationAPI = "https://storynavigation.com/api/user/info?username="
	instaStoriesURL    = "https://insta-stories-viewer.com/api/user/%s"
)

// GeminiExtractor is a narrow interface so Provider does not depend on the full gemini package.
type GeminiExtractor interface {
	ExtractInstagramHandle(ctx context.Context, businessName string, snippets []string) (string, error)
}

// Provider implements domain.InstagramSearcher.
type Provider struct {
	searcher domain.WebSearcher
	gemini   GeminiExtractor
	client   *http.Client
}

func New(searcher domain.WebSearcher, gemini GeminiExtractor) *Provider {
	return &Provider{
		searcher: searcher,
		gemini:   gemini,
		client:   &http.Client{Timeout: igValidateTimeout},
	}
}

func (p *Provider) Name() string { return "instagram" }

func (p *Provider) FindHandle(ctx context.Context, businessName, location string) (string, error) {
	query := fmt.Sprintf(`"%s" site:instagram.com %s`, businessName, location)
	results, err := p.searcher.Search(ctx, query)
	if err == nil && len(results) > 0 {
		for _, r := range results {
			if h := extractHandleFromURL(r.URL); h != "" {
				return h, nil
			}
		}
		if p.gemini != nil {
			snippets := resultsToSnippets(results)
			handle, err := p.gemini.ExtractInstagramHandle(ctx, businessName, snippets)
			if err == nil && handle != "" && handle != "NOT_FOUND" {
				return handle, nil
			}
		}
	}

	variations := GenerateHandleVariations(businessName)
	for _, candidate := range variations {
		_, ok, err := p.ValidateProfile(ctx, candidate)
		if err == nil && ok {
			return candidate, nil
		}
	}

	return "", domain.ErrNotFound
}

func (p *Provider) ValidateProfile(ctx context.Context, handle string) (int, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, igValidateTimeout)
	defer cancel()

	if followers, ok, err := p.validateViaStorynavigation(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	if followers, ok, err := p.validateViaInstaStoriesViewer(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	return 0, false, fmt.Errorf("instagram: profile not found for @%s", handle)
}

func (p *Provider) validateViaStorynavigation(ctx context.Context, handle string) (int, bool, error) {
	url := storynavigationAPI + handle
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("storynavigation: status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	return parseFollowersFromJSON(string(body))
}

func (p *Provider) validateViaInstaStoriesViewer(ctx context.Context, handle string) (int, bool, error) {
	url := fmt.Sprintf(instaStoriesURL, handle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("instastoriesviewer: status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	return parseFollowersFromJSON(string(body))
}

var stopWords = []string{
	"ltda", "eireli", "epp", "me", "sa", "cia", "companhia", "grupo",
	"loja", "lojas", "comercio", "comercial", "boutique",
	"industria", "industrias", "confeccoes", "vestuario", "brasil",
}

// GenerateHandleVariations applies heuristics to produce Instagram handle candidates.
func GenerateHandleVariations(name string) []string {
	clean := normalizeString(name)
	clean = removeLegalSuffixes(clean)
	words := splitWords(clean)
	words = removeStopWords(words)

	if len(words) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var variants []string

	add := func(s string) {
		s = sanitizeHandle(s)
		if s != "" && !seen[s] {
			seen[s] = true
			variants = append(variants, s)
		}
	}

	add(strings.Join(words, ""))
	add(strings.Join(words, "_"))
	add(strings.Join(words, "."))
	if len(words) >= 2 {
		add(strings.Join(words[:2], ""))
		add(strings.Join(words[:2], "_"))
		add(strings.Join(words[:2], "."))
	}
	add(words[0])
	if len(words) >= 3 {
		add(words[0] + words[len(words)-1])
		add(words[0] + "_" + words[len(words)-1])
	}

	return variants
}

var (
	reIGPath              = regexp.MustCompile(`(?i)instagram\.com/([A-Za-z0-9._]{1,30})/?`)
	reNonAlphanumUnderscore = regexp.MustCompile(`[^a-z0-9_.]`)
	reMultipleSeps        = regexp.MustCompile(`[_.]{2,}`)
)

func extractHandleFromURL(rawURL string) string {
	m := reIGPath.FindStringSubmatch(rawURL)
	if len(m) < 2 {
		return ""
	}
	h := strings.ToLower(m[1])
	for _, skip := range []string{"p", "explore", "reel", "reels", "stories", "accounts"} {
		if h == skip {
			return ""
		}
	}
	return h
}

func resultsToSnippets(results []domain.SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, fmt.Sprintf("URL: %s\nTitle: %s\nSnippet: %s", r.URL, r.Title, r.Snippet))
	}
	return out
}

func sanitizeHandle(s string) string {
	s = strings.ToLower(s)
	s = reNonAlphanumUnderscore.ReplaceAllString(s, "")
	s = reMultipleSeps.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_.")
	if len(s) < 2 || len(s) > 30 {
		return ""
	}
	return s
}

func normalizeString(s string) string {
	s = strings.ToLower(s)
	replacer := strings.NewReplacer(
		"a\u0301", "a", "a\u0300", "a", "a\u00e2", "a", "a\u00e3", "a",
		"e\u0301", "e", "e\u00ea", "e",
		"i\u0301", "i", "i\u00ee", "i",
		"o\u0301", "o", "o\u00f4", "o", "o\u00f5", "o",
		"u\u0301", "u", "u\u00fa", "u",
		"\u00e1", "a", "\u00e0", "a", "\u00e2", "a", "\u00e3", "a", "\u00e4", "a",
		"\u00e9", "e", "\u00e8", "e", "\u00ea", "e",
		"\u00ed", "i", "\u00ee", "i",
		"\u00f3", "o", "\u00f4", "o", "\u00f5", "o",
		"\u00fa", "u", "\u00fb", "u",
		"\u00e7", "c",
	)
	return replacer.Replace(s)
}

func removeLegalSuffixes(s string) string {
	patterns := []string{
		`\bltda\.?\b`, `\beireli\.?\b`, `\bepp\.?\b`,
		`\bs/?a\.?\b`, `\bme\.?\b`, `\bcia\.?\b`,
		`\bcompanhia\b`, `\bgrupo\b`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(`(?i)` + p)
		s = re.ReplaceAllString(s, " ")
	}
	return strings.TrimSpace(s)
}

func splitWords(s string) []string {
	re := regexp.MustCompile(`[^a-z0-9]+`)
	parts := re.Split(s, -1)
	var words []string
	for _, p := range parts {
		if p != "" {
			words = append(words, p)
		}
	}
	return words
}

func removeStopWords(words []string) []string {
	stopSet := make(map[string]bool, len(stopWords))
	for _, w := range stopWords {
		stopSet[w] = true
	}
	var out []string
	for _, w := range words {
		if !stopSet[w] {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return words
	}
	return out
}

func parseFollowersFromJSON(body string) (int, bool, error) {
	re1 := regexp.MustCompile(`"count"\s*:\s*([0-9]+)`)
	re2 := regexp.MustCompile(`"followers"\s*:\s*([0-9]+)`)
	re3 := regexp.MustCompile(`"follower_count"\s*:\s*([0-9]+)`)

	for _, re := range []*regexp.Regexp{re1, re2, re3} {
		if m := re.FindStringSubmatch(body); len(m) > 1 {
			var count int
			fmt.Sscanf(m[1], "%d", &count)
			return count, true, nil
		}
	}

	if strings.Contains(body, `"username"`) || strings.Contains(body, `"pk"`) {
		return 0, true, nil
	}

	return 0, false, fmt.Errorf("instagram: followers not found")
}
