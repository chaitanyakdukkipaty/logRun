#!/bin/bash

# Upload release assets to GitHub
# Usage: ./upload-release.sh v1.0.1

set -e

VERSION=${1:-"v1.0.2"}
REPO="chaitanyakdukkipaty/logRun"

echo "📦 Uploading release assets for $VERSION..."

# Check if gh CLI is installed
if ! command -v gh &> /dev/null; then
    echo "❌ GitHub CLI (gh) is not installed."
    echo "Install it with: brew install gh"
    echo ""
    echo "Or upload manually:"
    echo "1. Go to https://github.com/$REPO/releases/tag/$VERSION"
    echo "2. Click 'Edit release'"
    echo "3. Upload all files from dist/ directory"
    exit 1
fi

# Upload each file
for file in dist/logrun-${VERSION}-*.{tar.gz,zip}; do
    if [ -f "$file" ]; then
        echo "Uploading $(basename $file)..."
        gh release upload "$VERSION" "$file" --repo "$REPO" --clobber
    fi
done

echo "✅ All assets uploaded successfully!"
echo "🎉 Users can now install with:"
echo "   curl -sSL https://raw.githubusercontent.com/$REPO/main/install.sh | bash"