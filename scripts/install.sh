#!/bin/sh
# sage-router installer — curl -fsSL https://sage-router.dev/install.sh | sh
set -e

REPO="sage-router/sage-router"
BINARY="sage-router"

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

case "$OS" in
    linux)  PLATFORM="linux-${ARCH}" ;;
    darwin) PLATFORM="darwin-${ARCH}" ;;
    *)      echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Get latest release
LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
if [ -z "$LATEST" ]; then
    echo "Failed to fetch latest release"
    exit 1
fi

URL="https://github.com/${REPO}/releases/download/${LATEST}/${BINARY}-${PLATFORM}"

echo "Installing ${BINARY} ${LATEST} for ${PLATFORM}..."

# Download to temp
TMP=$(mktemp)
curl -fsSL "$URL" -o "$TMP"
chmod +x "$TMP"

# Install
INSTALL_DIR="${HOME}/.local/bin"
mkdir -p "$INSTALL_DIR"
mv "$TMP" "${INSTALL_DIR}/${BINARY}"

echo ""
echo "  Installed to ${INSTALL_DIR}/${BINARY}"
echo ""
echo "  Make sure ${INSTALL_DIR} is in your PATH:"
echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
echo ""
echo "  Then run:"
echo "    sage-router"
echo ""
