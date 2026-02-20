package geocoder

import "context"

// Adapter wraps LocationIQProvider to satisfy domain.Geocoder.
// (domain.Geocoder returns a "lat,lon" string directly.)
type Adapter struct {
	provider *LocationIQProvider
}

// NewAdapter creates a domain.Geocoder from a LocationIQProvider.
func NewAdapter(apiKey string) *Adapter {
	return &Adapter{provider: New(apiKey)}
}

func (a *Adapter) Name() string { return a.provider.Name() }

// Geocode returns "lat,lon" or an error.
func (a *Adapter) Geocode(ctx context.Context, location string) (string, error) {
	point, err := a.provider.Geocode(ctx, location)
	if err != nil {
		return "", err
	}
	return point.LatLonString(), nil
}
