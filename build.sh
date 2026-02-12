#!/bin/bash

# Build script for LogRun project
set -e

VERSION=${1:-"dev"}
BUILD_ALL_PLATFORMS=${2:-false}

echo "🚀 Building LogRun v$VERSION..."

# Create bin and dist directories
mkdir -p bin dist

if [ "$BUILD_ALL_PLATFORMS" = "true" ]; then
    echo "📦 Building for multiple platforms..."
    
    # Cross-compilation for distribution
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
        
        echo "Building for $GOOS/$GOARCH..."
        
        cd cmd/logrun
        GOOS=$GOOS GOARCH=$GOARCH go build -ldflags="-s -w -X main.version=$VERSION" -o "../../dist/$output_name" .
        cd ../..
        
        # Create compressed archives
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
    echo "✅ CLI built successfully -> bin/logrun"
fi

echo "📦 Installing API dependencies..."
cd api
npm install
cd ..
echo "✅ API dependencies installed"

echo "📦 Building Web UI..."
cd web
npm install
npm run build
cd ..
echo "✅ Web UI built successfully -> web/dist"

echo "🎉 Build completed successfully!"
echo ""
echo "Quick start:"
echo "1. Start API:    cd api && npm start"
echo "2. Start Web UI: cd web && npm run preview"
echo "3. Use CLI:      ./bin/logrun [command]"