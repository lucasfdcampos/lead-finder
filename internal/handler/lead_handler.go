// Package handler contains the HTTP handlers for the lead-finder API.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"github.com/lucasfdcampos/lead-finder/pkg/httputil"
)

const requestTimeout = 280 * time.Second

// LeadUseCase is the interface the handler depends on (dependency inversion).
type LeadUseCase interface {
	Execute(ctx context.Context, req domain.SearchRequest) ([]domain.Lead, error)
}

// LeadHandler handles POST /leads requests.
type LeadHandler struct {
	uc LeadUseCase
}

// NewLeadHandler creates a new LeadHandler.
func NewLeadHandler(uc LeadUseCase) *LeadHandler {
	return &LeadHandler{uc: uc}
}

// LeadResult is the API response shape for a single lead.
type LeadResult struct {
	Name        string   `json:"name"`
	TradeName   string   `json:"trade_name,omitempty"`
	CNPJ        string   `json:"cnpj,omitempty"`
	CNAE        string   `json:"cnae,omitempty"`
	CNAEDesc    string   `json:"cnae_description,omitempty"`
	OpeningDate string   `json:"opening_date,omitempty"`
	Partners    []string `json:"partners,omitempty"`
	Phone       string   `json:"phone,omitempty"`
	Email       string   `json:"email,omitempty"`
	WhatsApp    string   `json:"whatsapp,omitempty"`
	Instagram   string   `json:"instagram,omitempty"`
	Followers   int      `json:"instagram_followers,omitempty"`
	Website     string   `json:"website,omitempty"`
}

// toLeadResult maps a domain Lead to the response DTO.
func toLeadResult(l domain.Lead) LeadResult {
	r := LeadResult{
		Name:        l.Name,
		TradeName:   l.TradeName,
		CNPJ:        formatCNPJ(l.CNPJ),
		CNAE:        l.CNAE,
		CNAEDesc:    l.CNAEDesc,
		OpeningDate: l.OpeningDate,
		Phone:       l.Phone,
		Email:       l.Email,
		WhatsApp:    l.WhatsApp,
		Followers:   l.Followers,
		Website:     l.Website,
	}

	// Partners: extract just the names.
	for _, p := range l.Partners {
		if p.Name != "" {
			r.Partners = append(r.Partners, p.Name)
		}
	}

	// Instagram: normalise to full URL.
	if l.Instagram != "" {
		handle := strings.TrimPrefix(l.Instagram, "@")
		handle = strings.TrimPrefix(handle, "https://www.instagram.com/")
		handle = strings.TrimPrefix(handle, "https://instagram.com/")
		handle = strings.Trim(handle, "/")
		if handle != "" {
			r.Instagram = fmt.Sprintf("https://www.instagram.com/%s/", handle)
		}
	}

	return r
}

// formatCNPJ formats a 14-digit string as XX.XXX.XXX/XXXX-XX.
func formatCNPJ(cnpj string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, cnpj)
	if len(digits) != 14 {
		return cnpj // return as-is if already formatted or empty
	}
	return fmt.Sprintf("%s.%s.%s/%s-%s",
		digits[0:2], digits[2:5], digits[5:8], digits[8:12], digits[12:14])
}

// ServeHTTP handles the POST /leads endpoint.
func (h *LeadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req domain.SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if req.Query == "" || req.Location == "" {
		httputil.WriteError(w, http.StatusBadRequest, "query and location are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	leads, err := h.uc.Execute(ctx, req)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	results := make([]LeadResult, 0, len(leads))
	for _, l := range leads {
		results = append(results, toLeadResult(l))
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"total": len(results),
		"leads": results,
	})
}
