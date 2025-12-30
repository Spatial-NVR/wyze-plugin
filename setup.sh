#!/bin/bash
# Setup script for Wyze plugin
# Gets wyzecam library from submodule or downloads it

set -e

PLUGIN_DIR="$(cd "$(dirname "$0")" && pwd)"
WYZECAM_VERSION="2.10.2"

echo "Setting up Wyze plugin..."

# Create venv if it doesn't exist
if [ ! -d "$PLUGIN_DIR/venv" ]; then
    echo "Creating Python virtual environment..."
    python3 -m venv "$PLUGIN_DIR/venv"
fi

# Activate venv
source "$PLUGIN_DIR/venv/bin/activate"

# Install requirements
echo "Installing Python dependencies..."
pip install -r "$PLUGIN_DIR/requirements.txt"

# Get wyzecam library if not present
if [ ! -d "$PLUGIN_DIR/wyzecam" ]; then
    # First try: copy from submodule if it exists
    if [ -d "$PLUGIN_DIR/wyze-bridge/app/wyzecam" ]; then
        echo "Copying wyzecam library from submodule..."
        cp -r "$PLUGIN_DIR/wyze-bridge/app/wyzecam" "$PLUGIN_DIR/"
        echo "wyzecam library copied from submodule"
    else
        # Fallback: download via curl
        echo "Downloading wyzecam library..."
        TEMP_DIR=$(mktemp -d)
        curl -sL "https://github.com/mrlt8/docker-wyze-bridge/archive/refs/tags/v${WYZECAM_VERSION}.tar.gz" | tar -xz -C "$TEMP_DIR"

        # Copy wyzecam module
        cp -r "$TEMP_DIR/docker-wyze-bridge-${WYZECAM_VERSION}/app/wyzecam" "$PLUGIN_DIR/"

        # Cleanup
        rm -rf "$TEMP_DIR"
        echo "wyzecam library downloaded"
    fi
fi

echo "Setup complete!"
