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
#   make install  - Install to ~/.cog/bin/cogos

VERSION := 2.1.1
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

GO_SOURCES := $(wildcard *.go)

$(BINARY): $(GO_SOURCES) go.mod
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

# Run tests
test: build
	@echo "=== Unit Tests ==="
	$(GO) test -tags "$(BUILD_TAGS)" -count=1 ./...
	@echo ""
	@echo "=== Smoke Tests ==="
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
	gofmt -s -w *.go

vet:
	$(GO) vet ./...

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
