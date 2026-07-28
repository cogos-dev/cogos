#!/usr/bin/env bash
# setup-dev.sh — Developer setup for CogOS
#
# Run this after cloning the repo to set up your local dev environment:
#   git clone https://github.com/myrgic/cogos.git
#   cd cogos
#   ./scripts/setup-dev.sh
#
# What it does:
#   1. Checks prerequisites (Go, Docker/Colima, git)
#   2. Builds cogos from source
#   3. Installs cogos binary to ~/.cog/bin/cogos
#   4. Installs cog CLI wrapper to ~/.cog/bin/cog
#   5. Adds ~/.cog/bin to PATH (shell profile)
#   6. Verifies the install

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# PREFIX is the install root. Override it to set up a dev environment beside a
# production node instead of over it:  PREFIX=$HOME/.cog-dev ./scripts/setup-dev.sh
PREFIX="${PREFIX:-$HOME/.cog}"
INSTALL_DIR="$PREFIX/bin"
SHELL_NAME="$(basename "$SHELL")"

# refuse_if_running <target> — refuse to overwrite a binary that a live
# process is executing. Mirrors the Makefile's check-not-running target; see the
# comment there for the platform specifics. An unresolvable `cogos serve`
# process is refused rather than assumed harmless: it may belong to another
# user (a service account), and guessing wrong overwrites a live production
# binary. Set ALLOW_RUNNING_INSTALL=1 to override.
refuse_if_running() {
    local target="$1" rtarget pid exe
    [ "${ALLOW_RUNNING_INSTALL:-}" = "1" ] && return 0
    [ -e "$target" ] || return 0
    rtarget="$(cd "$(dirname "$target")" 2>/dev/null && pwd -P)/$(basename "$target")"
    if ! command -v pgrep >/dev/null 2>&1; then
        echo ""
        echo "REFUSING: pgrep not found, so a running kernel cannot be detected."
        echo "Installing blind could overwrite a live production binary."
        echo ""
        echo "  PREFIX=\$HOME/.cog-dev ./scripts/setup-dev.sh   # install beside it"
        echo "  ALLOW_RUNNING_INSTALL=1 ./scripts/setup-dev.sh # override, if you mean it"
        echo ""
        exit 1
    fi
    for pid in $(pgrep -f 'cogos serve' 2>/dev/null || true); do
        exe=""
        if [ -r "/proc/$pid/exe" ]; then
            exe="$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)"
        fi
        if [ -z "$exe" ] && command -v lsof >/dev/null 2>&1; then
            exe="$(lsof -p "$pid" -Ffn 2>/dev/null | awk '/^ftxt$/{t=1;next} /^n/{if(t){print substr($0,2);exit}} {t=0}')"
        fi
        [ -z "$exe" ] && exe="$(ps -o comm= -p "$pid" 2>/dev/null || true)"
        case "$exe" in /*) ;; *) exe="";; esac
        if [ -z "$exe" ]; then
            echo ""
            echo "REFUSING: cannot determine the executable of PID $pid, which is"
            echo "running 'cogos serve'. It may be $target."
            echo "It likely belongs to another user, so /proc and lsof are unreadable."
            echo "Refusing rather than guessing — a wrong guess overwrites production."
            echo ""
            echo "  PREFIX=\$HOME/.cog-dev ./scripts/setup-dev.sh   # install beside it"
            echo "  ALLOW_RUNNING_INSTALL=1 ./scripts/setup-dev.sh # override, if you mean it"
            echo ""
            exit 1
        fi
        if [ "$exe" = "$rtarget" ]; then
            echo ""
            echo "REFUSING: $target is being executed by PID $pid."
            echo "Installing over a running kernel's binary replaces production in place."
            echo ""
            echo "  PREFIX=\$HOME/.cog-dev ./scripts/setup-dev.sh   # install beside it"
            echo "  ALLOW_RUNNING_INSTALL=1 ./scripts/setup-dev.sh # override, if you mean it"
            echo ""
            exit 1
        fi
    done
}

# ── Colors ────────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${BOLD}$*${NC}"; }
ok()    { echo -e "  ${GREEN}✓${NC} $*"; }
warn()  { echo -e "  ${YELLOW}!${NC} $*"; }
fail()  { echo -e "  ${RED}✗${NC} $*"; }

# ── Prerequisites ─────────────────────────────────────────────────────────────

info "Checking prerequisites..."

MISSING=0

if command -v go &>/dev/null; then
    GO_VERSION=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1)
    ok "Go ($GO_VERSION)"
else
    fail "Go not found — install from https://go.dev/dl/"
    MISSING=1
fi

if command -v git &>/dev/null; then
    ok "git"
else
    fail "git not found"
    MISSING=1
fi

if command -v docker &>/dev/null; then
    ok "Docker"
elif command -v nerdctl &>/dev/null; then
    ok "nerdctl (Docker alternative)"
elif command -v colima &>/dev/null; then
    ok "Colima (container runtime)"
else
    warn "No container runtime found (Docker, nerdctl, or Colima)"
    warn "Container-based deployment and e2e tests won't work"
    warn "Install Docker Desktop or run: brew install colima && colima start"
fi

if [ "$MISSING" -gt 0 ]; then
    echo ""
    fail "Missing required tools. Install them and re-run this script."
    exit 1
fi

echo ""

# ── Build ─────────────────────────────────────────────────────────────────────

info "Building cogos from source..."

cd "$REPO_DIR"

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w -X github.com/myrgic/cogos/internal/engine.Version=${VERSION} -X github.com/myrgic/cogos/internal/engine.BuildTime=${BUILD_TIME}"

go build -ldflags="$LDFLAGS" -o cogos ./cmd/cogos

ok "Built cogos ($VERSION)"
echo ""

# ── Install ───────────────────────────────────────────────────────────────────

info "Installing to $INSTALL_DIR..."

mkdir -p "$INSTALL_DIR"

# Refuse to clobber a running kernel before writing anything.
refuse_if_running "$INSTALL_DIR/cogos"

# Install cogos binary.
cp cogos "$INSTALL_DIR/cogos"
chmod +x "$INSTALL_DIR/cogos"
ok "cogos → $INSTALL_DIR/cogos"

# Install cog CLI wrapper.
cp scripts/cog "$INSTALL_DIR/cog"
chmod +x "$INSTALL_DIR/cog"
ok "cog   → $INSTALL_DIR/cog"

# Clean up build artifact from repo dir.
rm -f cogos

echo ""

# ── PATH ──────────────────────────────────────────────────────────────────────

if echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    ok "$INSTALL_DIR is already in PATH"
else
    info "Adding $INSTALL_DIR to PATH..."

    PROFILE=""
    case "$SHELL_NAME" in
        zsh)  PROFILE="$HOME/.zshrc" ;;
        bash)
            if [ -f "$HOME/.bash_profile" ]; then
                PROFILE="$HOME/.bash_profile"
            else
                PROFILE="$HOME/.bashrc"
            fi
            ;;
        *)    PROFILE="$HOME/.profile" ;;
    esac

    PATH_LINE="export PATH=\"$INSTALL_DIR:\$PATH\""

    if [ -n "$PROFILE" ] && ! grep -qF "$INSTALL_DIR" "$PROFILE" 2>/dev/null; then
        echo "" >> "$PROFILE"
        echo "# CogOS" >> "$PROFILE"
        echo "$PATH_LINE" >> "$PROFILE"
        ok "Added to $PROFILE"
        warn "Run 'source $PROFILE' or open a new terminal for PATH to take effect"
    elif [ -n "$PROFILE" ]; then
        ok "Already in $PROFILE"
    fi

    # Also export for this session.
    export PATH="$INSTALL_DIR:$PATH"
fi

echo ""

# ── Verify ────────────────────────────────────────────────────────────────────

info "Verifying installation..."

if "$INSTALL_DIR/cogos" version &>/dev/null; then
    VERSION_OUT=$("$INSTALL_DIR/cogos" version 2>&1)
    ok "cogos: $VERSION_OUT"
else
    fail "cogos binary not working"
    exit 1
fi

if [ -x "$INSTALL_DIR/cog" ]; then
    ok "cog CLI installed"
else
    fail "cog CLI not found"
fi

echo ""

# ── Summary ───────────────────────────────────────────────────────────────────

info "Setup complete!"
echo ""
echo "  Next steps:"
echo ""
echo "    # Initialize a workspace"
echo "    cogos init --workspace ~/my-project"
echo ""
echo "    # Start the daemon"
echo "    cogos serve --workspace ~/my-project"
echo ""
echo "    # Or use the cog CLI"
echo "    cd ~/my-project"
echo "    cog health"
echo ""
echo "  Dev commands:"
echo ""
echo "    make build        # rebuild"
echo "    make test         # unit tests"
echo "    make e2e-local    # end-to-end test"
echo "    make e2e          # e2e in container"
echo ""
