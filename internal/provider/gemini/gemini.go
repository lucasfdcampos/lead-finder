// Package gemini wraps the Google Generative AI SDK to act as a QueryEnricher.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/lucasfdcampos/lead-finder/internal/domain"
	"google.golang.org/api/option"
)

const (
	defaultModel   = "gemini-1.5-flash"
	defaultTimeout = 15 * time.Second
)

// Provider implements domain.QueryEnricher using the Gemini API.
type Provider struct {
	client  *genai.Client
	model   string
	timeout time.Duration
}

// New creates a new Gemini Provider.
func New(apiKey string) (*Provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to create client: %w", err)
	}

	return &Provider{
		client:  client,
		model:   defaultModel,
		timeout: defaultTimeout,
	}, nil
}

// Name satisfies domain.DiscoveryProvider.
func (p *Provider) Name() string { return "gemini" }

// Enrich satisfies domain.QueryEnricher.
func (p *Provider) Enrich(ctx context.Context, query, location string) (*domain.GeminiSearchContext, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	prompt := buildEnrichPrompt(query, location)

	model := p.client.GenerativeModel(p.model)
	model.SetTemperature(0.2)
	model.ResponseMIMEType = "application/json"

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return nil, fmt.Errorf("gemini: GenerateContent error: %w", err)
	}

	raw, err := extractText(resp)
	if err != nil {
		return nil, err
	}

	var result domain.GeminiSearchContext
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("gemini: failed to parse response JSON: %w\nraw: %s", err, raw)
	}

	return &result, nil
}

// ExtractInstagramHandle asks Gemini to pull an Instagram handle from search snippets.
func (p *Provider) ExtractInstagramHandle(ctx context.Context, businessName string, snippets []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	prompt := buildInstagramPrompt(businessName, snippets)

	model := p.client.GenerativeModel(p.model)
	model.SetTemperature(0.1)

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("gemini: ExtractInstagramHandle error: %w", err)
	}

	raw, err := extractText(resp)
	if err != nil {
		return "", err
	}

	handle := strings.TrimSpace(raw)
	handle = strings.TrimPrefix(handle, "@")
	handle = strings.Trim(handle, "\"'")

	return handle, nil
}

// ExtractWhatsAppNumber asks Gemini to identify a WhatsApp number from search snippets.
func (p *Provider) ExtractWhatsAppNumber(ctx context.Context, businessName string, snippets []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	prompt := buildWhatsAppPrompt(businessName, snippets)

	model := p.client.GenerativeModel(p.model)
	model.SetTemperature(0.1)

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("gemini: ExtractWhatsAppNumber error: %w", err)
	}

	raw, err := extractText(resp)
	if err != nil {
		return "", err
	}

	number := strings.TrimSpace(raw)
	number = strings.Trim(number, "\"'")

	return number, nil
}

// Close releases the underlying gRPC connection.
func (p *Provider) Close() error {
	return p.client.Close()
}

func buildEnrichPrompt(query, location string) string {
	return fmt.Sprintf(`Voce e um assistente especializado em dados empresariais brasileiros.

Dada a busca abaixo, retorne SOMENTE um JSON valido (sem markdown, sem explicacoes) com o seguinte schema:
{
  "cnae": "<codigo CNAE de 7 digitos mais relevante>",
  "cnae_description": "<descricao oficial do CNAE>",
  "search_terms": ["<termo 1 em portugues>", "<termo 2>", "<termo 3>"]
}

Regras:
- "cnae" deve ser o codigo CNAE brasileiro (ex: "4781-4/00").
- "search_terms" devem ser variacoes e sinonimos uteis para buscar esse tipo de negocio no DuckDuckGo.
- Maximo 5 termos em "search_terms".
- Responda APENAS com o JSON, sem nenhum texto adicional.

Busca: "%s"
Localizacao: "%s"`, query, location)
}

func buildInstagramPrompt(businessName string, snippets []string) string {
	snippetBlock := strings.Join(snippets, "\n---\n")
	return fmt.Sprintf(`Voce e um assistente de extracao de dados.

Analise os snippets abaixo e encontre o perfil do Instagram da empresa "%s".
Responda SOMENTE com o handle do Instagram (sem @, sem URL completa, sem explicacoes).
Se nao encontrar, responda exatamente: NOT_FOUND

Snippets:
%s`, businessName, snippetBlock)
}

func buildWhatsAppPrompt(businessName string, snippets []string) string {
	snippetBlock := strings.Join(snippets, "\n---\n")
	return fmt.Sprintf(`Voce e um assistente de extracao de dados.

Analise os snippets abaixo e encontre o numero de WhatsApp da empresa "%s".
Retorne SOMENTE o numero no formato internacional sem espacos (ex: 5543999998888).
Se nao encontrar, responda exatamente: NOT_FOUND

Snippets:
%s`, businessName, snippetBlock)
}

func extractText(resp *genai.GenerateContentResponse) (string, error) {
	if resp == nil || len(resp.Candidates) == 0 {
		return "", fmt.Errorf("gemini: empty response")
	}

	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if t, ok := part.(genai.Text); ok {
			sb.WriteString(string(t))
		}
	}

	text := strings.TrimSpace(sb.String())
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	if text == "" {
		return "", fmt.Errorf("gemini: response contained no text parts")
	}

	return text, nil
}
