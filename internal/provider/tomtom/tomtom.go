// Package tomtom provides a PlacesSearcher backed by the TomTom Fuzzy Search API
// (free tier: 2 500 requests/day = ~75 000/month — no credit card required).
//
// It uses the /search/2/search/{query}.json endpoint which performs smart
// category-aware fuzzy matching and returns POI data including phone and website.
package tomtom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

const (
	ttBaseURL  = "https://api.tomtom.com/search/2/search"
	ttTimeout  = 12 * time.Second
	ttMaxLimit = 50
)

// Provider implements domain.PlacesSearcher using the TomTom Fuzzy Search API.
type Provider struct {
	apiKey string
	client *http.Client
}

// New creates a TomTom PlacesSearcher.
func New(apiKey string) *Provider {
	return &Provider{
		apiKey: apiKey,
		client: &http.Client{Timeout: ttTimeout},
	}
}

func (p *Provider) Name() string { return "tomtom" }

// SearchPlaces queries TomTom for businesses matching query + optional location.
// latlon is "lat,lon" (may be empty — will use countrySet=BR only).
func (p *Provider) SearchPlaces(ctx context.Context, query, latlon string, radiusM int) ([]domain.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, ttTimeout)
	defer cancel()

	// TomTom Fuzzy Search: /search/2/search/{encodedQuery}.json
	reqURL := ttBaseURL + "/" + url.PathEscape(query) + ".json"

	params := url.Values{}
	params.Set("key", p.apiKey)
	params.Set("countrySet", "BR")
	params.Set("limit", strconv.Itoa(ttMaxLimit))
	params.Set("language", "pt-BR")
	params.Set("typeahead", "false")
	// Request POI category info in the response.
	params.Set("categorySet", "") // empty = all categories

	if latlon != "" {
		lat, lon, ok := parseLatLon(latlon)
		if ok {
			params.Set("lat", strconv.FormatFloat(lat, 'f', 6, 64))
			params.Set("lon", strconv.FormatFloat(lon, 'f', 6, 64))
			if radiusM > 0 {
				params.Set("radius", strconv.Itoa(radiusM))
			} else {
				params.Set("radius", "50000") // 50 km default for city-level searches
			}
		}
	}

	fullURL := reqURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("tomtom: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tomtom: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("tomtom: invalid API key (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tomtom: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tomtom: read body: %w", err)
	}

	var ttResp ttResponse
	if err := json.Unmarshal(body, &ttResp); err != nil {
		return nil, fmt.Errorf("tomtom: unmarshal: %w", err)
	}

	if len(ttResp.Results) == 0 {
		return nil, domain.ErrNoResults
	}

	var leads []domain.Lead
	for _, r := range ttResp.Results {
		if r.POI.Name == "" {
			continue
		}
		leads = append(leads, r.toLead())
	}
	if len(leads) == 0 {
		return nil, domain.ErrNoResults
	}
	return leads, nil
}

// ─── JSON response structs ────────────────────────────────────────────────────

type ttResponse struct {
	Results []ttResult `json:"results"`
}

type ttResult struct {
	Type     string     `json:"type"`
	POI      ttPOI      `json:"poi"`
	Address  ttAddress  `json:"address"`
	Position ttPosition `json:"position"`
}

type ttPOI struct {
	Name       string   `json:"name"`
	Phone      string   `json:"phone"`
	URL        string   `json:"url"`
	Categories []string `json:"categories"`
}

type ttAddress struct {
	StreetName         string `json:"streetName"`
	Municipality       string `json:"municipality"`
	CountrySubdivision string `json:"countrySubdivision"`
	PostalCode         string `json:"postalCode"`
	FreeformAddress    string `json:"freeformAddress"`
}

type ttPosition struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func (r *ttResult) toLead() domain.Lead {
	phone := sanitizePhone(r.POI.Phone)

	var whatsapp string
	if phone != "" {
		digits := phone
		if strings.HasPrefix(digits, "55") && len(digits) == 13 {
			digits = digits[2:]
		}
		if len(digits) == 11 {
			whatsapp = "55" + digits
		}
	}

	website := r.POI.URL
	if website != "" && !strings.HasPrefix(website, "http") {
		website = "https://" + website
	}

	return domain.Lead{
		Name:     r.POI.Name,
		Phone:    phone,
		WhatsApp: whatsapp,
		Website:  website,
		Address: domain.Address{
			Street:  r.Address.StreetName,
			City:    r.Address.Municipality,
			State:   r.Address.CountrySubdivision,
			ZipCode: r.Address.PostalCode,
		},
		Source: []string{"tomtom"},
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func parseLatLon(latlon string) (lat, lon float64, ok bool) {
	parts := strings.SplitN(latlon, ",", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var err error
	lat, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	lon, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	return lat, lon, true
}

func sanitizePhone(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	s = strings.ReplaceAll(s, "+", "")
	return s
}
