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
#   make dev      - Build a dev binary + print isolation instructions
#   make install  - Install to $(PREFIX)/bin/cogos (PREFIX defaults to ~/.cog)
#   make push     - Build + push to OCI layout (triggers kernel auto-reload)
#   make image    - Build production OCI image
#   make e2e      - Run e2e test in a container
#   make e2e-local - Run e2e test locally

# VERSION is derived from git so a local build never claims to be a release it
# is not. `git describe --tags --always --dirty` yields e.g.
# v0.16.22-2-g127b651-dirty: descended from v0.16.22, two commits past it, with
# uncommitted changes.
# Overridable (?=) so packagers and any caller that builds through make can pin
# an exact value. Note the release workflow does NOT go through make — it calls
# `go build` with its own ldflags (release.yml:95) — so releases are unaffected
# by this default either way.
#
# The `dev-` prefix is load-bearing, not cosmetic — DO NOT REMOVE IT. A bare
# `git describe` string like `v0.16.22-6-g4f0acf1-dirty` is VALID semver whose
# suffix is a PRERELEASE, and prereleases sort BEFORE their base tag under
# golang.org/x/mod/semver — see internal/providers/selfupdate/version_test.go:35-36.
# So a bare describe string reads as OLDER than the release it descends from:
# GATE D (provider.go:272) does not fire because the version parses, and GATE F
# (provider.go:282) does not fire because
# versionAfter("v0.16.22", "v0.16.22-6-g4f0acf1-dirty") is TRUE.
# Self-update would then overwrite the dev binary with the plain release — the
# exact clobber this PR exists to prevent. Prefixing with `dev-` makes
# normVersion() return "" so GATE D fires and self-update stays inert, while the
# full describe string is retained for traceability. This is pinned by
# internal/providers/selfupdate/devbuild_version_test.go.
# Falls back to the honest "dev" (matching internal/engine/cli.go:32's default)
# when git is unavailable, e.g. a source tarball with no .git.
VERSION ?= $(shell d=$$(git describe --tags --always --dirty 2>/dev/null) && echo "dev-$$d" || echo dev)
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

.PHONY: all build clean test test-coverage test-integration bench install check-not-running dev push image run e2e e2e-local $(PLATFORMS) $(BINARY)

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

# PREFIX is the install root. Override it to install a dev build somewhere that
# is not the production kernel's path:
#   make install PREFIX=$$HOME/.cog-dev
PREFIX ?= $(HOME)/.cog
INSTALL_DIR := $(PREFIX)/bin
INSTALL_TARGET := $(INSTALL_DIR)/cogos

# Install to $(PREFIX)/bin/cogos (atomic: build, verify, checksum, move).
# Refuses to overwrite a binary that a running process is executing — see
# check-not-running.
install: build check-not-running
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

# Refuse to clobber a binary that a live process is executing. This is the guard
# that turns "use a different PREFIX" from a convention into a safety property:
# on a machine running a production node, `make install` would otherwise replace
# the running kernel's binary in place.
#
# Set ALLOW_RUNNING_INSTALL=1 to override (e.g. a deliberate in-place upgrade
# where you intend to restart the daemon afterwards).
#
# Resolving a PID's executable is platform-specific, and getting this wrong
# makes the guard a silent no-op rather than a loud failure:
#   Linux — /proc/PID/exe is the authoritative symlink. NOTE: `ps -o comm=`
#           returns only the BASENAME on procps, so comparing it to a full path
#           never matches; do not rely on it here.
#   macOS — no /proc; `lsof -p PID` reports the text (executable) mapping, and
#           BSD `ps -o comm=` also happens to give a full path.
# Anything that does not resolve to an absolute path is treated as UNKNOWN, and
# an unknown `cogos serve` process is refused rather than assumed harmless: a
# process owned by another user (a service account) is not readable via
# /proc/PID/exe or lsof, and guessing wrong overwrites a live production binary.
# Fail loud, with ALLOW_RUNNING_INSTALL=1 as the documented escape.
# Both sides are realpath-normalised so a symlinked PREFIX still matches.
#
# Detection is two-stage, NOT a `pgrep -f 'cogos serve'` contiguous-substring
# match: the official `cog` wrapper (scripts/cog) always execs the kernel as
# `<kernel> --workspace <path> serve ...`, putting the workspace flag between
# "cogos" and "serve" in argv, so a plain "cogos serve" substring match never
# fires for wrapper-started daemons — exactly the invisible-daemon case this
# guard exists to catch. Instead: (1) pgrep on the binary name only, anchored
# to a path/word boundary so it doesn't match unrelated binaries that merely
# contain "cogos" as a substring (e.g. cogos-channel-bridge); (2) for each
# candidate PID, pull its full argv and check for a standalone "serve" word
# anywhere in it, so the workspace flag (or any other flag) between the binary
# and the subcommand no longer defeats detection.
check-not-running:
	@if [ "$(ALLOW_RUNNING_INSTALL)" = "1" ]; then \
		echo "  ALLOW_RUNNING_INSTALL=1 — skipping running-daemon check"; \
		exit 0; \
	fi; \
	target="$(INSTALL_TARGET)"; \
	if [ ! -e "$$target" ]; then exit 0; fi; \
	rtarget=$$(cd "$$(dirname "$$target")" 2>/dev/null && pwd -P)/$$(basename "$$target"); \
	if ! command -v pgrep >/dev/null 2>&1; then \
		echo ""; \
		echo "REFUSING TO INSTALL: pgrep not found, so a running kernel cannot be"; \
		echo "detected. Installing blind could overwrite a live production binary."; \
		echo ""; \
		echo "  make install PREFIX=\$$HOME/.cog-dev   # install beside it, not over it"; \
		echo "  make install ALLOW_RUNNING_INSTALL=1  # override, if you mean it"; \
		echo ""; \
		exit 1; \
	fi; \
	pids=$$(pgrep -f '(^|/)cogos( |$$)' 2>/dev/null || true); \
	for pid in $$pids; do \
		cmdline=""; \
		if [ -r "/proc/$$pid/cmdline" ]; then \
			cmdline=$$(tr '\0' ' ' < "/proc/$$pid/cmdline" 2>/dev/null || true); \
		fi; \
		if [ -z "$$cmdline" ]; then \
			cmdline=$$(ps -o args= -p "$$pid" 2>/dev/null || true); \
		fi; \
		is_serve=0; \
		for tok in $$cmdline; do \
			if [ "$$tok" = "serve" ]; then is_serve=1; fi; \
		done; \
		if [ "$$is_serve" != "1" ]; then continue; fi; \
		exe=""; \
		if [ -r "/proc/$$pid/exe" ]; then \
			exe=$$(readlink -f "/proc/$$pid/exe" 2>/dev/null || true); \
		fi; \
		if [ -z "$$exe" ] && command -v lsof >/dev/null 2>&1; then \
			exe=$$(lsof -p "$$pid" -Ffn 2>/dev/null | awk '/^ftxt$$/{t=1;next} /^n/{if(t){print substr($$0,2);exit}} {t=0}'); \
		fi; \
		if [ -z "$$exe" ]; then \
			exe=$$(ps -o comm= -p "$$pid" 2>/dev/null || true); \
		fi; \
		case "$$exe" in /*) ;; *) exe="";; esac; \
		if [ -z "$$exe" ]; then \
			echo ""; \
			echo "REFUSING TO INSTALL: cannot determine the executable of PID $$pid,"; \
			echo "which is running 'cogos serve'. It may be this target."; \
			echo ""; \
			echo "This usually means the process belongs to another user (a service"; \
			echo "account), so /proc/PID/exe and lsof are not readable from here."; \
			echo "Refusing rather than guessing: a wrong guess overwrites a live"; \
			echo "production binary."; \
			echo ""; \
			echo "Options:"; \
			echo "  make install PREFIX=\$$HOME/.cog-dev   # install beside it, not over it"; \
			echo "  sudo make install                     # if you can read the process"; \
			echo "  make install ALLOW_RUNNING_INSTALL=1  # override, if you mean it"; \
			echo ""; \
			exit 1; \
		fi; \
		if [ "$$exe" = "$$rtarget" ]; then \
			echo ""; \
			echo "REFUSING TO INSTALL: $$target is being executed by PID $$pid."; \
			echo ""; \
			echo "Installing over a running kernel's binary replaces production in place."; \
			echo "Options:"; \
			echo "  make install PREFIX=\$$HOME/.cog-dev   # install beside it, not over it"; \
			echo "  make dev                              # build + isolation instructions"; \
			echo "  make install ALLOW_RUNNING_INSTALL=1  # override, if you mean it"; \
			echo ""; \
			exit 1; \
		fi; \
	done; \
	exit 0

# Build a local dev binary and print how to run it without colliding with a
# production node. Does not install anything.
dev: build
	@echo ""
	@echo "=== Dev build ready: ./$(BINARY) (version $(VERSION)) ==="
	@echo ""
	@echo "Run it isolated from any production node — separate port AND workspace:"
	@echo "  ./$(BINARY) serve --workspace /tmp/cog-dev --port 6932"
	@echo ""
	@echo "Never point a dev kernel at a production workspace: the two would"
	@echo "contend on the same state dir and lock files (see #482)."
	@echo ""
	@echo "If this node has self-update enabled with auto_apply, pin it first or"
	@echo "a release will silently replace an installed dev binary mid-session:"
	@echo "  pin: $(shell git describe --tags --abbrev=0 2>/dev/null || echo vX.Y.Z)   # in .cog/config/self-update.yaml"
	@echo ""

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
