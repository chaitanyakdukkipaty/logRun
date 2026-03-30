#!/bin/bash

# LogRun Uninstall Script
# Usage: curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/uninstall.sh | bash
# Keep data: KEEP_DATA=true curl -sSL .../uninstall.sh | bash

set -e

# Configuration
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="logrun"
STATE_FILE=".logrun-services.json"
KEEP_DATA=${KEEP_DATA:-false}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1" >&2
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

# Stop any running LogRun services
stop_services() {
    log_info "Stopping any running LogRun services..."

    # Kill processes listening on the default ports (3001 API, 3000 web)
    for port in 3001 3000; do
        local pid
        pid=$(lsof -ti tcp:"$port" 2>/dev/null || true)
        if [ -n "$pid" ]; then
            log_info "  Stopping process on port $port (PID $pid)..."
            kill "$pid" 2>/dev/null || true
        fi
    done

    # Also check the state file for non-default ports
    if [ -f "$STATE_FILE" ]; then
        for key in api_port web_port; do
            local port
            port=$(grep -o "\"${key}\":[[:space:]]*[0-9]*" "$STATE_FILE" 2>/dev/null | grep -o '[0-9]*$' || true)
            if [ -n "$port" ] && [ "$port" != "3001" ] && [ "$port" != "3000" ]; then
                local pid
                pid=$(lsof -ti tcp:"$port" 2>/dev/null || true)
                if [ -n "$pid" ]; then
                    log_info "  Stopping process on port $port (PID $pid)..."
                    kill "$pid" 2>/dev/null || true
                fi
            fi
        done
    fi
}

# Remove the binary
remove_binary() {
    local binary_path="$INSTALL_DIR/$BINARY_NAME"

    if [ -f "$binary_path" ]; then
        log_info "Removing $binary_path..."
        if ! rm -f "$binary_path" 2>/dev/null; then
            log_warn "Permission denied, trying with sudo..."
            sudo rm -f "$binary_path"
        fi
        log_info "  ✓ Binary removed"
    else
        log_warn "Binary not found at $binary_path (already removed?)"
    fi
}

# Remove runtime data (logs, database, state file)
remove_data() {
    if [ "$KEEP_DATA" = "true" ]; then
        log_info "KEEP_DATA=true — skipping data removal."
        return
    fi

    log_info "Removing LogRun runtime data..."

    # State file (project-local, remove from cwd if present)
    if [ -f "$STATE_FILE" ]; then
        rm -f "$STATE_FILE"
        log_info "  ✓ Removed $STATE_FILE"
    fi

    # Database
    if [ -f "logrun.db" ]; then
        rm -f "logrun.db"
        log_info "  ✓ Removed logrun.db"
    fi

    # Log directory
    if [ -d "logs" ] && ls logs/*.log 2>/dev/null | head -1 | grep -q '.'; then
        log_warn "  Log files found in ./logs/ — remove manually if no longer needed:"
        log_warn "    rm -rf ./logs"
    fi
}

main() {
    echo ""
    log_info "Uninstalling LogRun..."
    echo ""

    stop_services
    remove_binary
    remove_data

    echo ""
    log_info "✅ LogRun has been uninstalled."

    if [ "$KEEP_DATA" != "true" ]; then
        log_info "   Runtime data removed. Log files in ./logs/ were left in place."
    else
        log_info "   Runtime data kept (KEEP_DATA=true)."
    fi

    log_info "   To reinstall: curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash"
    echo ""
}

main "$@"
