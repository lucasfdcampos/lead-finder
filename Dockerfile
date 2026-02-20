FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o lead-finder ./cmd/api

# ── Final image ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /app/lead-finder /lead-finder

EXPOSE 8080

ENTRYPOINT ["/lead-finder"]
