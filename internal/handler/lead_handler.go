// Package handler contains the HTTP handlers for the lead-finder API.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"github.com/lucasfdcampos/lead-finder/pkg/httputil"
)

const requestTimeout = 120 * time.Second

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

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"total": len(leads),
		"leads": leads,
	})
}
