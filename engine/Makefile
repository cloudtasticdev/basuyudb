.PHONY: build test lint run tidy

# Build the BasuyuDB binary (static, no CGO).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/basuyudb ./cmd/basuyudb

# Run all tests with race detector.
test:
	go test -race -count=1 ./...

# Run the linter (requires golangci-lint in PATH).
lint:
	golangci-lint run ./...

# Run the server locally (requires BASUYUDB_JWKS_URL to be set or uses the default below).
run:
	BASUYUDB_JWKS_URL=$${BASUYUDB_JWKS_URL:-http://localhost:4000/.well-known/jwks.json} \
	BASUYUDB_DEV_MODE=$${BASUYUDB_DEV_MODE:-false} \
	go run ./cmd/basuyudb

# Tidy and download dependencies.
tidy:
	go mod tidy
	go mod download
