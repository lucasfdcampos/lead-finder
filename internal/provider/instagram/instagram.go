// Package instagram provides Instagram handle discovery and profile validation.
//
// Validation cascade (for each handle candidate):
//  1. insta-stories-viewer.com/<handle>  — scrapes the HTML profile page, extracts
//     followers count and bio text; uses bio/city/phone as confirmation signals
//  2. storynavigation.com/user/<handle>  — fallback HTML page scraper
//  3. instagram.com/<handle>             — last-resort direct scrape (no auth)
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
	igValidateTimeout    = 12 * time.Second
	instaStoriesBaseURL  = "https://insta-stories-viewer.com/"
	storynavBaseURL      = "https://storynavigation.com/user/"
	instagramDirectURL   = "https://www.instagram.com/"
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
			handle, gErr := p.gemini.ExtractInstagramHandle(ctx, businessName, snippets)
			if gErr == nil && handle != "" && handle != "NOT_FOUND" {
				return handle, nil
			}
		}
	}

	// Heuristic variations + validation
	variations := GenerateHandleVariations(businessName)
	for _, candidate := range variations {
		_, ok, err := p.ValidateProfile(ctx, candidate)
		if err == nil && ok {
			return candidate, nil
		}
	}

	return "", domain.ErrNotFound
}

// ValidateProfile checks if an Instagram handle exists and returns (followers, ok, err).
// It also receives optional confirmation hints (city, phone) for bio matching.
func (p *Provider) ValidateProfile(ctx context.Context, handle string) (int, bool, error) {
	return p.ValidateProfileWithHints(ctx, handle, "", "")
}

// ValidateProfileWithHints is like ValidateProfile but uses city/phone to
// confirm the bio actually belongs to the right business.
func (p *Provider) ValidateProfileWithHints(ctx context.Context, handle, city, phone string) (int, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, igValidateTimeout)
	defer cancel()

	// 1. insta-stories-viewer.com
	if followers, bio, ok, err := p.scrapeInstaStoriesViewer(ctx, handle); err == nil && ok {
		if city == "" || bioMatchesHints(bio, city, phone) {
			return followers, true, nil
		}
		// Profile found but bio doesn't match — return followers=0 as signal to caller
		// that it exists but may be the wrong account.
		return followers, false, fmt.Errorf("instagram: bio does not match hints for @%s", handle)
	}

	// 2. storynavigation.com
	if followers, _, ok, err := p.scrapeStorynavigation(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	// 3. instagram.com direct
	if followers, ok, err := p.scrapeInstagramDirect(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	return 0, false, fmt.Errorf("instagram: profile not found for @%s", handle)
}

// ─── scraper: insta-stories-viewer.com ───────────────────────────────────────
//
// The page at https://insta-stories-viewer.com/<handle> renders profile info
// inside HTML. We extract:
//   - follower count  (text like "1.234 seguidores" / "1,234 followers" / raw JSON)
//   - bio text        (used for city/phone confirmation)

var (
	reISVFollowers = regexp.MustCompile(`(?i)(\d[\d.,\s]*)\s*(?:seguidores|followers)`)
	reISVFollowersJSON = regexp.MustCompile(`"edge_followed_by"\s*:\s*\{\s*"count"\s*:\s*(\d+)`)
	reISVBio       = regexp.MustCompile(`(?i)(?:biography|bio)["\s:>]+([^<"]{5,300})`)
	reISVBioMeta   = regexp.MustCompile(`(?i)<meta[^>]+name="description"[^>]+content="([^"]{10,400})"`)
	reFollowerNum  = regexp.MustCompile(`[\d.,\s]+`)
)

func (p *Provider) scrapeInstaStoriesViewer(ctx context.Context, handle string) (followers int, bio string, ok bool, err error) {
	pageURL := instaStoriesBaseURL + handle
	body, statusErr := p.fetchHTML(ctx, pageURL, map[string]string{
		"Referer": "https://insta-stories-viewer.com/",
	})
	if statusErr != nil {
		return 0, "", false, statusErr
	}

	// Try JSON-embedded follower count first (most reliable)
	if m := reISVFollowersJSON.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	} else if m := reISVFollowers.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	}

	// Extract bio
	if m := reISVBio.FindStringSubmatch(body); len(m) > 1 {
		bio = strings.TrimSpace(m[1])
	} else if m := reISVBioMeta.FindStringSubmatch(body); len(m) > 1 {
		bio = strings.TrimSpace(m[1])
	}

	// Even followers=0 is ok if the page loaded; absence of profile yields 404/redirect
	ok = strings.Contains(body, handle) || followers > 0
	return followers, bio, ok, nil
}

// ─── scraper: storynavigation.com ────────────────────────────────────────────
//
// The page at https://storynavigation.com/user/<handle> also renders profile HTML.

var (
	reSNFollowers     = regexp.MustCompile(`(?i)(\d[\d.,]*)\s*(?:Followers|Seguidores)`)
	reSNFollowersJSON = regexp.MustCompile(`"follower_count"\s*:\s*(\d+)`)
	reSNBio           = regexp.MustCompile(`(?i)class="[^"]*bio[^"]*"[^>]*>([^<]{5,300})`)
)

func (p *Provider) scrapeStorynavigation(ctx context.Context, handle string) (followers int, bio string, ok bool, err error) {
	pageURL := storynavBaseURL + handle
	body, fetchErr := p.fetchHTML(ctx, pageURL, map[string]string{
		"Referer": "https://storynavigation.com/",
	})
	if fetchErr != nil {
		return 0, "", false, fetchErr
	}

	if m := reSNFollowersJSON.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &followers)
	} else if m := reSNFollowers.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	}

	if m := reSNBio.FindStringSubmatch(body); len(m) > 1 {
		bio = strings.TrimSpace(m[1])
	}

	ok = strings.Contains(body, handle) || followers > 0
	return followers, bio, ok, nil
}

// ─── scraper: instagram.com direct ───────────────────────────────────────────

var (
	reIGDirectFollowers = regexp.MustCompile(`"edge_followed_by"\s*:\s*\{\s*"count"\s*:\s*(\d+)`)
	reIGDirectFollowersAlt = regexp.MustCompile(`"follower_count"\s*:\s*(\d+)`)
)

func (p *Provider) scrapeInstagramDirect(ctx context.Context, handle string) (int, bool, error) {
	pageURL := instagramDirectURL + handle + "/"
	body, err := p.fetchHTML(ctx, pageURL, nil)
	if err != nil {
		return 0, false, err
	}

	for _, re := range []*regexp.Regexp{reIGDirectFollowers, reIGDirectFollowersAlt} {
		if m := re.FindStringSubmatch(body); len(m) > 1 {
			var count int
			fmt.Sscanf(m[1], "%d", &count)
			return count, true, nil
		}
	}

	// If page returned 200 and mentions the handle, profile exists (followers unknown)
	if strings.Contains(body, `"username":"`+handle+`"`) {
		return 0, true, nil
	}
	return 0, false, fmt.Errorf("instagram: direct scrape found no data for @%s", handle)
}

// ─── bio confirmation ─────────────────────────────────────────────────────────

// bioMatchesHints returns true when the bio text contains evidence matching
// city or phone, confirming the profile belongs to the expected business.
func bioMatchesHints(bio, city, phone string) bool {
	if bio == "" {
		return true // no bio to check — assume ok
	}
	bioLow := strings.ToLower(bio)
	if city != "" && strings.Contains(bioLow, strings.ToLower(city)) {
		return true
	}
	if phone != "" {
		// Compare last 8 digits to avoid country-code mismatches
		digitsPhone := reDigits.ReplaceAllString(phone, "")
		digitsBio   := reDigits.ReplaceAllString(bio, "")
		if len(digitsPhone) >= 8 && strings.Contains(digitsBio, digitsPhone[len(digitsPhone)-8:]) {
			return true
		}
	}
	return false
}

// ─── HTTP helper ──────────────────────────────────────────────────────────────

func (p *Provider) fetchHTML(ctx context.Context, rawURL string, extraHeaders map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", domain.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("instagram scraper: status %d for %s", resp.StatusCode, rawURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ─── utilities ────────────────────────────────────────────────────────────────

var stopWords = []string{
	"ltda", "eireli", "epp", "me", "sa", "cia", "companhia", "grupo",
	"loja", "lojas", "comercio", "comercial", "boutique",
	"industria", "industrias", "confeccoes", "vestuario", "brasil",
}

var reDigits = regexp.MustCompile(`\D`)

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
	reIGPath                = regexp.MustCompile(`(?i)instagram\.com/([A-Za-z0-9._]{1,30})/?`)
	reNonAlphanumUnderscore = regexp.MustCompile(`[^a-z0-9_.]`)
	reMultipleSeps          = regexp.MustCompile(`[_.]{2,}`)
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
	for _, pat := range patterns {
		re := regexp.MustCompile(`(?i)` + pat)
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

func cleanNumber(s string) string {
	// Remove dots and commas used as thousand separators, keep only digits
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
