# CogOS Kernel Build System
# github.com/myrgic/cogos
#
# Multi-platform binaries can be built for distribution.
#
# Usage:
#   make          - Build for current platform (creates 'cog' binary)
#   make all      - Build for all platforms (cog-{os}-{arch})
#   make test     - Run tests
#   make clean    - Remove build artifacts
#   make install  - Install to ~/.cog/bin/cogos
#   make push     - Build + push to OCI layout (triggers kernel auto-reload)
#   make image    - Build production OCI image
#   make e2e      - Run e2e test in a container
#   make e2e-local - Run e2e test locally

VERSION := 0.16.1
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/myrgic/cogos/internal/engine.BuildTime=$(BUILD_TIME) -X github.com/myrgic/cogos/internal/engine.Version=$(VERSION)
BUILD_TAGS := fts5
BINARY := cog
GO := go

IMAGE      := ghcr.io/myrgic/cogos
TAG        := dev
PORT       := 6931
WORKSPACE  ?= $(shell git rev-parse --show-toplevel 2>/dev/null || echo $$HOME/cog-workspace)

# Detect current platform
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

# Build targets
PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 android-arm64 windows-amd64 windows-arm64

.PHONY: all build clean test test-coverage test-integration bench install push image run e2e e2e-local $(PLATFORMS) $(BINARY)

# Default: build for current platform
build: $(BINARY)

$(BINARY):
	$(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/cogos

# Build for all platforms
all: $(PLATFORMS)

darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-arm64 ./cmd/cogos

darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-amd64 ./cmd/cogos

linux-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-amd64 ./cmd/cogos

linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-arm64 ./cmd/cogos

# Android requires PIE (position-independent executables)
android-arm64:
	GOOS=android GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -buildmode=pie -ldflags="$(LDFLAGS)" -o $(BINARY)-android-arm64 ./cmd/cogos

# Windows cross-compile: matches .github/workflows/release.yml exactly.
# CGO_ENABLED=0 avoids tree-sitter's CGO path (same reason as the default
# build target). .exe suffix is required to execute on Windows.
windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BINARY)-windows-amd64.exe ./cmd/cogos/

windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BINARY)-windows-arm64.exe ./cmd/cogos/

INSTALL_DIR := $(HOME)/.cog/bin
INSTALL_TARGET := $(INSTALL_DIR)/cogos

# Install to ~/.cog/bin/cogos (atomic: build, verify, checksum, move)
install: build
	@echo "=== Installing to $(INSTALL_TARGET) ==="
	@./$(BINARY) version > /dev/null 2>&1 || (echo "ERROR: built binary fails version check" && exit 1)
	@mkdir -p "$(INSTALL_DIR)"
	@if [ -f "$(INSTALL_TARGET)" ]; then \
		cp "$(INSTALL_TARGET)" "$(INSTALL_TARGET).bak"; \
		echo "  Backed up existing binary to $(INSTALL_TARGET).bak"; \
	fi
	@cp $(BINARY) "$(INSTALL_TARGET).tmp"
	@chmod +x "$(INSTALL_TARGET).tmp"
	@mv "$(INSTALL_TARGET).tmp" "$(INSTALL_TARGET)"
	@NEW_SHA=$$(shasum -a 256 "$(INSTALL_TARGET)" | cut -d' ' -f1); \
		echo "  Installed cogos $(VERSION) ($(GOOS)/$(GOARCH))"; \
		echo "  SHA-256: $$NEW_SHA"

# Push to OCI layout — running kernel auto-reloads
push: build
	@echo "=== Pushing to OCI layout ==="
	@./$(BINARY) oci push ./$(BINARY)

# Run tests
test: build
	@echo "=== Unit Tests ==="
	$(GO) test -tags "$(BUILD_TAGS)" -count=1 ./...
	@echo ""
	@echo "=== Smoke Tests ==="
	@echo "=== Version Test ==="
	./$(BINARY) version
	@echo ""
	@echo "=== Health Check ==="
	./$(BINARY) health || true
	@echo ""
	@echo "=== All tests passed ==="

test-coverage:
	$(GO) test -tags "$(BUILD_TAGS)" -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

test-integration:
	$(GO) test -tags "integration,$(BUILD_TAGS)" -race -count=1 -timeout 30s ./...

bench: build
	./$(BINARY) bench --workspace $(WORKSPACE) --no-inference

# Docker targets
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

e2e:
	docker build -f Dockerfile.e2e -t cogos-e2e-test .
	docker run --rm cogos-e2e-test

e2e-local: build
	COGOS_BIN=./$(BINARY) ./scripts/e2e-test.sh

# Compare with Python version
compare: build
	@echo "=== Go Version ==="
	./$(BINARY) coherence check
	@echo ""
	@echo "=== Python Version ==="
	python3 hooks/coherence.py check || echo "(Python coherence not available)"

# Clean build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-*
	rm -f *.tmp.*
	go clean ./...

# Development helpers
fmt:
	gofmt -s -w *.go

vet:
	$(GO) vet ./...

tidy:
	go mod tidy

lint: vet
	@echo "=== Checking for bare exec.Command ==="
	@if grep -n 'exec\.Command(' *.go | grep -v '_test\.go' | grep -v 'CommandContext' | grep -v '// bare-ok' > /dev/null 2>&1; then \
		echo "ERROR: bare exec.Command found (use CommandContext with timeout):"; \
		grep -n 'exec\.Command(' *.go | grep -v '_test\.go' | grep -v 'CommandContext' | grep -v '// bare-ok'; \
		exit 1; \
	else \
		echo "  All exec.Command calls use CommandContext"; \
	fi
	@if grep -rn 'exec\.Command(' sdk/ | grep -v '_test\.go' | grep -v 'CommandContext' | grep -v '// bare-ok' > /dev/null 2>&1; then \
		echo "ERROR: bare exec.Command found in sdk/:"; \
		grep -rn 'exec\.Command(' sdk/ | grep -v '_test\.go' | grep -v 'CommandContext' | grep -v '// bare-ok'; \
		exit 1; \
	else \
		echo "  SDK: All exec.Command calls use CommandContext"; \
	fi

# Show binary info
info: build
	@echo "Binary: $(BINARY)"
	@echo "Size: $(shell ls -lh $(BINARY) | awk '{print $$5}')"
	@echo "Version: $(VERSION)"
	@echo "Build: $(BUILD_TIME)"
	@file $(BINARY)
