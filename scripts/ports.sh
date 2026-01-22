#!/bin/bash
# .cog/lib/shell/ports.sh - Port Registry Management
#
# Provides port registry operations:
#   cog_ports_list       - List all registered ports
#   cog_ports_check      - Check if a port is available
#   cog_ports_status     - Show status of all registered ports
#   cog_ports_validate   - Validate registry consistency
#
# Author: Cog
# Created: 2026-01-20

set -e

# Registry location
PORTS_REGISTRY="${COG_DIR:-$(dirname "$0")/../..}/conf/ports.cog.md"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# =============================================================================
# HELPERS
# =============================================================================

# Extract the YAML ports block from the cogdoc
_extract_ports_yaml() {
    if [ ! -f "$PORTS_REGISTRY" ]; then
        echo "Error: Port registry not found at $PORTS_REGISTRY" >&2
        return 1
    fi

    # Extract the machine-readable YAML block containing ports:
    # Look for the block that starts with "ports:" at root level
    sed -n '/^```yaml/,/^```/p' "$PORTS_REGISTRY" | \
        sed '1d;$d' | \
        grep -A100 "^ports:" | \
        head -100
}

# Parse ports into a simple format: port service_id status
_parse_ports() {
    _extract_ports_yaml 2>/dev/null | \
        awk '
            /^  [a-z]/ { service = $1; gsub(/:/, "", service) }
            /port:/ { port = $2 }
            /status:/ {
                status = $2
                if (port && service) {
                    print port, service, status
                }
            }
        '
}

# =============================================================================
# COMMANDS
# =============================================================================

# List all registered ports
cog_ports_list() {
    echo -e "${BLUE}CogOS Port Registry${NC}"
    echo "===================="
    echo ""
    printf "%-6s %-20s %-12s\n" "PORT" "SERVICE" "STATUS"
    printf "%-6s %-20s %-12s\n" "----" "-------" "------"

    _parse_ports | while read -r port service status; do
        case "$status" in
            active)     color="${GREEN}" ;;
            planned)    color="${YELLOW}" ;;
            dev-only)   color="${BLUE}" ;;
            on-demand)  color="${YELLOW}" ;;
            *)          color="${NC}" ;;
        esac
        printf "%-6s %-20s ${color}%-12s${NC}\n" "$port" "$service" "$status"
    done

    echo ""
    echo "Registry: $PORTS_REGISTRY"
}

# Check if a port is registered/available
cog_ports_check() {
    local port="$1"

    if [ -z "$port" ]; then
        echo "Usage: cog ports check <port>" >&2
        return 1
    fi

    local found=false
    local service=""
    local status=""

    while read -r p s st; do
        if [ "$p" = "$port" ]; then
            found=true
            service="$s"
            status="$st"
            break
        fi
    done < <(_parse_ports)

    if [ "$found" = true ]; then
        echo -e "Port ${GREEN}$port${NC} is registered"
        echo "  Service: $service"
        echo "  Status:  $status"

        # Check if actually in use
        if lsof -i ":$port" >/dev/null 2>&1; then
            echo -e "  Bound:   ${GREEN}Yes${NC} (process listening)"
        else
            echo -e "  Bound:   ${YELLOW}No${NC} (not listening)"
        fi
        return 0
    else
        echo -e "Port ${RED}$port${NC} is NOT registered"
        echo ""
        echo "To register, add to: $PORTS_REGISTRY"
        return 1
    fi
}

# Show status of all ports (with live checks)
cog_ports_status() {
    echo -e "${BLUE}CogOS Port Status${NC}"
    echo "=================="
    echo ""
    printf "%-6s %-20s %-12s %-8s\n" "PORT" "SERVICE" "STATUS" "BOUND"
    printf "%-6s %-20s %-12s %-8s\n" "----" "-------" "------" "-----"

    _parse_ports | while read -r port service status; do
        if lsof -i ":$port" >/dev/null 2>&1; then
            bound="${GREEN}Yes${NC}"
        else
            bound="${RED}No${NC}"
        fi

        case "$status" in
            active)     scolor="${GREEN}" ;;
            planned)    scolor="${YELLOW}" ;;
            dev-only)   scolor="${BLUE}" ;;
            on-demand)  scolor="${YELLOW}" ;;
            *)          scolor="${NC}" ;;
        esac

        printf "%-6s %-20s ${scolor}%-12s${NC} ${bound}\n" "$port" "$service" "$status"
    done

    echo ""
}

# Validate registry consistency
cog_ports_validate() {
    echo -e "${BLUE}Validating Port Registry${NC}"
    echo ""

    local errors=0
    local warnings=0
    local seen_ports=""

    while read -r port service status; do
        # Check for duplicates
        if echo "$seen_ports" | grep -qw "$port"; then
            echo -e "${RED}ERROR${NC}: Port $port is registered multiple times"
            ((errors++))
        fi
        seen_ports="$seen_ports $port"

        # Check port ranges
        case "$port" in
            51[0-9][0-9])
                if [ "$status" != "active" ] && [ "$status" != "planned" ]; then
                    echo -e "${YELLOW}WARN${NC}: Core port $port has status '$status' (expected active/planned)"
                    ((warnings++))
                fi
                ;;
            80[0-9][0-9])
                # MCP range - OK
                ;;
            84[0-9][0-9]|87[0-9][0-9])
                # Web app ranges - OK
                ;;
            5173|30[0-9][0-9])
                if [ "$status" != "dev-only" ] && [ "$status" != "reserved" ]; then
                    echo -e "${YELLOW}WARN${NC}: Dev port $port should be 'dev-only' or 'reserved'"
                    ((warnings++))
                fi
                ;;
        esac
    done < <(_parse_ports)

    echo ""
    if [ $errors -eq 0 ] && [ $warnings -eq 0 ]; then
        echo -e "${GREEN}✓ Registry is valid${NC}"
    elif [ $errors -eq 0 ]; then
        echo -e "${YELLOW}⚠ Registry has $warnings warning(s)${NC}"
    else
        echo -e "${RED}✗ Registry has $errors error(s), $warnings warning(s)${NC}"
        return 1
    fi
}

# =============================================================================
# MAIN
# =============================================================================

# If sourced, export functions. If executed directly, run command.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    cmd="${1:-list}"
    shift 2>/dev/null || true

    case "$cmd" in
        list)     cog_ports_list "$@" ;;
        check)    cog_ports_check "$@" ;;
        status)   cog_ports_status "$@" ;;
        validate) cog_ports_validate "$@" ;;
        help|-h|--help)
            echo "Usage: cog ports <command>"
            echo ""
            echo "Commands:"
            echo "  list      List all registered ports"
            echo "  check     Check if a port is registered"
            echo "  status    Show status with live checks"
            echo "  validate  Validate registry consistency"
            ;;
        *)
            echo "Unknown command: $cmd" >&2
            echo "Run 'cog ports help' for usage" >&2
            exit 1
            ;;
    esac
fi
