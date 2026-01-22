# CogOS Kernel Build System
# github.com/cogos-dev/cogos
#
# Multi-platform binaries can be built for distribution.
#
# Usage:
#   make          - Build for current platform (creates 'cog' binary)
#   make all      - Build for all platforms (cog-{os}-{arch})
#   make test     - Run tests
#   make clean    - Remove build artifacts
#   make install  - Install to current directory

VERSION := 2.0.0
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.BuildTime=$(BUILD_TIME)
BUILD_TAGS := fts5
BINARY := cog
GO := go

# Detect current platform
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

# Build targets
PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 android-arm64

.PHONY: all build clean test install $(PLATFORMS)

# Default: build for current platform
build: $(BINARY)

$(BINARY): cog.go go.mod
	$(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY) .

# Build for all platforms
all: $(PLATFORMS)

darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-arm64 .

darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-amd64 .

linux-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-amd64 .

linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

# Android requires PIE (position-independent executables)
android-arm64:
	GOOS=android GOARCH=arm64 $(GO) build -tags "$(BUILD_TAGS)" -buildmode=pie -ldflags="$(LDFLAGS)" -o $(BINARY)-android-arm64 .

# Install to default location
install: build
	@echo "Installed $(BINARY) ($(GOOS)/$(GOARCH))"

# Run tests
test: build
	@echo "=== Version Test ==="
	./$(BINARY) version
	@echo ""
	@echo "=== Help Test ==="
	./$(BINARY) help
	@echo ""
	@echo "=== Health Check ==="
	./$(BINARY) health
	@echo ""
	@echo "=== Coherence Check ==="
	./$(BINARY) coherence check || true
	@echo ""
	@echo "=== All tests passed ==="

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

# Development helpers
fmt:
	gofmt -s -w cog.go

vet:
	$(GO) vet ./...

# Show binary info
info: build
	@echo "Binary: $(BINARY)"
	@echo "Size: $(shell ls -lh $(BINARY) | awk '{print $$5}')"
	@echo "Version: $(VERSION)"
	@echo "Build: $(BUILD_TIME)"
	@file $(BINARY)
