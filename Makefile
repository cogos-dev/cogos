# CogOS v3 Kernel — Makefile
#
# Targets:
#   make build      — compile the binary to ./cog-v3
#   make test       — run tests
#   make image      — build OCI image (cogos/kernel-v3:dev)
#   make run        — run in Docker with workspace volume mount
#   make clean      — remove build artifacts
#   make tidy       — go mod tidy

BINARY     := cog-v3
IMAGE      := cogos/kernel-v3
TAG        := dev
PORT       := 5200
WORKSPACE  ?= $(shell git -C ../.. rev-parse --show-toplevel 2>/dev/null || echo $$HOME/cog-workspace)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.BuildTime=$(BUILD_TIME)
INSTALL    := /opt/homebrew/bin/cogos-v3
INSTALL_ALT := $(HOME)/bin/cogos-v3

.PHONY: build test test-coverage test-integration bench install install-dev image run push clean tidy

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

install: build
	cp $(BINARY) $(INSTALL)
	codesign --sign - --force $(INSTALL)
	@echo "Installed $(INSTALL)"

# install-dev: install to ~/bin (user PATH), ad-hoc sign to bypass provenance check
install-dev: build
	cp $(BINARY) $(INSTALL_ALT)
	codesign --sign - --force $(INSTALL_ALT)
	@echo "Installed $(INSTALL_ALT)"

# Unit tests (fast — no integration tag).
test:
	go test -race -count=1 ./...

# Run the benchmark suite against the real workspace.
bench: build
	./$(BINARY) bench --workspace $(WORKSPACE) --no-inference

# Unit tests with HTML coverage report.
test-coverage:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Integration tests (starts real goroutines, makes HTTP calls).
test-integration:
	go test -tags integration -race -count=1 -timeout 30s ./...

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

clean:
	rm -f $(BINARY)
	go clean ./...

tidy:
	go mod tidy
