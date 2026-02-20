// Package whatsapp finds WhatsApp numbers for a business via DuckDuckGo + Gemini.
package whatsapp

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/lucasfdcampos/lead-finder/internal/domain"
)

// GeminiExtractor is a narrow interface to avoid importing the full gemini package.
type GeminiExtractor interface {
	ExtractWhatsAppNumber(ctx context.Context, businessName string, snippets []string) (string, error)
}

// Provider implements domain.WhatsAppSearcher.
type Provider struct {
	searcher domain.WebSearcher
	gemini   GeminiExtractor
}

// New creates a new WhatsApp Provider.
func New(searcher domain.WebSearcher, gemini GeminiExtractor) *Provider {
	return &Provider{
		searcher: searcher,
		gemini:   gemini,
	}
}

func (p *Provider) Name() string { return "whatsapp" }

// FindNumber tries to find a WhatsApp number for the given business.
func (p *Provider) FindNumber(ctx context.Context, businessName, location string) (string, error) {
	query := fmt.Sprintf(`"%s" whatsapp %s`, businessName, location)
	results, err := p.searcher.Search(ctx, query)
	if err == nil && len(results) > 0 {
		snippets := make([]string, 0, len(results))
		for _, r := range results {
			text := r.Snippet + " " + r.Title + " " + r.URL
			snippets = append(snippets, text)
			if num := extractBrazilianPhone(text); num != "" {
				return num, nil
			}
		}

		if p.gemini != nil {
			num, err := p.gemini.ExtractWhatsAppNumber(ctx, businessName, snippets)
			if err == nil && num != "" && num != "NOT_FOUND" {
				return sanitizeNumber(num), nil
			}
		}
	}

	return "", domain.ErrNotFound
}

var rePhone = regexp.MustCompile(
	`(?:wa\.me/|phone=|whatsapp\.com/send\?phone=)?\+?` +
		`(?:55)?` +
		`\(?([0-9]{2})\)?` +
		`[\s\-]?` +
		`(9?[0-9]{4})` +
		`[\s\-]?` +
		`([0-9]{4})`,
)

var reWALink = regexp.MustCompile(`wa\.me/([0-9]{10,13})`)

func extractBrazilianPhone(text string) string {
	if m := reWALink.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	if m := rePhone.FindStringSubmatch(text); len(m) > 3 {
		ddd := m[1]
		first := m[2]
		last := m[3]
		full := "55" + ddd + first + last
		if len(full) >= 12 && len(full) <= 13 {
			return full
		}
	}
	return ""
}

func sanitizeNumber(s string) string {
	re := regexp.MustCompile(`\D`)
	s = re.ReplaceAllString(strings.TrimSpace(s), "")
	if len(s) == 11 {
		s = "55" + s
	}
	return s
}
