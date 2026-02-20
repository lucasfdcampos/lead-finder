// Package httputil provides shared HTTP helper utilities.
package httputil

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrorResponse is the standard error body returned by all endpoints.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Error: msg})
}
