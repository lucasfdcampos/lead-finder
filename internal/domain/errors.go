package domain

import "errors"

var (
	ErrNotFound        = errors.New("resource not found")
	ErrExternalTimeout = errors.New("external provider timed out")
	ErrRateLimited     = errors.New("external provider rate-limited this request")
	ErrInvalidCNPJ     = errors.New("invalid CNPJ format")
	ErrNoResults       = errors.New("no results returned by provider")
)
