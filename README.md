# lead-finder

> API em Go para geração de leads empresariais brasileiros via Clean Architecture.

## Funcionalidades

| Fase | O que faz |
|------|-----------|
| **1 – Enriquecimento** | Chama o **Gemini** para identificar o CNAE correto e gerar termos de busca |
| **2 – CNPJ** | **Brasil API** (JSON, primary) → **CNPJ.biz** (scraping, fallback) |
| **3 – Detalhamento** | Para cada CNPJ encontrado: busca razão social, endereço, QSA, telefone e e-mail |
| **4 – Instagram** | DuckDuckGo → Gemini extractor → Heurística `GenerateHandleVariations` → validação via *storynavigation*/*insta-stories-viewer* |
| **4 – WhatsApp** | DuckDuckGo → regex de número → Gemini extractor fallback |

## Endpoint

```
POST /leads
Content-Type: application/json

{
  "query": "loja de roupas",
  "location": "Arapongas-PR",
  "search_whatsapp": true,
  "search_cnpj": true,
  "search_instagram": true
}
```

### Resposta

```json
{
  "total": 3,
  "leads": [
    {
      "name": "MODA FASHION LTDA",
      "trade_name": "Moda Fashion",
      "cnpj": "12345678000195",
      "cnae": "4781400",
      "cnae_description": "Comércio varejista de artigos do vestuário e acessórios",
      "opening_date": "2015-03-10",
      "address": { "street": "Rua XV de Novembro", "number": "100", ... },
      "partners": [{ "name": "JOAO DA SILVA", "qualification": "Sócio-Administrador" }],
      "phone": "4330001234",
      "email": "contato@modafashion.com.br",
      "whatsapp": "554330001234",
      "instagram": "modafashionarapongas",
      "instagram_followers": 1842,
      "source": ["brasilapi", "instagram", "whatsapp"]
    }
  ]
}
```

## Arquitetura

```
lead-finder/
├── cmd/api/           # main.go — wiring de dependências e servidor HTTP
├── internal/
│   ├── domain/        # Structs (Lead, SearchRequest, …) + Interfaces (DiscoveryProvider, …)
│   ├── usecase/       # LeadDiscoveryUseCase — orquestração do pipeline
│   ├── handler/       # HTTP handler (POST /leads)
│   └── provider/
│       ├── gemini/    # QueryEnricher + extratores de handle/número
│       ├── cnpj/      # BrasilAPIProvider (primary) + CNPJBizProvider (fallback)
│       ├── search/    # DuckDuckGoProvider (WebSearcher)
│       ├── instagram/ # InstagramSearcher + GenerateHandleVariations
│       └── whatsapp/  # WhatsAppSearcher
└── pkg/httputil/      # helpers de resposta JSON
```

## Setup

```bash
cp .env.example .env
# Edite .env e preencha GEMINI_API_KEY

make tidy
make run
```

### Com Docker

```bash
make docker-build
make docker-run
```

## Variáveis de Ambiente

| Variável | Obrigatória | Descrição |
|----------|-------------|-----------|
| `GEMINI_API_KEY` | ✅ | Chave da Google AI Studio |
| `GOOGLE_SEARCH_API_KEY` | ⭐ Recomendado | Google Cloud API key com "Custom Search API" habilitada (100 req/dia grátis) |
| `GOOGLE_CX` | ⭐ Recomendado | ID do Programmable Search Engine (veja setup abaixo) |
| `SERPAPI_KEY` | ❌ | Chave do SerpAPI (100 req/mês grátis) — fallback se Google CSE não configurado |
| `BING_API_KEY` | ❌ | Chave RapidAPI do Bing Web Search — fallback se nenhum dos acima configurado |
| `FOURSQUARE_API_KEY` | ❌ | Chave do Foursquare Places API v3 (grátis) |
| `LOCATIONIQ_API_KEY` | ❌ | Chave do LocationIQ para geocoding |
| `PORT` | ❌ | Porta HTTP (padrão: `8080`) |

### Prioridade do Web Searcher

O pipeline usa o primeiro provider disponível:
```
Google Custom Search > SerpAPI > Bing (RapidAPI) > DuckDuckGo (fallback, pode ser bloqueado)
```

### Setup do Google Custom Search (recomendado)

1. Acesse [programmablesearchengine.google.com](https://programmablesearchengine.google.com/)
2. Crie um novo Search Engine → marque **"Search the entire web"**
3. Copie o **CX ID** → `GOOGLE_CX`
4. No [Google Cloud Console](https://console.cloud.google.com/), habilite a **Custom Search API**
5. Crie ou use uma API key existente → `GOOGLE_SEARCH_API_KEY`
   - A mesma key do Gemini pode ser usada se estiver no mesmo projeto


## Heurística de Instagram — `GenerateHandleVariations`

Dado o nome empresarial `"Moda Fashion Ltda"`, a função gera:

```
modafashion
moda_fashion
moda.fashion
modafashion   (2 primeiras palavras)
moda          (primeira palavra)
```

Cada candidato é validado via scraper antes de ser aceito.
