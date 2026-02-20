// Package geocoder provides forward + reverse geocoding via LocationIQ.
package geocoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	liqBaseURL = "https://us1.locationiq.com/v1"
	liqTimeout = 8 * time.Second
)

// GeoPoint holds a geocoded result.
type GeoPoint struct {
	Lat         float64
	Lon         float64
	DisplayName string
	City        string
	State       string
	Country     string
}

// LocationIQProvider geocodes location strings to lat/lon.
type LocationIQProvider struct {
	apiKey string
	client *http.Client
}

func New(apiKey string) *LocationIQProvider {
	return &LocationIQProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: liqTimeout},
	}
}

func (p *LocationIQProvider) Name() string { return "locationiq" }

// Geocode converts a free-text location string into a GeoPoint.
// Returns the first (best) result.
func (p *LocationIQProvider) Geocode(ctx context.Context, location string) (*GeoPoint, error) {
	ctx, cancel := context.WithTimeout(ctx, liqTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("key", p.apiKey)
	params.Set("q", location)
	params.Set("format", "json")
	params.Set("limit", "1")
	params.Set("countrycodes", "br")
	params.Set("addressdetails", "1")
	params.Set("accept-language", "pt")

	reqURL := fmt.Sprintf("%s/search?%s", liqBaseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("locationiq: build request: %w", err)
	}
	req.Header.Set("User-Agent", "lead-finder/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("locationiq: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("locationiq: rate limited")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("locationiq: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("locationiq: read body: %w", err)
	}

	var results []liqResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("locationiq: unmarshal: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("locationiq: no results for %q", location)
	}

	r := results[0]
	var lat, lon float64
	fmt.Sscanf(r.Lat, "%f", &lat)
	fmt.Sscanf(r.Lon, "%f", &lon)

	point := &GeoPoint{
		Lat:         lat,
		Lon:         lon,
		DisplayName: r.DisplayName,
		City:        coalesce(r.Address.City, r.Address.Town, r.Address.Village, r.Address.Municipality),
		State:       r.Address.State,
		Country:     r.Address.Country,
	}
	return point, nil
}

// LatLonString returns "lat,lon" ready for Foursquare API.
func (g *GeoPoint) LatLonString() string {
	return strings.TrimRight(
		strings.TrimRight(fmt.Sprintf("%.6f,%.6f", g.Lat, g.Lon), "0"),
		",",
	)
}

// ─── JSON structs ─────────────────────────────────────────────────────────────

type liqResult struct {
	Lat         string     `json:"lat"`
	Lon         string     `json:"lon"`
	DisplayName string     `json:"display_name"`
	Address     liqAddress `json:"address"`
}

type liqAddress struct {
	City         string `json:"city"`
	Town         string `json:"town"`
	Village      string `json:"village"`
	Municipality string `json:"municipality"`
	State        string `json:"state"`
	Country      string `json:"country"`
}

func coalesce(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
