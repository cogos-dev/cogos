#!/usr/bin/env bash
# setup.sh — End-user install for CogOS
#
# Downloads the latest release binary and installs cogos + cog.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cogos-dev/cogos/main/scripts/setup.sh | sh
#
# Or clone and run locally:
#   ./scripts/setup.sh

set -euo pipefail

REPO="cogos-dev/cogos"
INSTALL_DIR="$HOME/.cogos/bin"

# ── Colors ────────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${BOLD}$*${NC}"; }
ok()    { echo -e "  ${GREEN}✓${NC} $*"; }
warn()  { echo -e "  ${YELLOW}!${NC} $*"; }
fail()  { echo -e "  ${RED}✗${NC} $*"; exit 1; }

# ── Detect platform ──────────────────────────────────────────────────────────

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin) os="darwin" ;;
        Linux)  os="linux" ;;
        *)      fail "Unsupported OS: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *)             fail "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}-${arch}"
}

# ── Find latest release ──────────────────────────────────────────────────────

get_latest_version() {
    if command -v gh &>/dev/null; then
        gh api "repos/$REPO/releases/latest" --jq '.tag_name' 2>/dev/null && return
    fi

    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
        | grep '"tag_name"' \
        | head -1 \
        | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}

# ── Main ──────────────────────────────────────────────────────────────────────

info "CogOS Installer"
echo ""

PLATFORM=$(detect_platform)
ok "Platform: $PLATFORM"

info "Finding latest release..."
VERSION=$(get_latest_version)

if [ -z "$VERSION" ]; then
    fail "Could not determine latest version. Check https://github.com/$REPO/releases"
fi

ok "Version: $VERSION"
echo ""

# Download binary.
BINARY_NAME="cogos-${PLATFORM}"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$BINARY_NAME"

info "Downloading $BINARY_NAME..."
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

if ! curl -fsSL -o "$TMPDIR/cogos" "$DOWNLOAD_URL"; then
    fail "Download failed. Check that $VERSION has a release for $PLATFORM"
fi
chmod +x "$TMPDIR/cogos"
ok "Downloaded"

# Download cog CLI wrapper.
COG_URL="https://raw.githubusercontent.com/$REPO/$VERSION/scripts/cog"
if curl -fsSL -o "$TMPDIR/cog" "$COG_URL" 2>/dev/null; then
    chmod +x "$TMPDIR/cog"
    ok "Downloaded cog CLI"
else
    warn "Could not download cog CLI wrapper (non-fatal)"
fi

echo ""

# Install.
info "Installing to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"

mv "$TMPDIR/cogos" "$INSTALL_DIR/cogos"
ok "cogos → $INSTALL_DIR/cogos"

if [ -f "$TMPDIR/cog" ]; then
    mv "$TMPDIR/cog" "$INSTALL_DIR/cog"
    ok "cog   → $INSTALL_DIR/cog"
fi

echo ""

# PATH setup.
SHELL_NAME="$(basename "$SHELL")"

if echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    ok "$INSTALL_DIR is already in PATH"
else
    PROFILE=""
    case "$SHELL_NAME" in
        zsh)  PROFILE="$HOME/.zshrc" ;;
        bash)
            [ -f "$HOME/.bash_profile" ] && PROFILE="$HOME/.bash_profile" || PROFILE="$HOME/.bashrc"
            ;;
        *)    PROFILE="$HOME/.profile" ;;
    esac

    if [ -n "$PROFILE" ] && ! grep -qF '.cogos/bin' "$PROFILE" 2>/dev/null; then
        echo "" >> "$PROFILE"
        echo "# CogOS" >> "$PROFILE"
        echo 'export PATH="$HOME/.cogos/bin:$PATH"' >> "$PROFILE"
        ok "Added to $PROFILE"
        warn "Run 'source $PROFILE' or open a new terminal"
    fi

    export PATH="$INSTALL_DIR:$PATH"
fi

echo ""

# Verify.
info "Verifying..."
VERSION_OUT=$("$INSTALL_DIR/cogos" version 2>&1)
ok "$VERSION_OUT"

echo ""
info "Installation complete!"
echo ""
echo "  Quick start:"
echo ""
echo "    cogos init --workspace ~/my-project"
echo "    cogos serve --workspace ~/my-project"
echo "    curl http://localhost:6931/health"
echo ""
