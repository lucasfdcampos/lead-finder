BINARY   := lead-finder
CMD_PATH := ./cmd/api
OUT_DIR  := ./bin

.PHONY: all build run test tidy lint clean

all: build

## build: compile the binary to ./bin/lead-finder
build:
	@mkdir -p $(OUT_DIR)
	go build -ldflags="-s -w" -o $(OUT_DIR)/$(BINARY) $(CMD_PATH)

## run: run the server (requires .env to be present)
run:
	go run $(CMD_PATH)/...

## test: run all unit tests with race detector
test:
	go test -race -cover ./...

## tidy: tidy & verify go modules
tidy:
	go mod tidy
	go mod verify

## lint: run golangci-lint (must be installed)
lint:
	golangci-lint run ./...

## clean: remove build artefacts
clean:
	rm -rf $(OUT_DIR)

## docker-build: build the Docker image
docker-build:
	docker build -t $(BINARY):latest .

## docker-run: run the container (pass GEMINI_API_KEY via env file)
docker-run:
	docker run --rm -p 8080:8080 --env-file .env $(BINARY):latest
