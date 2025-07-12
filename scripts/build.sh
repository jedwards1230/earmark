#!/bin/bash

# Build script for lil-whisper with version embedding

set -e

# Default values
VERSION="${VERSION:-dev}"
OUTPUT="${OUTPUT:-lil-whisper}"
GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"

# Get build information
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GO_VERSION=$(go version | awk '{print $3}')
# Run module path detection with host GOOS/GOARCH (not cross-compiled)
MODULE_PATH=$(env GOOS= GOARCH= go run scripts/get-module-path.go)

# Build ldflags
LDFLAGS="-X '${MODULE_PATH}/internal/version.Version=${VERSION}' \
         -X '${MODULE_PATH}/internal/version.Commit=${COMMIT}' \
         -X '${MODULE_PATH}/internal/version.BuildTime=${BUILD_TIME}' \
         -X '${MODULE_PATH}/internal/version.GoVersion=${GO_VERSION}'"

# Validate target OS (macOS only due to Yap dependency)
if [[ "${GOOS}" != "darwin" ]]; then
    echo "ERROR: This application only supports macOS due to Yap (Apple speech recognition) dependency"
    echo "GOOS must be 'darwin', got: ${GOOS}"
    exit 1
fi

# Print build information
echo "Building lil-whisper for macOS..."
echo "Version: ${VERSION}"
echo "Commit: ${COMMIT}"
echo "Build Time: ${BUILD_TIME}"
echo "Go Version: ${GO_VERSION}"
echo "Target: ${GOOS}/${GOARCH}"
echo "Output: ${OUTPUT}"
echo

# Build the binary
GOOS="${GOOS}" GOARCH="${GOARCH}" go build -ldflags "${LDFLAGS}" -o "${OUTPUT}" .

echo "Build completed: ${OUTPUT}"

# Show file info
if [[ -f "${OUTPUT}" ]]; then
    echo "File size: $(du -h "${OUTPUT}" | cut -f1)"
    echo "File info: $(file "${OUTPUT}" 2>/dev/null || echo "binary file")"
fi