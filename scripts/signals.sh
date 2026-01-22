#!/bin/sh
# .cog/lib/signals.sh - Signal layer shell interface
#
# Mutable, time-decaying state for real-time queries.
# Unlike events (immutable ledger), signals are the "now" of the system.
#
# Usage:
#   source .cog/lib/signals.sh
#   cog_signals_list
#   cog_signals_get "inference.active"
#   cog_signals_emit "focus.current" '{"task": "implementing signals"}'
#   cog_signals_emit_ttl "session.heartbeat" '{}' 300
#   cog_signals_clear "focus.current"

# Workspace root detection
# COG_ROOT and COG_DIR are set by the cog wrapper when sourcing libs
# Fall back to relative path detection if sourced directly
if [ -z "$COG_ROOT" ]; then
    PROJECT_ROOT="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "$0")/../.." && pwd)}"
else
    PROJECT_ROOT="$COG_ROOT"
fi

# COG_DIR for consistency with other libs
: "${COG_DIR:=${PROJECT_ROOT}/.cog}"

# Signal library location
COG_SIGNAL_LIB="${COG_DIR}/scripts/signal_lib.py"

# =============================================================================
# SIGNAL QUERIES
# =============================================================================

# List active signals
# Usage: cog_signals_list [prefix]
cog_signals_list() {
    local prefix="${1:-}"
    if [ -n "$prefix" ]; then
        python3 "$COG_SIGNAL_LIB" list "$prefix"
    else
        python3 "$COG_SIGNAL_LIB" list
    fi
}

# Get a specific signal
# Usage: cog_signals_get <type>
cog_signals_get() {
    local signal_type="${1:?Signal type required}"
    python3 "$COG_SIGNAL_LIB" get "$signal_type"
}

# Emit a signal (create or replace)
# Usage: cog_signals_emit <type> <json>
cog_signals_emit() {
    local signal_type="${1:?Signal type required}"
    local json_data="${2:?JSON data required}"
    python3 "$COG_SIGNAL_LIB" emit "$signal_type" "$json_data"
}

# Emit a signal with TTL
# Usage: cog_signals_emit_ttl <type> <json> <ttl_seconds>
cog_signals_emit_ttl() {
    local signal_type="${1:?Signal type required}"
    local json_data="${2:?JSON data required}"
    local ttl="${3:?TTL in seconds required}"
    python3 "$COG_SIGNAL_LIB" emit "$signal_type" "$json_data" "$ttl"
}

# Clear a specific signal
# Usage: cog_signals_clear <type>
cog_signals_clear() {
    local signal_type="${1:?Signal type required}"
    python3 "$COG_SIGNAL_LIB" clear "$signal_type"
}

# Clear all signals
# Usage: cog_signals_clear_all
cog_signals_clear_all() {
    python3 "$COG_SIGNAL_LIB" clear-all
}

# Clear expired signals
# Usage: cog_signals_clear_expired
cog_signals_clear_expired() {
    python3 "$COG_SIGNAL_LIB" clear-expired
}

# Get signal summary
# Usage: cog_signals_summary
cog_signals_summary() {
    python3 "$COG_SIGNAL_LIB" summary
}

# =============================================================================
# FORMATTED OUTPUT
# =============================================================================

# Show signals with formatted output
# Usage: cog_signals_status
cog_signals_status() {
    echo "Active Signals"
    echo "=============="
    echo ""
    python3 "$COG_SIGNAL_LIB" summary | python3 -c "
import sys, json
summary = json.load(sys.stdin)
total = summary.get('total', 0)
print(f'Total active: {total}')
print('')
if total > 0:
    by_prefix = summary.get('by_prefix', {})
    if by_prefix:
        print('By category:')
        for prefix, count in sorted(by_prefix.items()):
            print(f'  {prefix:20s} {count}')
        print('')

    signals = summary.get('signals', [])
    if signals:
        print('Signals:')
        for s in signals:
            sig_type = s.get('type', 'unknown')
            expires = s.get('expires', '')
            ttl_mark = ' [TTL]' if expires else ''
            print(f'  {sig_type}{ttl_mark}')
"
}

# Check if a signal exists
# Usage: cog_signals_exists <type>
# Returns: 0 if exists, 1 if not
cog_signals_exists() {
    local signal_type="${1:?Signal type required}"
    python3 "$COG_SIGNAL_LIB" get "$signal_type" >/dev/null 2>&1
}

# Get signal data field (extracts from JSON)
# Usage: cog_signals_data <type> [field]
cog_signals_data() {
    local signal_type="${1:?Signal type required}"
    local field="${2:-}"

    if [ -z "$field" ]; then
        # Return entire data object
        python3 -c "
import sys
sys.path.insert(0, '${COG_DIR}/scripts')
from signal_lib import get_signal
import json
signal = get_signal('${signal_type}')
if signal and 'data' in signal:
    print(json.dumps(signal['data'], indent=2))
"
    else
        # Return specific field from data
        python3 -c "
import sys
sys.path.insert(0, '${COG_DIR}/scripts')
from signal_lib import get_signal
signal = get_signal('${signal_type}')
if signal and 'data' in signal:
    value = signal['data'].get('${field}')
    if value is not None:
        print(value)
"
    fi
}

# Get signal age in seconds
# Usage: cog_signals_age <type>
cog_signals_age() {
    local signal_type="${1:?Signal type required}"
    python3 -c "
import sys
sys.path.insert(0, '${COG_DIR}/scripts')
from signal_lib import get_signal_age
age = get_signal_age('${signal_type}')
if age is not None:
    print(f'{age:.1f}')
"
}

# Get signal remaining TTL in seconds
# Usage: cog_signals_ttl <type>
cog_signals_ttl() {
    local signal_type="${1:?Signal type required}"
    python3 -c "
import sys
sys.path.insert(0, '${COG_DIR}/scripts')
from signal_lib import get_signal_ttl
ttl = get_signal_ttl('${signal_type}')
if ttl is not None:
    print(f'{ttl:.1f}')
"
}

# =============================================================================
# COMMON SIGNAL PATTERNS
# =============================================================================

# Emit a heartbeat signal (useful for session liveness)
# Usage: cog_signals_heartbeat [ttl_seconds]
cog_signals_heartbeat() {
    local ttl="${1:-300}"  # Default 5 minutes
    local now
    now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    python3 "$COG_SIGNAL_LIB" emit "session.heartbeat" "{\"timestamp\": \"${now}\"}" "$ttl"
}

# Set current focus
# Usage: cog_signals_focus <description>
cog_signals_focus() {
    local description="${1:?Focus description required}"
    local now
    now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    python3 "$COG_SIGNAL_LIB" emit "focus.current" "{\"task\": \"${description}\", \"started\": \"${now}\"}"
}

# Clear current focus
# Usage: cog_signals_focus_clear
cog_signals_focus_clear() {
    python3 "$COG_SIGNAL_LIB" clear "focus.current"
}

# Set kernel status
# Usage: cog_signals_kernel_status <status> [details_json]
cog_signals_kernel_status() {
    local status="${1:?Status required}"
    local details="${2:-{}}"
    local now
    now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

    # Merge status into details
    python3 -c "
import json
details = json.loads('${details}')
details['status'] = '${status}'
details['updated'] = '${now}'
print(json.dumps(details))
" | xargs -I{} python3 "$COG_SIGNAL_LIB" emit "kernel.status" '{}'
}

# Mark inference as active
# Usage: cog_signals_inference_start <request_id> [model] [ttl_seconds]
cog_signals_inference_start() {
    local request_id="${1:?Request ID required}"
    local model="${2:-unknown}"
    local ttl="${3:-600}"  # Default 10 minutes
    local now
    now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    python3 "$COG_SIGNAL_LIB" emit "inference.active" \
        "{\"request_id\": \"${request_id}\", \"model\": \"${model}\", \"started\": \"${now}\"}" \
        "$ttl"
}

# Clear inference signal
# Usage: cog_signals_inference_end
cog_signals_inference_end() {
    python3 "$COG_SIGNAL_LIB" clear "inference.active"
}
