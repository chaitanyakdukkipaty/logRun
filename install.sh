#!/bin/bash

# LogRun Installation Script
# Usage: curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash
# Debug: DEBUG=true curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash

set -e

# Configuration
REPO="chaitanyakdukkipaty/logRun" 
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="logrun"
DEBUG=${DEBUG:-false}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1" >&2
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

log_debug() {
    if [ "$DEBUG" = "true" ]; then
        echo -e "${YELLOW}[DEBUG]${NC} $1" >&2
    fi
}

# Detect OS and architecture
detect_platform() {
    local os
    local arch
    
    case "$(uname -s)" in
        Darwin*)
            os="darwin"
            ;;
        Linux*)
            os="linux"
            ;;
        CYGWIN*|MINGW32*|MSYS*|MINGW*)
            os="windows"
            ;;
        *)
            log_error "Unsupported operating system: $(uname -s)"
            exit 1
            ;;
    esac
    
    case "$(uname -m)" in
        x86_64|amd64)
            arch="amd64"
            ;;
        arm64|aarch64)
            arch="arm64"
            ;;
        *)
            log_error "Unsupported architecture: $(uname -m)"
            exit 1
            ;;
    esac
    
    echo "${os}-${arch}"
}

# Get latest release version
get_latest_version() {
    local version
    version=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    
    if [ -z "$version" ]; then
        log_error "Failed to get latest version from GitHub API"
        log_error "This could be due to rate limiting or network issues"
        log_error "You can also check manually at: https://github.com/${REPO}/releases/latest"
        exit 1
    fi
    
    # Debug: show what we got from the API call
    log_info "GitHub API returned version: $version"
    
    echo "$version"
}

# Download and install binary
install_binary() {
    local version="$1"
    local platform="$2"
    local tmp_dir
    
    tmp_dir=$(mktemp -d)
    
    log_info "Downloading LogRun $version for $platform..."
    
    local download_url="https://github.com/${REPO}/releases/download/${version}/logrun-${version}-${platform}.tar.gz"
    
    log_info "Download URL: $download_url"
    log_debug "Using temp directory: $tmp_dir"
    
    # Check if the file exists first
    local http_status
    http_status=$(curl -s -I "$download_url" | head -n 1 | cut -d' ' -f2)
    
    if [ "$http_status" != "200" ] && [ "$http_status" != "302" ]; then
        log_error "Release file not found at: $download_url"
        log_error "HTTP Status: $http_status"
        log_error "Please check if the release has been properly built and uploaded."
        log_error "Visit: https://github.com/${REPO}/releases"
        exit 1
    fi
    
    # Download file to temp location first for inspection
    local archive_file="$tmp_dir/logrun.tar.gz"
    log_info "Downloading to: $archive_file"
    
    if ! curl -sL "$download_url" -o "$archive_file"; then
        log_error "Failed to download LogRun"
        exit 1
    fi
    
    # Check if it's actually a tar.gz file
    local file_type
    if command -v file >/dev/null 2>&1; then
        file_type=$(file "$archive_file" 2>/dev/null || echo "unknown")
        log_info "Downloaded file type: $file_type"
        
        if [[ "$file_type" != *"gzip compressed"* ]]; then
            log_error "Downloaded file is not a valid gzip archive"
            log_error "File content (first 200 chars):"
            head -c 200 "$archive_file" | cat -v
            exit 1
        fi
    else
        log_warn "file command not available, skipping file type check"
        # Try to verify it's gzip by checking magic bytes
        local magic_bytes
        magic_bytes=$(head -c 2 "$archive_file" | od -x | head -n1 | awk '{print $2}')
        if [ "$magic_bytes" != "8b1f" ]; then
            log_error "Downloaded file does not appear to be a gzip archive"
            log_error "File content (first 200 chars):"
            head -c 200 "$archive_file" | cat -v
            exit 1
        fi
    fi
    
    # Extract the archive
    if ! tar -xzf "$archive_file" -C "$tmp_dir"; then
        log_error "Failed to extract LogRun archive"
        log_error "Archive contents:"
        tar -tzf "$archive_file" 2>/dev/null || echo "Cannot list archive contents"
        exit 1
    fi
    
    # Find the binary in the extracted archive
    local binary_path
    
    # Try different possible binary names
    local possible_names=(
        "logrun-${version}-${platform}"
        "logrun"
        "${BINARY_NAME}"
    )
    
    log_info "Looking for binary in extracted files..."
    log_info "Archive contents:"
    ls -la "$tmp_dir"
    
    for name in "${possible_names[@]}"; do
        if [ -f "$tmp_dir/$name" ]; then
            binary_path="$tmp_dir/$name"
            log_info "Found binary: $name"
            break
        fi
    done
    
    if [ -z "$binary_path" ]; then
        log_error "Binary not found in downloaded archive"
        log_error "Tried the following names: ${possible_names[*]}"
        log_error "Available files:"
        find "$tmp_dir" -type f -executable 2>/dev/null || find "$tmp_dir" -type f
        exit 1
    fi
    
    log_info "Installing LogRun to $INSTALL_DIR/$BINARY_NAME..."
    
    # Try to install with sudo if needed
    if ! cp "$binary_path" "$INSTALL_DIR/$BINARY_NAME" 2>/dev/null; then
        log_warn "Permission denied, trying with sudo..."
        sudo cp "$binary_path" "$INSTALL_DIR/$BINARY_NAME"
    fi
    
    # Make executable
    if ! chmod +x "$INSTALL_DIR/$BINARY_NAME" 2>/dev/null; then
        sudo chmod +x "$INSTALL_DIR/$BINARY_NAME"
    fi
    
    # Clean up
    rm -rf "$tmp_dir"
    
    log_info "LogRun installed successfully!"
}

# Main installation process
main() {
    log_info "Installing LogRun..."
    
    # Check if curl is available
    if ! command -v curl >/dev/null 2>&1; then
        log_error "curl is required but not installed. Please install curl and try again."
        exit 1
    fi
    
    # Check if tar is available
    if ! command -v tar >/dev/null 2>&1; then
        log_error "tar is required but not installed. Please install tar and try again."
        exit 1
    fi
    
    local platform
    platform=$(detect_platform)
    log_info "Detected platform: $platform"
    
    local version
    version=$(get_latest_version)
    log_info "Latest version: $version"
    
    install_binary "$version" "$platform"
    
    # Verify installation
    if command -v "$BINARY_NAME" >/dev/null 2>&1; then
        log_info "Verification successful!"
        log_info "LogRun is now available in your PATH"
    else
        log_warn "LogRun was installed but may not be in your PATH."
        log_warn "Try running: export PATH=\"$INSTALL_DIR:\$PATH\""
    fi
    
    echo ""
    log_info "🎉 LogRun installation complete!"
    log_info "Get started with: $BINARY_NAME --help"
}

main "$@"