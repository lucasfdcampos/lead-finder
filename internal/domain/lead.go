// Package domain contains the core business entities and contracts for the lead-finder application.
package domain

// SearchRequest is the payload received by the API endpoint.
type SearchRequest struct {
	Query           string `json:"query"`
	Location        string `json:"location"`
	Limit           int    `json:"limit"` // max leads to return (0 = no limit, returns all discovered)
	SearchWhatsApp  bool   `json:"search_whatsapp"`
	SearchCNPJ      bool   `json:"search_cnpj"`
	SearchInstagram bool   `json:"search_instagram"`
}

// Lead represents a single discovered business lead.
type Lead struct {
	Name        string `json:"name"`
	TradeName   string `json:"trade_name,omitempty"`
	CNPJ        string `json:"cnpj,omitempty"`
	CNAE        string `json:"cnae,omitempty"`
	CNAEDesc    string `json:"cnae_description,omitempty"`
	OpeningDate string `json:"opening_date,omitempty"`

	Address  Address   `json:"address,omitempty"`
	Partners []Partner `json:"partners,omitempty"`

	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
	WhatsApp  string `json:"whatsapp,omitempty"`
	Instagram string `json:"instagram,omitempty"`
	Followers int    `json:"instagram_followers,omitempty"`
	Website   string `json:"website,omitempty"`

	Source []string `json:"source,omitempty"`
}

// Address holds the physical address of a business.
type Address struct {
	Street       string `json:"street,omitempty"`
	Number       string `json:"number,omitempty"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	ZipCode      string `json:"zip_code,omitempty"`
}

// Partner represents a member of the QSA.
type Partner struct {
	Name          string `json:"name"`
	Qualification string `json:"qualification,omitempty"`
}

// SearchContext is the structured output from CNAE enrichment for a user query.
type SearchContext struct {
	CNAE            string   `json:"cnae"`
	CNAEDescription string   `json:"cnae_description"`
	SearchTerms     []string `json:"search_terms"`
	// UseFoursquare signals the pipeline to also query Foursquare-like APIs
	// (true for bars, restaurants, cafés, hotels and similar POI categories).
	UseFoursquare bool `json:"use_foursquare"`
}
