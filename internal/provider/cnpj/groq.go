// Package cnpj – Groq LLM CNPJ search provider.
//
// GroqProvider calls the Groq API (OpenAI-compatible) with Llama 3.3 70B to find
// a Brazilian CNPJ by company name + city.  Because the model can reason over its
// training knowledge AND we give it clear formatting instructions, this works well
// as a last-resort provider after the direct-scraper chain.
//
// Free tier (groq.com/console):
//
//	30 req/min  |  131 072 tokens/min  — no credit card required
//
// Rate-limit strategy: package-level token-bucket with capacity 30, refilled at
// 1 token / 2 s (= 30 /min).  A call that cannot acquire a token within 5 s is
// aborted so we never block the worker pool indefinitely.
//
// Implements domain.CNPJNameSearcher.
package cnpj

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// ─── Rate limiter ─────────────────────────────────────────────────────────────

// groqBucket is a package-level token bucket: capacity 30, 1 token refilled every 2 s.
// It is initialised lazily on first call to GroqProvider.SearchByName to avoid
// spawning a background goroutine during package init in tests that don't use Groq.
var (
	groqBucket     chan struct{}
	groqBucketOnce sync.Once
)

func initGroqBucket() {
	groqBucket = make(chan struct{}, 30)
	// Pre-fill with 30 tokens (full burst on startup).
	for i := 0; i < 30; i++ {
		groqBucket <- struct{}{}
	}
	// Background goroutine refills 1 token every 2 s (30 /min).
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for range ticker.C {
			select {
			case groqBucket <- struct{}{}: // add token
			default: // bucket full — discard
			}
		}
	}()
}

// acquireGroqToken blocks until a rate-limit token is available or ctx is cancelled.
// Returns false when the context expires before a token arrives.
func acquireGroqToken(ctx context.Context) bool {
	groqBucketOnce.Do(initGroqBucket)
	// Limit wait to 5 s so a busy bucket doesn't stall the whole worker pool.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-groqBucket:
		return true
	case <-waitCtx.Done():
		return false
	}
}

// ─── Provider ─────────────────────────────────────────────────────────────────

const (
	groqBaseURL   = "https://api.groq.com/openai/v1/chat/completions"
	groqModel     = "llama-3.3-70b-versatile"
	groqTimeout   = 20 * time.Second
	groqMaxTokens = 64 // we only need the 14-digit CNPJ in the reply
)

// GroqProvider searches for a CNPJ using the Groq API (Llama 3.3 70B).
type GroqProvider struct {
	apiKey string
	client *http.Client
	// callCount is incremented each time SearchByName is invoked, for logging.
	callCount atomic.Int64
}

// NewGroq creates a GroqProvider.  Returns nil when apiKey is empty so callers
// can pass it straight into NewMultiNameSearcher without an extra nil-check.
func NewGroq(apiKey string) *GroqProvider {
	if apiKey == "" {
		return nil
	}
	return &GroqProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: groqTimeout},
	}
}

func (p *GroqProvider) Name() string {
	if p == nil {
		return "groq(disabled)"
	}
	return "groq"
}

// SearchByName queries Groq (Llama 3.3 70B) for the CNPJ of name + city.
// Returns domain.ErrNotFound when the model cannot find a result, or
// domain.ErrRateLimited when the token bucket is exhausted.
func (p *GroqProvider) SearchByName(ctx context.Context, name, city string) (string, error) {
	if p == nil {
		return "", domain.ErrNotFound
	}

	// Honour the package-level rate limit before making the HTTP call.
	if !acquireGroqToken(ctx) {
		return "", domain.ErrRateLimited
	}

	ctx, cancel := context.WithTimeout(ctx, groqTimeout)
	defer cancel()

	p.callCount.Add(1)

	prompt := fmt.Sprintf(
		`Qual é o CNPJ da empresa brasileira "%s" localizada em "%s"? `+
			`Responda APENAS com os 14 dígitos do CNPJ, sem pontos, barras ou traços (exemplo: 12345678000199). `+
			`Se não tiver certeza, responda exatamente: NAO_ENCONTRADO`,
		name, city,
	)

	reqBody := groqRequest{
		Model: groqModel,
		Messages: []groqMessage{
			{Role: "user", Content: prompt},
		},
		MaxTokens:   groqMaxTokens,
		Temperature: 0, // deterministic — we want a factual CNPJ, not creativity
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("groq: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqBaseURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("groq: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", domain.ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("groq: HTTP %d — %s", resp.StatusCode, truncate(string(body), 200))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("groq: read body: %w", err)
	}

	var gr groqResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return "", fmt.Errorf("groq: unmarshal: %w", err)
	}

	text := groqResponseText(gr)
	if text == "" || strings.Contains(strings.ToUpper(text), "NAO_ENCONTRADO") {
		return "", domain.ErrNotFound
	}

	cnpj := extractValidCNPJ(text)
	if cnpj == "" {
		return "", domain.ErrNotFound
	}
	return cnpj, nil
}

// ─── CNPJ extraction + checksum ──────────────────────────────────────────────

// reCNPJRaw matches a 14-consecutive-digit block (model output format we request).
var reCNPJRaw = regexp.MustCompile(`\b(\d{14})\b`)

// reCNPJFmt matches XX.XXX.XXX/XXXX-XX.
var reCNPJFmt = regexp.MustCompile(`\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}`)

// extractValidCNPJ finds the first checksum-valid CNPJ in s and returns 14 digits.
func extractValidCNPJ(s string) string {
	for _, m := range reCNPJRaw.FindAllStringSubmatch(s, 5) {
		if isValidCNPJ(m[1]) {
			return m[1]
		}
	}
	for _, m := range reCNPJFmt.FindAllString(s, 5) {
		d := reNonDigit.ReplaceAllString(m, "")
		if isValidCNPJ(d) {
			return d
		}
	}
	return ""
}

// isValidCNPJ checks both CNPJ check-digits per the Receita Federal algorithm.
func isValidCNPJ(cnpj string) bool {
	if len(cnpj) != 14 {
		return false
	}
	allSame := true
	for i := 1; i < 14; i++ {
		if cnpj[i] != cnpj[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return false
	}
	d := func(c byte) int { return int(c - '0') }
	calc := func(s string, w []int) int {
		sum := 0
		for i, wi := range w {
			sum += d(s[i]) * wi
		}
		r := sum % 11
		if r < 2 {
			return 0
		}
		return 11 - r
	}
	if calc(cnpj[:12], []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}) != d(cnpj[12]) {
		return false
	}
	if calc(cnpj[:13], []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}) != d(cnpj[13]) {
		return false
	}
	return true
}

// ─── Groq JSON structs ────────────────────────────────────────────────────────

type groqRequest struct {
	Model       string        `json:"model"`
	Messages    []groqMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func groqResponseText(r groqResponse) string {
	if len(r.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(r.Choices[0].Message.Content)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
