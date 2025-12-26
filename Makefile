# Makefile for GoGate

.PHONY: all build test lint clean docker docker-push helm-package help

# Variables
BINARY_NAME=gogate
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

# Docker
REGISTRY?=ghcr.io
IMAGE_NAME?=abd-ulbasit/gogate
DOCKER_TAG?=$(VERSION)

# Go
GOOS?=$(shell go env GOOS)
GOARCH?=$(shell go env GOARCH)

# Default target
all: lint test build

## Build
build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/gogate

build-all: ## Build for all platforms
	@echo "Building for all platforms..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/gogate
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-arm64 ./cmd/gogate
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-amd64 ./cmd/gogate
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-darwin-arm64 ./cmd/gogate

## Test
test: ## Run tests
	go test -race -v ./...

test-coverage: ## Run tests with coverage
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

test-bench: ## Run benchmarks
	go test -bench=. -benchmem ./...

## Lint
lint: ## Run linters
	@echo "Running go vet..."
	go vet ./...
	@echo "Running staticcheck..."
	@which staticcheck > /dev/null || go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./...

fmt: ## Format code
	go fmt ./...
	gofmt -s -w .

## Docker
docker: ## Build Docker image
	docker build -t $(REGISTRY)/$(IMAGE_NAME):$(DOCKER_TAG) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		.

docker-push: docker ## Push Docker image
	docker push $(REGISTRY)/$(IMAGE_NAME):$(DOCKER_TAG)

docker-compose-up: ## Start docker-compose stack
	docker-compose -f deployments/docker/docker-compose.yaml up -d

docker-compose-down: ## Stop docker-compose stack
	docker-compose -f deployments/docker/docker-compose.yaml down

docker-compose-logs: ## View docker-compose logs
	docker-compose -f deployments/docker/docker-compose.yaml logs -f

## Kubernetes
helm-lint: ## Lint Helm chart
	helm lint deployments/helm/gogate

helm-template: ## Render Helm templates
	helm template gogate deployments/helm/gogate

helm-package: ## Package Helm chart
	helm package deployments/helm/gogate -d dist/

## Development
run: ## Run locally
	go run ./cmd/gogate -config config.yaml

run-dev: ## Run with development settings
	go run ./cmd/gogate -config config.yaml -dev

watch: ## Run with file watching (requires entr)
	@which entr > /dev/null || (echo "Install entr: brew install entr" && exit 1)
	find . -name '*.go' | entr -r go run ./cmd/gogate -config config.yaml

## Load testing
k6-load: ## Run k6 load test
	k6 run k6/load-test.js

k6-rate-limit: ## Run k6 rate limit test
	k6 run k6/rate-limit-test.js

k6-soak: ## Run k6 soak test
	k6 run k6/soak-test.js

## Cleanup
clean: ## Clean build artifacts
	rm -rf bin/
	rm -rf dist/
	rm -f coverage.out coverage.html
	rm -f k6/*.json

## Dependencies
deps: ## Download dependencies
	go mod download
	go mod verify

deps-update: ## Update dependencies
	go get -u ./...
	go mod tidy

## Help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# Version info
version: ## Show version info
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"
