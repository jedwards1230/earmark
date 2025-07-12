#!/bin/bash

# Download yap binary for embedding
# Usage: ./scripts/download-yap.sh [version]
# Environment: YAP_VERSION can override the version

set -e

# Configuration
YAP_VERSION="${YAP_VERSION:-${1:-latest}}"
YAP_REPO="finnvoor/yap"
EMBED_DIR="internal/yap/embedded"
BINARY_PATH="${EMBED_DIR}/yap"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}🔄 Installing yap binary for embedding...${NC}"
echo -e "   Repository: ${YAP_REPO}"
echo -e "   Version: ${YAP_VERSION}"
echo -e "   Target: ${BINARY_PATH}"
echo

# Create embed directory
mkdir -p "${EMBED_DIR}"

# Check if Homebrew is available
if ! command -v brew >/dev/null 2>&1; then
    echo -e "${RED}❌ Error: Homebrew is required to install yap${NC}"
    echo -e "${YELLOW}💡 Install Homebrew first:${NC}"
    echo -e "   /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""
    exit 1
fi

# Install yap via Homebrew
echo -e "${BLUE}📦 Installing yap via Homebrew...${NC}"
if [ "${YAP_VERSION}" = "latest" ]; then
    echo -e "   Installing latest version..."
    if brew install finnvoor/tools/yap >/dev/null 2>&1; then
        echo -e "${GREEN}✅ yap installed successfully via Homebrew${NC}"
    else
        echo -e "${RED}❌ Error: Failed to install yap via Homebrew${NC}"
        echo -e "${YELLOW}💡 Try installing manually:${NC}"
        echo -e "   brew install finnvoor/tools/yap"
        exit 1
    fi
else
    echo -e "${YELLOW}⚠️  Warning: Specific version requested but Homebrew installs latest${NC}"
    echo -e "   Installing latest version via Homebrew..."
    if brew install finnvoor/tools/yap >/dev/null 2>&1; then
        echo -e "${GREEN}✅ yap installed successfully via Homebrew${NC}"
    else
        echo -e "${RED}❌ Error: Failed to install yap via Homebrew${NC}"
        exit 1
    fi
fi

# Find the yap binary in Homebrew installation
YAP_HOMEBREW_PATH=$(brew --prefix)/bin/yap
if [ ! -f "${YAP_HOMEBREW_PATH}" ]; then
    echo -e "${RED}❌ Error: yap binary not found at expected Homebrew location${NC}"
    echo -e "${YELLOW}💡 Expected location: ${YAP_HOMEBREW_PATH}${NC}"
    exit 1
fi

# Copy the binary to embed directory
echo -e "${BLUE}📋 Copying yap binary for embedding...${NC}"
if cp "${YAP_HOMEBREW_PATH}" "${BINARY_PATH}"; then
    # Make sure it's executable
    chmod +x "${BINARY_PATH}"
    
    # Get version info (yap doesn't have --version, so use a placeholder)
    ACTUAL_VERSION="homebrew-latest"
    
    # Verify copy
    FILE_SIZE=$(du -h "${BINARY_PATH}" | cut -f1)
    FILE_TYPE=$(file "${BINARY_PATH}" 2>/dev/null || echo "binary file")
    
    echo -e "${GREEN}✅ Binary copy successful!${NC}"
    echo -e "   Source: ${YAP_HOMEBREW_PATH}"
    echo -e "   Target: ${BINARY_PATH}"
    echo -e "   File size: ${FILE_SIZE}"
    echo -e "   File type: ${FILE_TYPE}"
    echo -e "   Version: ${ACTUAL_VERSION}"
    
    # Create version info file for build system
    echo "${ACTUAL_VERSION}" > "${EMBED_DIR}/version.txt"
    echo -e "   Version file: ${EMBED_DIR}/version.txt"
    
else
    echo -e "${RED}❌ Error: Failed to copy yap binary${NC}"
    exit 1
fi

echo
echo -e "${GREEN}🎉 Yap binary ready for embedding!${NC}"
echo -e "${YELLOW}💡 Build your application normally - the binary will be embedded automatically${NC}"