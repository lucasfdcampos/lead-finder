// Package foursquare provides a PlacesSearcher that queries the Foursquare
// Places API v3 to discover POI-type businesses (bars, restaurants, hotels, …).
// It is only invoked when Gemini sets UseFoursquare=true on the search context.
package foursquare

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
	fsqPlacesURL = "https://api.foursquare.com/v3/places/search"
	fsqTimeout   = 12 * time.Second
	fsqMaxLimit  = 50
)

// Provider implements domain.PlacesSearcher using Foursquare Places API v3.
type Provider struct {
	apiKey string
	client *http.Client
}

func New(apiKey string) *Provider {
	return &Provider{
		apiKey: apiKey,
		client: &http.Client{Timeout: fsqTimeout},
	}
}

func (p *Provider) Name() string { return "foursquare" }

// SearchPlaces queries Foursquare for POI businesses matching query + location.
// location must be a geocoded "lat,lon" string (e.g. "-23.5505,-46.6333").
func (p *Provider) SearchPlaces(ctx context.Context, query, latlon string, radiusM int) ([]domain.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, fsqTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("query", query)
	params.Set("limit", strconv.Itoa(fsqMaxLimit))
	if radiusM > 0 {
		params.Set("radius", strconv.Itoa(radiusM))
	}
	if latlon != "" {
		params.Set("ll", latlon)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fsqPlacesURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("foursquare: build request: %w", err)
	}
	req.Header.Set("Authorization", p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("foursquare: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("foursquare: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("foursquare: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("foursquare: read body: %w", err)
	}

	var fsqResp fsqResponse
	if err := json.Unmarshal(body, &fsqResp); err != nil {
		return nil, fmt.Errorf("foursquare: unmarshal: %w", err)
	}

	if len(fsqResp.Results) == 0 {
		return nil, domain.ErrNoResults
	}

	var leads []domain.Lead
	for _, r := range fsqResp.Results {
		leads = append(leads, r.toLead())
	}
	return leads, nil
}

// ─── JSON structs ─────────────────────────────────────────────────────────────

type fsqResponse struct {
	Results []fsqPlace `json:"results"`
}

type fsqPlace struct {
	FSQID      string         `json:"fsq_id"`
	Name       string         `json:"name"`
	Categories []fsqCategory  `json:"categories"`
	Location   fsqLocation    `json:"location"`
	Geocode    fsqGeocode     `json:"geocodes"`
	Website    string         `json:"website"`
	Tel        string         `json:"tel"`
}

type fsqCategory struct {
	Name string `json:"name"`
}

type fsqLocation struct {
	Address      string   `json:"address"`
	CrossStreet  string   `json:"cross_street"`
	Locality     string   `json:"locality"`
	Region       string   `json:"region"`
	Postcode     string   `json:"postcode"`
	Country      string   `json:"country"`
	FormattedAdr string   `json:"formatted_address"`
}

type fsqGeocode struct {
	Main fsqLatLon `json:"main"`
}

type fsqLatLon struct {
	Lat float64 `json:"latitude"`
	Lon float64 `json:"longitude"`
}

func (p *fsqPlace) toLead() domain.Lead {
	var cats []string
	for _, c := range p.Categories {
		cats = append(cats, c.Name)
	}

	return domain.Lead{
		Name:    p.Name,
		Phone:   sanitizePhone(p.Tel),
		Website: p.Website,
		Address: domain.Address{
			Street: p.Location.Address,
			City:   p.Location.Locality,
			State:  p.Location.Region,
			ZipCode: p.Location.Postcode,
		},
		Source: []string{"foursquare"},
	}
}

func sanitizePhone(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	s = strings.ReplaceAll(s, "+", "")
	return s
}
