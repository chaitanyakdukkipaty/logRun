#!/bin/bash

# Build script for LogRun project
# The web UI MUST be built before Go so //go:embed web/dist/* finds the files.
set -e

VERSION=${1:-"dev"}
BUILD_ALL_PLATFORMS=${2:-false}

echo "🚀 Building LogRun v$VERSION..."

# Create bin and dist directories
mkdir -p bin dist

# ── Step 1: Build Web UI (must come before Go build for go:embed) ─────────────
echo "📦 Building Web UI (required for binary embedding)..."
cd web
npm install --silent
npm run build
cd ..
echo "✅ Web UI built -> web/dist/"

# Copy built assets into cmd/logrun/web/dist/ so //go:embed finds them.
echo "📋 Copying web assets to cmd/logrun/web/dist/..."
rm -rf cmd/logrun/web/dist
cp -r web/dist cmd/logrun/web/dist
echo "✅ Assets copied -> cmd/logrun/web/dist/"

# ── Step 2: Build Go CLI (embeds web/dist at compile time) ────────────────────
if [ "$BUILD_ALL_PLATFORMS" = "true" ]; then
    echo "📦 Building Go CLI for all platforms..."

    platforms=(
        "linux/amd64"
        "linux/arm64"
        "darwin/amd64"
        "darwin/arm64"
        "windows/amd64"
    )

    for platform in "${platforms[@]}"; do
        platform_split=(${platform//\// })
        GOOS=${platform_split[0]}
        GOARCH=${platform_split[1]}

        output_name="logrun-$VERSION-$GOOS-$GOARCH"
        if [ $GOOS = "windows" ]; then
            output_name+='.exe'
        fi

        echo "  Building $GOOS/$GOARCH..."
        cd cmd/logrun
        GOOS=$GOOS GOARCH=$GOARCH go build -ldflags="-s -w -X main.version=$VERSION" -o "../../dist/$output_name" .
        cd ../..

        if [ $GOOS = "windows" ]; then
            (cd dist && zip "logrun-$VERSION-$GOOS-$GOARCH.zip" "$output_name" && rm "$output_name")
        else
            (cd dist && tar -czf "logrun-$VERSION-$GOOS-$GOARCH.tar.gz" "$output_name" && rm "$output_name")
        fi
    done

    echo "✅ Cross-platform builds complete in dist/"
    ls -la dist/
else
    echo "📦 Building Go CLI for current platform..."
    cd cmd/logrun
    go mod tidy
    go build -ldflags="-s -w -X main.version=$VERSION" -o ../../bin/logrun .
    cd ../..
    echo "✅ CLI built -> bin/logrun"
fi

echo ""
echo "🎉 Build complete! The binary is self-contained — no Node.js or repo required."
echo ""
echo "Quick start:"
echo "  ./bin/logrun --help"
echo "  ./bin/logrun echo 'hello world'"
echo ""
echo "Web UI available at http://localhost:4000 after first run."