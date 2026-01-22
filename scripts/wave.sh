#!/bin/bash
# wave.sh - Wave Terminal integration functions
#
# Provides wsh wrapper functions for cog-workspace integration.
# Only functional when WAVETERM=1 is set.

# Check if in Wave Terminal
cog_wave_check() {
    if [ "$WAVETERM" != "1" ]; then
        echo "Not running in Wave Terminal (WAVETERM not set)" >&2
        return 1
    fi
    return 0
}

# Show current Wave state
cog_wave_status() {
    cog_wave_check || return 1

    echo "=== Wave Terminal Status ==="
    echo "Workspace:     $(wsh getvar cog:workspace 2>/dev/null || echo 'not set')"
    echo "Branch:        $(wsh getvar cog:branch 2>/dev/null || echo 'not set')"
    echo "Status:        $(wsh getvar cog:status 2>/dev/null || echo 'not set')"
    echo "Session Start: $(wsh getvar cog:session_start 2>/dev/null || echo 'not set')"
    echo "Session ID:    $(wsh getvar cog:session_id 2>/dev/null || echo 'not set')"
    echo ""
    echo "Block ID:      ${WAVETERM_BLOCKID:-not set}"
    echo "Tab ID:        ${WAVETERM_TABID:-not set}"
    echo "Workspace ID:  ${WAVETERM_WORKSPACEID:-not set}"
}

# Send a notification
cog_wave_notify() {
    cog_wave_check || return 1
    local message="${1:-Cog notification}"
    wsh notify "$message"
}

# Set block title
cog_wave_title() {
    cog_wave_check || return 1
    local title="${1:-Cog}"
    wsh setmeta -b this "title=$title"
}

# Set block background color
cog_wave_bg() {
    cog_wave_check || return 1
    local color="${1:-}"
    wsh setmeta -b this "bg=$color"
}

# Preview a file in a new block
cog_wave_view() {
    cog_wave_check || return 1
    local file="${1:?Usage: cog wave view <file>}"
    wsh view "$file"
}

# Open URL in browser block
cog_wave_web() {
    cog_wave_check || return 1
    local url="${1:?Usage: cog wave web <url>}"
    wsh web open "$url"
}

# Send to AI sidebar
cog_wave_ai() {
    cog_wave_check || return 1
    local message="$*"
    if [ -z "$message" ]; then
        echo "Usage: cog wave ai <message>" >&2
        return 1
    fi
    echo "$message" | wsh ai - -m "$message"
}

# Set a persistent variable
cog_wave_set() {
    cog_wave_check || return 1
    local key="${1:?Usage: cog wave set <key> <value>}"
    local value="${2:-}"
    wsh setvar "${key}=${value}"
}

# Get a persistent variable
cog_wave_get() {
    cog_wave_check || return 1
    local key="${1:?Usage: cog wave get <key>}"
    wsh getvar "$key"
}

# Run command in new block
cog_wave_run() {
    cog_wave_check || return 1
    local cmd="$*"
    if [ -z "$cmd" ]; then
        echo "Usage: cog wave run <command>" >&2
        return 1
    fi
    wsh run "$cmd"
}

# Run command in magnified block
cog_wave_run_magnified() {
    cog_wave_check || return 1
    local cmd="$*"
    if [ -z "$cmd" ]; then
        echo "Usage: cog wave run-m <command>" >&2
        return 1
    fi
    wsh run -m "$cmd"
}

# Reinitialize Wave state (manual trigger)
cog_wave_init() {
    cog_wave_check || return 1
    python3 "$COG_DIR/hooks/session-start.d/12-wave-terminal-init.py"
}

# Wave help
cog_wave_help() {
    cat <<'EOF'
Wave Terminal Integration Commands

Usage: cog wave <command> [args]

Commands:
  status          Show current Wave state
  init            Reinitialize Wave state
  notify <msg>    Send toast notification
  title <title>   Set block title
  bg [color]      Set block background (empty to clear)
  view <file>     Preview file in new block
  web <url>       Open URL in browser block
  ai <message>    Send message to AI sidebar
  set <k> <v>     Set persistent variable
  get <key>       Get persistent variable
  run <cmd>       Run command in new block
  run-m <cmd>     Run command in magnified block

Environment:
  WAVETERM=1 must be set (automatically set by Wave Terminal)

Examples:
  cog wave status
  cog wave notify "Build complete!"
  cog wave title "Working on feature-x"
  cog wave view .cog/ontology/crystal.cog.md
  cog wave web "https://github.com/..."
EOF
}
