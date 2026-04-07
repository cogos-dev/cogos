# CogOS Kernel — Makefile
#
# Targets:
#   make build      — compile the binary
#   make test       — run unit tests
#   make e2e        — run e2e test in a container (cold-start → serve → verify)
#   make e2e-local  — run e2e test locally (requires built binary)
#   make image      — build production OCI image
#   make run        — run in Docker with workspace volume mount
#   make clean      — remove build artifacts
#   make tidy       — go mod tidy

BINARY     := cogos
IMAGE      := cogos-dev/cogos
TAG        := dev
PORT       := 6931
WORKSPACE  ?= $(shell git rev-parse --show-toplevel 2>/dev/null || echo $$HOME/cog-workspace)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION    ?= dev
LDFLAGS    := -s -w -X github.com/cogos-dev/cogos/internal/engine.Version=$(VERSION) -X github.com/cogos-dev/cogos/internal/engine.BuildTime=$(BUILD_TIME)

.PHONY: build test test-coverage test-integration bench install image run push clean tidy e2e e2e-local

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/cogos

build-mcp:
	go build -tags mcpserver -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/cogos

install: build
	install -m 755 $(BINARY) /usr/local/bin/cogos
	@echo "Installed /usr/local/bin/cogos"

test:
	go test -race -count=1 ./...

test-coverage:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

test-integration:
	go test -tags integration -race -count=1 -timeout 30s ./...

bench: build
	./$(BINARY) bench --workspace $(WORKSPACE) --no-inference

image:
	docker build \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE):$(TAG) \
		.

run: image
	docker run --rm \
		-v $(WORKSPACE):$(WORKSPACE) \
		-p $(PORT):$(PORT) \
		$(IMAGE):$(TAG) \
		serve --workspace $(WORKSPACE) --port $(PORT)

push:
	docker push $(IMAGE):$(TAG)

e2e:
	docker build -f Dockerfile.e2e -t cogos-e2e-test .
	docker run --rm cogos-e2e-test

e2e-local: build
	COGOS_BIN=./$(BINARY) ./scripts/e2e-test.sh

clean:
	rm -f $(BINARY)
	go clean ./...

tidy:
	go mod tidy
