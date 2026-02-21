// Package geoapify provides a PlacesSearcher backed by the Geoapify Geocoding API
// (free tier: 90 000 requests/month, 3 000/day — no credit card required).
//
// It uses the /v1/geocode/search endpoint with type=amenity, which performs
// free-text POI lookup and returns business name, address, phone and website.
package geoapify

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
	geoBaseURL = "https://api.geoapify.com/v1/geocode/search"
	geoTimeout = 12 * time.Second
	geoMaxRows = 50
)

// Provider implements domain.PlacesSearcher using Geoapify Geocoding API.
type Provider struct {
	apiKey string
	client *http.Client
}

// New creates a Geoapify provider.
func New(apiKey string) *Provider {
	return &Provider{
		apiKey: apiKey,
		client: &http.Client{Timeout: geoTimeout},
	}
}

func (p *Provider) Name() string { return "geoapify" }

// SearchPlaces queries Geoapify for businesses matching query + optional location.
// latlon is "lat,lon" (may be empty — will fall back to country-level bias).
func (p *Provider) SearchPlaces(ctx context.Context, query, latlon string, radiusM int) ([]domain.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, geoTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("text", query)
	params.Set("limit", strconv.Itoa(geoMaxRows))
	params.Set("type", "amenity")
	params.Set("lang", "pt")
	params.Set("apiKey", p.apiKey)

	// Always scope to Brazil; optionally bias towards the geocoded point.
	if latlon != "" {
		lat, lon, ok := parseLatLon(latlon)
		if ok {
			// Geoapify bias uses "lon,lat" order (GeoJSON convention).
			params.Set("bias", fmt.Sprintf("proximity:%f,%f|countrycode:br", lon, lat))
			if radiusM > 0 {
				params.Set("filter", fmt.Sprintf("circle:%f,%f,%d", lon, lat, radiusM))
			} else {
				params.Set("filter", "countrycode:br")
			}
		} else {
			params.Set("filter", "countrycode:br")
		}
	} else {
		params.Set("filter", "countrycode:br")
	}

	reqURL := geoBaseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("geoapify: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geoapify: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("geoapify: invalid API key (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geoapify: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("geoapify: read body: %w", err)
	}

	var geoResp geoResponse
	if err := json.Unmarshal(body, &geoResp); err != nil {
		return nil, fmt.Errorf("geoapify: unmarshal: %w", err)
	}

	if len(geoResp.Features) == 0 {
		return nil, domain.ErrNoResults
	}

	var leads []domain.Lead
	for _, f := range geoResp.Features {
		p := f.Properties
		if p.Name == "" {
			continue
		}
		lead := p.toLead()
		leads = append(leads, lead)
	}
	if len(leads) == 0 {
		return nil, domain.ErrNoResults
	}
	return leads, nil
}

// ─── JSON response structs ────────────────────────────────────────────────────

type geoResponse struct {
	Features []geoFeature `json:"features"`
}

type geoFeature struct {
	Properties geoProperties `json:"properties"`
}

type geoProperties struct {
	Name         string  `json:"name"`
	AddressLine1 string  `json:"address_line1"`
	AddressLine2 string  `json:"address_line2"`
	City         string  `json:"city"`
	State        string  `json:"state"`
	Postcode     string  `json:"postcode"`
	Phone        string  `json:"contact_phone"`
	Website      string  `json:"website"`
	Lat          float64 `json:"lat"`
	Lon          float64 `json:"lon"`
}

func (p *geoProperties) toLead() domain.Lead {
	phone := sanitizePhone(p.Phone)

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

	website := p.Website
	if website != "" && !strings.HasPrefix(website, "http") {
		website = "https://" + website
	}

	return domain.Lead{
		Name:     p.Name,
		Phone:    phone,
		WhatsApp: whatsapp,
		Website:  website,
		Address: domain.Address{
			Street:  p.AddressLine1,
			City:    p.City,
			State:   p.State,
			ZipCode: p.Postcode,
		},
		Source: []string{"geoapify"},
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// parseLatLon splits a "lat,lon" string into float64 values.
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
