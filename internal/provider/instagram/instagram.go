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
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	igValidateTimeout   = 12 * time.Second
	igSearchTimeout     = 8 * time.Second
	instaStoriesBaseURL = "https://insta-stories-viewer.com/"
	storynavBaseURL     = "https://storynavigation.com/user/"
	instagramDirectURL  = "https://www.instagram.com/"
	instagramAPIURL     = "https://www.instagram.com/api/v1/users/web_profile_info/" // internal JSON endpoint
	picukiBaseURL       = "https://picuki.com/profile/"                              // third-party viewer
	ddgHTMLBase         = "https://html.duckduckgo.com/html/"
	igAppID             = "936619743392459" // public app-id used by instagram.com web client
)

// Provider implements domain.InstagramSearcher.
type Provider struct {
	searcher domain.WebSearcher // optional; uses DDG HTML when nil
	client   *http.Client
}

func New(searcher domain.WebSearcher) *Provider {
	return &Provider{
		searcher: searcher,
		client:   &http.Client{Timeout: igValidateTimeout},
	}
}

func (p *Provider) Name() string { return "instagram" }

func (p *Provider) FindHandle(ctx context.Context, businessName, location string) (string, error) {
	city := extractCity(location)
	short := igShortName(businessName)

	// Step 0: exact full-name search — "<businessName>" instagram "<city>".
	// Quoted name maximises precision; best signal when the profile name matches exactly.
	if p.searcher != nil {
		query := fmt.Sprintf(`"%s" instagram "%s"`, businessName, city)
		if h := p.validateSearchResults(ctx, query, businessName, city); h != "" {
			return h, nil
		}
	}

	// Step 0b: site:instagram.com with full quoted name.
	if p.searcher != nil {
		query := fmt.Sprintf(`site:instagram.com "%s" "%s"`, businessName, city)
		if h := p.validateSearchResults(ctx, query, businessName, city); h != "" {
			return h, nil
		}
	}

	// Step 1: site:instagram.com with abbreviated name (unquoted) — Google flexibility.
	if p.searcher != nil {
		query := fmt.Sprintf(`site:instagram.com %s "%s"`, short, city)
		if h := p.validateSearchResults(ctx, query, businessName, city); h != "" {
			return h, nil
		}
	}

	// Step 1b: broader query without site: restriction — picks up Linktree/GMB pages.
	if p.searcher != nil {
		query := fmt.Sprintf(`%s instagram "%s"`, short, city)
		if h := p.validateSearchResults(ctx, query, businessName, city); h != "" {
			return h, nil
		}
	}

	// Step 2: DuckDuckGo HTML direct — free, no quota, validate every candidate.
	for _, h := range p.searchDDGHTMLCandidates(ctx, short, city) {
		if handleMatchesName(h, businessName, city) {
			return h, nil
		}
		validCtx, vcancel := context.WithTimeout(ctx, 6*time.Second)
		_, ok, verr := p.ValidateProfileWithHints(validCtx, h, city, "")
		vcancel()
		if verr == nil && ok {
			return h, nil
		}
	}

	// Step 3: heuristic handle variations + profile validation (no web search needed).
	// Pass city so patterns like "hamburgueriaholambra" / "hamburgueriasp" are tried.
	variations := GenerateHandleVariations(businessName, city)
	for _, candidate := range variations {
		_, ok, err := p.ValidateProfile(ctx, candidate)
		if err == nil && ok {
			return candidate, nil
		}
	}

	return "", domain.ErrNotFound
}

// validateSearchResults runs a web search and validates each Instagram handle candidate.
// Returns the first handle that passes name-similarity fast-accept or profile validation.
// At most maxValidate candidates are checked via HTTP to avoid burning the time budget.
func (p *Provider) validateSearchResults(ctx context.Context, query, businessName, city string) string {
	searchCtx, cancel := context.WithTimeout(ctx, igSearchTimeout)
	results, err := p.searcher.Search(searchCtx, query)
	cancel()
	if err != nil {
		return ""
	}
	const maxValidate = 2 // max HTTP validation calls per query step
	validated := 0
	for _, r := range results {
		h := extractHandleFromURL(r.URL)
		if h == "" {
			continue
		}
		// Fast-accept: no HTTP call needed.
		if handleMatchesName(h, businessName, city) {
			return h
		}
		// HTTP validation — capped at maxValidate to preserve time budget.
		if validated >= maxValidate {
			continue
		}
		validated++
		validCtx, vcancel := context.WithTimeout(ctx, 6*time.Second)
		_, ok, verr := p.ValidateProfileWithHints(validCtx, h, city, "")
		vcancel()
		if verr == nil && ok {
			return h
		}
	}
	return ""
}

// searchDDGHTMLCandidates queries html.duckduckgo.com for instagram.com links.
// Returns all candidate handles found (caller is responsible for validation).
func (p *Provider) searchDDGHTMLCandidates(ctx context.Context, shortName, city string) []string {
	searchCtx, cancel := context.WithTimeout(ctx, igSearchTimeout)
	defer cancel()

	query := fmt.Sprintf(`site:instagram.com "%s" %s`, shortName, city)

	form := url.Values{}
	form.Set("q", query)
	form.Set("kl", "br-pt")

	req, err := http.NewRequestWithContext(searchCtx, http.MethodPost, ddgHTMLBase,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://html.duckduckgo.com/")

	// Small random delay to reduce bot fingerprinting.
	time.Sleep(time.Duration(rand.Intn(400)+100) * time.Millisecond)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	return extractInstagramHandlesFromDDGHTML(string(body))
}

// reDDGResultLink matches result anchor hrefs in DDG HTML (uddg redirect).
var reDDGResultLink = regexp.MustCompile(`href="(?:https:)?//duckduckgo\.com/l/\?uddg=([^"&]+)`)

// igShortName returns a short, normalised version of a business name for use in
// Instagram search queries: strips legal suffixes, stop words, and limits to 3 words.
// Example: "LUCAS G. TEREBEYCZIK - CONFECCOES LTDA" → "lucas terebeyczik confeccoes"
func igShortName(name string) string {
	clean := removeLegalSuffixes(normalizeString(name))
	words := removeStopWords(splitWords(clean))
	if len(words) == 0 {
		return normalizeString(name)
	}
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, " ")
}

// handleMatchesName returns true when the Instagram handle shares enough key words
// with the business name (or city) to be a high-confidence match without an HTTP call.
func handleMatchesName(handle, businessName, city string) bool {
	h := normalizeString(handle)
	words := removeStopWords(splitWords(normalizeString(businessName)))
	cityNorm := normalizeString(city)

	matched := 0
	for _, w := range words {
		if len(w) >= 4 && strings.Contains(h, w) {
			matched++
		}
	}
	// 2+ significant words match — high confidence.
	if matched >= 2 {
		return true
	}
	// 1 word + city also strong enough.
	if matched >= 1 && cityNorm != "" && strings.Contains(h, cityNorm) {
		return true
	}
	return false
}

// extractInstagramHandlesFromDDGHTML parses a DDG HTML page and returns all
// distinct instagram.com handles found in the result links (up to 10).
func extractInstagramHandlesFromDDGHTML(body string) []string {
	seen := make(map[string]bool)
	var handles []string
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			handles = append(handles, h)
		}
	}
	for _, m := range reDDGResultLink.FindAllStringSubmatch(body, 30) {
		rawURL, err := url.QueryUnescape(html.UnescapeString(m[1]))
		if err != nil {
			continue
		}
		add(extractHandleFromURL(rawURL))
	}
	for _, raw := range reInstagramDirectLink.FindAllString(body, 30) {
		add(extractHandleFromURL(raw))
	}
	if len(handles) > 10 {
		return handles[:10]
	}
	return handles
}

var reInstagramDirectLink = regexp.MustCompile(`https://(?:www\.)?instagram\.com/[^"&\s<]{2,30}`)

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

	// 1. Instagram internal JSON API — clean JSON, no scraping.
	if followers, bio, ok, err := p.scrapeInstagramAPI(ctx, handle); err == nil && ok {
		if city == "" || bioMatchesHints(bio, city, phone) {
			return followers, true, nil
		}
		return followers, false, fmt.Errorf("instagram: bio does not match hints for @%s", handle)
	}

	// 2. insta-stories-viewer.com
	if followers, bio, ok, err := p.scrapeInstaStoriesViewer(ctx, handle); err == nil && ok {
		if city == "" || bioMatchesHints(bio, city, phone) {
			return followers, true, nil
		}
		return followers, false, fmt.Errorf("instagram: bio does not match hints for @%s", handle)
	}

	// 3. picuki.com — independent viewer, reliable follower counts.
	if followers, bio, ok, err := p.scrapePicuki(ctx, handle); err == nil && ok {
		if city == "" || bioMatchesHints(bio, city, phone) {
			return followers, true, nil
		}
		return followers, false, fmt.Errorf("instagram: bio does not match hints for @%s", handle)
	}

	// 4. storynavigation.com
	if followers, _, ok, err := p.scrapeStorynavigation(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	// 5. instagram.com direct
	if followers, ok, err := p.scrapeInstagramDirect(ctx, handle); err == nil && ok {
		return followers, true, nil
	}

	return 0, false, fmt.Errorf("instagram: profile not found for @%s", handle)
}

// ─── scraper: Instagram internal JSON API ────────────────────────────────────
//
// GET /api/v1/users/web_profile_info/?username=<handle> with the public app-id
// header returns clean JSON with follower count + biography for public profiles.

type igAPIResponse struct {
	Data struct {
		User struct {
			Username       string `json:"username"`
			Biography      string `json:"biography"`
			EdgeFollowedBy struct {
				Count int `json:"count"`
			} `json:"edge_followed_by"`
		} `json:"user"`
	} `json:"data"`
}

var reIGAPIFollowersFallback = regexp.MustCompile(`"follower_count"\s*:\s*(\d+)`)

func (p *Provider) scrapeInstagramAPI(ctx context.Context, handle string) (followers int, bio string, ok bool, err error) {
	reqURL := instagramAPIURL + "?username=" + handle
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, "", false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("x-ig-app-id", igAppID)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.instagram.com/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, doErr := p.client.Do(req)
	if doErr != nil {
		return 0, "", false, doErr
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, "", false, domain.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", false, fmt.Errorf("instagram api: status %d", resp.StatusCode)
	}

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return 0, "", false, readErr
	}
	bodyStr := string(body)

	// Preferred: unmarshal typed response.
	var apiResp igAPIResponse
	if jsonErr := json.Unmarshal(body, &apiResp); jsonErr == nil && apiResp.Data.User.Username != "" {
		user := apiResp.Data.User
		if !strings.EqualFold(user.Username, handle) {
			return 0, "", false, fmt.Errorf("instagram api: username mismatch")
		}
		return user.EdgeFollowedBy.Count, user.Biography, true, nil
	}

	// Fallback: regex on raw JSON.
	if m := reISVFollowersJSON.FindStringSubmatch(bodyStr); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &followers)
	} else if m := reIGAPIFollowersFallback.FindStringSubmatch(bodyStr); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &followers)
	}
	ok = strings.Contains(bodyStr, `"username":"`+handle+`"`)
	return followers, "", ok, nil
}

// ─── scraper: picuki.com ─────────────────────────────────────────────────────
//
// picuki.com/profile/<handle> — popular Instagram viewer that exposes
// follower counts and bio text in plain HTML without requiring authentication.

var (
	rePicukiFollowers     = regexp.MustCompile(`(?i)(\d[\d.,]*)\s*(?:[Ff]ollowers|[Ss]eguidores)`)
	rePicukiFollowersSpan = regexp.MustCompile(`(?i)class="[^"]*followers[^"]*"[^>]*>[^<]*(\d[\d.,]*)`)
	rePicukiBio           = regexp.MustCompile(`(?i)class="[^"]*biography[^"]*"[^>]*>([^<]{5,300})`)
	rePicukiBioMeta       = regexp.MustCompile(`(?i)<meta[^>]+name="description"[^>]+content="([^"]{10,400})"`)
)

func (p *Provider) scrapePicuki(ctx context.Context, handle string) (followers int, bio string, ok bool, err error) {
	pageURL := picukiBaseURL + handle
	body, fetchErr := p.fetchHTML(ctx, pageURL, map[string]string{
		"Referer": "https://picuki.com/",
	})
	if fetchErr != nil {
		return 0, "", false, fetchErr
	}

	if m := rePicukiFollowersSpan.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	} else if m := rePicukiFollowers.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	}

	if m := rePicukiBio.FindStringSubmatch(body); len(m) > 1 {
		bio = strings.TrimSpace(m[1])
	} else if m := rePicukiBioMeta.FindStringSubmatch(body); len(m) > 1 {
		bio = strings.TrimSpace(m[1])
	}

	ok = strings.Contains(body, handle) || followers > 0
	return followers, bio, ok, nil
}

// ─── scraper: insta-stories-viewer.com ───────────────────────────────────────
//
// The page at https://insta-stories-viewer.com/<handle> renders profile info
// inside HTML. We extract:
//   - follower count  (text like "1.234 seguidores" / "1,234 followers" / raw JSON)
//   - bio text        (used for city/phone confirmation)

var (
	reISVFollowers     = regexp.MustCompile(`(?i)(\d[\d.,\s]*)\s*(?:seguidores|followers)`)
	reISVFollowersJSON = regexp.MustCompile(`"edge_followed_by"\s*:\s*\{\s*"count"\s*:\s*(\d+)`)
	// Extra patterns for updated 2025 HTML where the label structure changed.
	reISVFollowersAlt  = regexp.MustCompile(`(?i)follower[^<]{0,40}?(\d[\d.,]+)`)
	reISVFollowersData = regexp.MustCompile(`(?i)data-followers[="'\s]+(\d+)`)
	reISVBio           = regexp.MustCompile(`(?i)(?:biography|bio)["\s:>]+([^<"]{5,300})`)
	reISVBioMeta       = regexp.MustCompile(`(?i)<meta[^>]+name="description"[^>]+content="([^"]{10,400})"`)
	reFollowerNum      = regexp.MustCompile(`[\d.,\s]+`)
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
	} else if m := reISVFollowersAlt.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(cleanNumber(m[1]), "%d", &followers)
	} else if m := reISVFollowersData.FindStringSubmatch(body); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &followers)
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
	reIGDirectFollowers    = regexp.MustCompile(`"edge_followed_by"\s*:\s*\{\s*"count"\s*:\s*(\d+)`)
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
		digitsBio := reDigits.ReplaceAllString(bio, "")
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
// An optional city string adds localised variants (e.g. "hamburgueriaholambra").
func GenerateHandleVariations(name string, cityArgs ...string) []string {
	city := ""
	if len(cityArgs) > 0 {
		city = normalizeString(cityArgs[0])
		// strip accents and keep only alphanum
		city = regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(city, "")
	}

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

	base := strings.Join(words, "")

	// Name-only variants (most common).
	add(base)                     // lafemme
	add(strings.Join(words, "_")) // la_femme
	add(strings.Join(words, ".")) // la.femme
	if len(words) >= 2 {
		base2 := strings.Join(words[:2], "")
		add(base2)
		add(strings.Join(words[:2], "_"))
		add(strings.Join(words[:2], "."))
	}
	add(words[0]) // la
	if len(words) >= 3 {
		add(words[0] + words[len(words)-1])
		add(words[0] + "_" + words[len(words)-1])
	}
	// word pairs with dots/underscores — "la.femme.boutique"
	if len(words) >= 3 {
		add(strings.Join(words, "."))
		add(words[0] + "." + words[len(words)-1])
	}

	// City-suffixed variants — very common for Brazilian local businesses.
	if city != "" {
		// Full city: "lafemmearapongas", "lafemme_arapongas"
		add(base + city)
		add(base + "_" + city)
		add(base + "." + city)
		if len(words) >= 2 {
			base2 := strings.Join(words[:2], "")
			add(base2 + city)
			add(base2 + "_" + city)
		}
		add(words[0] + city)
		add(words[0] + "_" + city)

		// Short city (first 3 chars): "lafemmear", "lafemme_ar", "lafemme.ar"
		if len(city) >= 3 {
			shortCity3 := city[:3]
			add(base + shortCity3)
			add(base + "_" + shortCity3)
			add(base + "." + shortCity3)
			add(words[0] + shortCity3)
		}
		// Short city (first 4 chars): "lafemmearap"
		if len(city) >= 4 {
			shortCity4 := city[:4]
			add(base + shortCity4)
			add(base + "_" + shortCity4)
		}

		// City-prefix variants: "arapongas.lafemme", "arapongas_lafemme"
		add(city + base)
		add(city + "_" + base)
		add(city + words[0])
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

// extractCity returns the city portion from a "City-ST" or "City, ST" location string.
func extractCity(location string) string {
	location = strings.TrimSpace(location)
	for _, sep := range []string{"-", ","} {
		if idx := strings.LastIndex(location, sep); idx > 0 {
			return strings.TrimSpace(location[:idx])
		}
	}
	return location
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

// cleanNumber normalises follower/likes strings to a plain integer string.
// It handles:
//   - Thousand separators: "12.345" or "12,345" → "12345"
//   - K suffix:  "12.3K" or "12K"  → "12300" / "12000"
//   - M suffix:  "1.2M"            → "1200000"
//   - B suffix:  "1.1B"            → "1100000000"
func cleanNumber(s string) string {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)

	// Detect multiplier suffix.
	multiplier := 1
	switch {
	case strings.HasSuffix(upper, "K"):
		multiplier = 1_000
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "M"):
		multiplier = 1_000_000
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "B"):
		multiplier = 1_000_000_000
		s = s[:len(s)-1]
	}

	// Parse decimal part if present (e.g. "12.3" from "12.3K").
	if multiplier > 1 {
		s = strings.ReplaceAll(s, ",", ".")
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
			return strconv.Itoa(int(f * float64(multiplier)))
		}
	}

	// Plain numeric string: strip thousand separators.
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
