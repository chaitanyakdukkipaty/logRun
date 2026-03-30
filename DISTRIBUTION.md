# LogRun Distribution Guide

This document explains how to build and distribute LogRun binaries for multiple platforms.

## Building for Distribution

### 1. Single Platform Build (Development)
```bash
./build.sh
```

### 2. Multi-Platform Build (Distribution)
```bash
./build.sh v1.0.0 true
```

This creates binaries for:
- Linux (amd64, arm64)
- macOS (Intel, Apple Silicon)
- Windows (amd64)

## Distribution Methods

### 1. GitHub Releases (Recommended)

1. **Tag your release:**
   ```bash
   git tag -a v1.0.0 -m "Release v1.0.0"
   git push origin v1.0.0
   ```

2. **Build all platforms:**
   ```bash
   ./build.sh v1.0.0 true
   ```

3. **Upload to GitHub Releases:**
   - Go to your GitHub repo → Releases → Create new release
   - Upload all files from `dist/` directory
   - Add release notes

### 2. One-Line Installation

Users can install with:
```bash
curl -sSL https://raw.githubusercontent.com/yourusername/logrun/main/install.sh | bash
```

### 3. Package Managers

#### Homebrew (macOS)
Create a Homebrew formula:

```ruby
# Formula/logrun.rb
class Logrun < Formula
  desc "Process monitoring and log streaming tool"
  homepage "https://github.com/yourusername/logrun"
  url "https://github.com/yourusername/logrun/archive/v1.0.0.tar.gz"
  sha256 "your-sha256-here"

  depends_on "go" => :build

  def install
    system "go", "build", "-ldflags", "-s -w", "-o", bin/"logrun", "cmd/logrun/main.go"
  end

  test do
    assert_match "logrun version", shell_output("#{bin}/logrun --version")
  end
end
```

#### Linux Packages
For Ubuntu/Debian, create `.deb` packages:

```bash
# Install fpm
gem install fpm

# Create .deb package
fpm -s dir -t deb -n logrun -v 1.0.0 \
    --description "Process monitoring and log streaming tool" \
    --url "https://github.com/yourusername/logrun" \
    --license "MIT" \
    --maintainer "Your Name <your.email@example.com>" \
    bin/logrun=/usr/local/bin/logrun
```

### 4. Docker Distribution

```dockerfile
# Dockerfile
FROM scratch
COPY bin/logrun /logrun
ENTRYPOINT ["/logrun"]
```

Build and publish:
```bash
docker build -t yourusername/logrun:v1.0.0 .
docker push yourusername/logrun:v1.0.0
```

## File Sizes and Optimization

The build script uses these optimizations:
- `-ldflags="-s -w"` - Strip debug info and symbol table
- Cross-compilation for multiple architectures
- Compressed archives (tar.gz/zip)

Expected binary sizes:
- ~8-15MB per platform (depending on dependencies)

## Security Considerations

1. **Code Signing** (macOS/Windows):
   - Sign binaries for better user trust
   - Use certificates from Apple/Microsoft

2. **Checksums**:
   - Generate SHA256 checksums for all binaries
   - Include in release notes

3. **GPG Signatures**:
   - Sign releases with GPG key
   - Provide verification instructions

## Usage Instructions for End Users

### Direct Download
1. Go to [Releases](https://github.com/yourusername/logrun/releases)
2. Download the appropriate binary for your platform
3. Extract and move to PATH: `mv logrun /usr/local/bin/`

### Installation Script
```bash
curl -sSL https://raw.githubusercontent.com/yourusername/logrun/main/install.sh | bash
```

### Docker
```bash
docker run --rm yourusername/logrun:latest --help
```

### Homebrew (macOS)
```bash
brew tap yourusername/logrun
brew install logrun
```

## Testing Distribution

Test on different platforms:
```bash
# Test the installation script
./install.sh

# Verify functionality
logrun --help
logrun --version
```