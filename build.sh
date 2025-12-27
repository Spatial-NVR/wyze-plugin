#!/bin/bash
set -e

echo "Building Wyze plugin..."

# Get the directory of this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Build for current platform
echo "Building for current platform..."
go build -o wyze-plugin .

# Build for multiple platforms
echo "Building for Linux AMD64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o wyze-plugin-linux-amd64 .

echo "Building for Linux ARM64..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o wyze-plugin-linux-arm64 .

echo "Building for macOS AMD64..."
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o wyze-plugin-darwin-amd64 .

echo "Building for macOS ARM64..."
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o wyze-plugin-darwin-arm64 .

echo "Build complete!"
echo ""
echo "Binaries:"
ls -la wyze-plugin*
