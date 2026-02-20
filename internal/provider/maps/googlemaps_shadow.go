// Package maps provides the GoogleMapsShadowScraper, which uses DuckDuckGo to
// discover real business names indexed by Google Maps without requiring any API key.
//
// Strategy:
//  1. Build DDG query: "site:google.com/maps/place <query> <location>"
//  2. POST to DDG HTML endpoint with increasing page offset (s=0, s=30, s=60, …)
//  3. Decode uddg= redirect params to recover real Google Maps URLs
//  4. Extract the place name from the /maps/place/<NAME>/ URL path
//  5. Stop when no new names are found or maxPages is reached
package maps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	ddgEndpoint     = "https://html.duckduckgo.com/html/"
	ddgMapsTimeout  = 12 * time.Second
	defaultMaxPages = 3
	pageSize        = 30
)

// GoogleMapsShadowScraper implements domain.BusinessDiscoverer.
type GoogleMapsShadowScraper struct {
	client   *http.Client
	maxPages int
}

// NewGoogleMapsShadowScraper creates a new scraper.
// maxPages controls pagination depth (0 uses the default of 3).
func NewGoogleMapsShadowScraper(maxPages int) *GoogleMapsShadowScraper {
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	return &GoogleMapsShadowScraper{
		client:   &http.Client{Timeout: ddgMapsTimeout},
		maxPages: maxPages,
	}
}

func (s *GoogleMapsShadowScraper) Name() string { return "googlemaps-shadow" }

// DiscoverBusinessNames queries DDG for site:google.com/maps/place results and
// returns deduplicated business name strings.
// Pagination stops early when a page yields no new names.
func (s *GoogleMapsShadowScraper) DiscoverBusinessNames(ctx context.Context, query, location string) ([]string, error) {
	ddgQuery := fmt.Sprintf("site:google.com/maps/place %s %s", query, location)

	var allNames []string
	seen := make(map[string]bool)

	for page := 0; page < s.maxPages; page++ {
		offset := page * pageSize

		pageCtx, cancel := context.WithTimeout(ctx, ddgMapsTimeout)
		names, err := s.fetchPage(pageCtx, ddgQuery, offset)
		cancel()

		if err != nil || len(names) == 0 {
			break
		}

		added := 0
		for _, name := range names {
			key := strings.ToLower(strings.TrimSpace(name))
			if key != "" && !seen[key] {
				seen[key] = true
				allNames = append(allNames, name)
				added++
			}
		}

		// No new results on this page — stop paginating early.
		if added == 0 {
			break
		}
	}

	if len(allNames) == 0 {
		return nil, domain.ErrNoResults
	}

	return allNames, nil
}

func (s *GoogleMapsShadowScraper) fetchPage(ctx context.Context, query string, offset int) ([]string, error) {
	formData := url.Values{}
	formData.Set("q", query)
	formData.Set("kl", "br-pt")
	if offset > 0 {
		formData.Set("s", strconv.Itoa(offset))
		formData.Set("dc", strconv.Itoa(offset))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ddgEndpoint,
		strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("googlemaps-shadow: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("googlemaps-shadow: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("googlemaps-shadow: read body: %w", err)
	}

	return parseGoogleMapsNames(string(body)), nil
}

// ─── HTML parsers ──────────────────────────────────────────────────────────────

var (
	// Captures the URL-encoded real destination inside DDG's redirect param.
	reUDDG = regexp.MustCompile(`uddg=([^&"#\s]+)`)

	// Captures the place slug after /maps/place/ in a decoded Google URL.
	reMapsPlace = regexp.MustCompile(`(?i)/maps/place/([^/@?&"#\s]{2,})`)

	// Captures DDG result link titles as fallback.
	reResultTitle = regexp.MustCompile(`(?i)<a[^>]+class="result__a"[^>]*>([^<]{3,120})</a>`)

	// Strips Google Maps branding from titles (e.g. "Loja X · Google Maps").
	reMapsTitle = regexp.MustCompile(`(?i)\s*[·\-]\s*(?:Google\s*Maps?|Mapas?)\s*$`)

	// Detects raw coordinate strings (not business names).
	reCoords = regexp.MustCompile(`^-?[0-9]{1,3}\.[0-9]`)
)

// htmlEnt decodes the most common HTML character entities found in DDG HTML.
var htmlEnt = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">",
	"&quot;", `"`, "&#39;", "'", "&nbsp;", " ",
)

func parseGoogleMapsNames(html string) []string {
	var names []string
	seen := make(map[string]bool)

	add := func(raw string) {
		name := strings.TrimSpace(raw)
		if !isValidBusinessName(name) {
			return
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}

	// ── Primary: decode uddg= param → real Google Maps URL → place slug ──────
	for _, m := range reUDDG.FindAllStringSubmatch(html, 300) {
		if len(m) < 2 {
			continue
		}
		decodedURL, err := url.QueryUnescape(m[1])
		if err != nil {
			continue
		}

		pm := reMapsPlace.FindStringSubmatch(decodedURL)
		if len(pm) < 2 {
			continue
		}

		// The slug uses + for space (form encoding) and %XX for other chars.
		slug := strings.ReplaceAll(pm[1], "+", " ")
		name, err := url.PathUnescape(slug)
		if err != nil {
			name = slug
		}
		// Trim anything after a coordinate separator.
		name = strings.SplitN(name, "@", 2)[0]
		name = strings.TrimRight(name, "/ ")
		add(name)
	}

	// ── Fallback: result link titles (if URL approach found nothing) ──────────
	if len(names) == 0 {
		for _, m := range reResultTitle.FindAllStringSubmatch(html, 100) {
			if len(m) < 2 {
				continue
			}
			title := htmlEnt.Replace(strings.TrimSpace(m[1]))
			title = reMapsTitle.ReplaceAllString(title, "")
			add(strings.TrimSpace(title))
		}
	}

	return names
}

// isValidBusinessName filters out coordinates, URLs and excessively short/long strings.
func isValidBusinessName(name string) bool {
	runes := []rune(name)
	if len(runes) < 3 || len(runes) > 120 {
		return false
	}
	if reCoords.MatchString(name) {
		return false
	}
	if strings.Contains(name, "http") || strings.Contains(name, "www.") {
		return false
	}
	return true
}
