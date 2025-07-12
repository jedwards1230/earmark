# =============================================================================
# Makefile for lil-whisper
# =============================================================================
# 
# This Makefile provides common development tasks for the lil-whisper project.
# If you're new to Make, here are the basics:
#
#   make           - Runs the default target (same as 'make build')
#   make build     - Build the main binary  
#   make test      - Run all tests
#   make clean     - Remove build artifacts
#   make help      - Show all available targets
#
# Variables can be overridden:
#   make build VERSION=v1.0.0
#
# =============================================================================

# Default target (runs when you just type 'make')
.DEFAULT_GOAL := build

# Version information (can be overridden: make build VERSION=v1.2.3)
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GO_VERSION := $(shell go version | awk '{print $$3}')
MODULE_PATH := $(shell go run scripts/get-module-path.go)

# Yap binary embedding (can be overridden: make build YAP_VERSION=v1.0.0)
YAP_VERSION ?= latest

# Build flags (embeds version info into the binary)
LDFLAGS := -X '$(MODULE_PATH)/internal/version.Version=$(VERSION)' \
           -X '$(MODULE_PATH)/internal/version.Commit=$(COMMIT)' \
           -X '$(MODULE_PATH)/internal/version.BuildTime=$(BUILD_TIME)' \
           -X '$(MODULE_PATH)/internal/version.GoVersion=$(GO_VERSION)'

# =============================================================================
# Dependency Management
# =============================================================================

# Download and embed yap binary
.PHONY: download-yap
download-yap:
	@echo "📥 Downloading yap binary for embedding..."
	@echo "   Version: $(YAP_VERSION)"
	@YAP_VERSION=$(YAP_VERSION) ./scripts/download-yap.sh

# Check if embedded yap binary exists, download if needed
.PHONY: ensure-yap
ensure-yap:
	@if [ ! -s internal/yap/embedded/yap ]; then \
		echo "🔍 Embedded yap binary not found or empty - downloading..."; \
		$(MAKE) download-yap; \
	else \
		echo "✅ Embedded yap binary already available"; \
	fi

# =============================================================================
# Main Build Targets
# =============================================================================

# Build the main binary (default target)
.PHONY: build
build: ensure-yap
	@echo "🔨 Building lil-whisper..."
	@echo "   Version: $(VERSION)"
	@echo "   Commit: $(COMMIT)"
	@echo "   Build Time: $(BUILD_TIME)"
	@echo "   Go Version: $(GO_VERSION)"
	@echo "   Yap Version: $(YAP_VERSION)"
	@echo ""
	go build -ldflags "$(LDFLAGS)" -o lil-whisper .
	@echo "✅ Build complete: ./lil-whisper"

# Build for release with specific version (make release VERSION=v1.0.0)
.PHONY: release
release: ensure-yap
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "❌ Error: VERSION must be set for release builds"; \
		echo "   Usage: make release VERSION=v1.0.0"; \
		exit 1; \
	fi
	@echo "🚀 Building release $(VERSION)..."
	@echo "   Yap Version: $(YAP_VERSION)"
	go build -ldflags "$(LDFLAGS)" -o lil-whisper .
	@echo "✅ Release build complete: ./lil-whisper"

# =============================================================================
# Platform-Specific Builds (Apple Silicon macOS only - Yap requires Apple Silicon)
# =============================================================================

# Build for all supported platforms (alias for build-darwin-arm64)
.PHONY: build-all
build-all: build-darwin-arm64

# Build for Apple Silicon macOS (arm64 only)
.PHONY: build-darwin-arm64
build-darwin-arm64: ensure-yap
	@echo "🍎 Building for Apple Silicon macOS..."
	@mkdir -p dist
	@echo "   Building for Apple Silicon (arm64)..."
	@echo "   Yap Version: $(YAP_VERSION)"
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/lil-whisper-darwin-arm64 .
	@echo "✅ Apple Silicon macOS build complete:"
	@ls -la dist/lil-whisper-darwin-arm64

# Legacy alias for backwards compatibility
.PHONY: build-darwin
build-darwin: build-darwin-arm64

# Quick development build (same as 'build' but with clearer intent)
.PHONY: dev
dev: ensure-yap
	@echo "⚡ Building development version..."
	@echo "   Yap Version: $(YAP_VERSION)"
	go build -ldflags "$(LDFLAGS)" -o lil-whisper .
	@echo "✅ Development build complete: ./lil-whisper"

# =============================================================================
# Installation & Cleanup
# =============================================================================

# Install binary to system PATH (/usr/local/bin)
.PHONY: install
install: build
	@echo "📦 Installing lil-whisper to /usr/local/bin..."
	sudo cp lil-whisper /usr/local/bin/
	@echo "✅ Installation complete! You can now run 'lil-whisper' from anywhere."

# Remove all build artifacts and generated files
.PHONY: clean
clean:
	@echo "🧹 Cleaning build artifacts..."
	rm -f lil-whisper
	rm -rf dist/
	rm -rf cmd/*/main
	rm -f cmd/monitor/monitor cmd/serve/serve cmd/list/list cmd/search/search cmd/mcp/mcp cmd/version/version cmd/update/update
	rm -f coverage.out coverage.html
	@echo "✅ Clean complete"

# Remove all build artifacts including embedded yap binary
.PHONY: clean-all
clean-all: clean
	@echo "🧹 Cleaning embedded binaries..."
	rm -f internal/yap/embedded/yap
	echo "not-downloaded" > internal/yap/embedded/version.txt
	@echo "✅ Complete clean including embedded binaries"

# =============================================================================
# Testing & Quality Assurance
# =============================================================================

# Run all tests
.PHONY: test
test:
	@echo "🧪 Running tests..."
	go test ./...

# Run tests with coverage report
.PHONY: test-coverage
test-coverage:
	@echo "🧪 Running tests with coverage..."
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "✅ Coverage report generated: coverage.html"

# Run linter (requires golangci-lint to be installed)
.PHONY: lint
lint:
	@echo "🔍 Running linter..."
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "❌ golangci-lint not found. Install it with:"; \
		echo "   go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	golangci-lint run

# Format code using gofmt
.PHONY: fmt
fmt:
	@echo "🎨 Formatting code..."
	go fmt ./...
	@echo "✅ Code formatted"

# Run go vet for suspicious constructs
.PHONY: vet
vet:
	@echo "🔍 Vetting code..."
	go vet ./...
	@echo "✅ Code vetted"

# Run all quality checks (format, vet, lint, test)
.PHONY: check
check: fmt vet lint test
	@echo "✅ All quality checks passed!"

# =============================================================================
# Information & Help
# =============================================================================

# Show build version information
.PHONY: version
version:
	@echo "📋 Build Information:"
	@echo "   Version: $(VERSION)"
	@echo "   Commit: $(COMMIT)"
	@echo "   Build Time: $(BUILD_TIME)"
	@echo "   Go Version: $(GO_VERSION)"
	@echo "   Module Path: $(MODULE_PATH)"
	@echo "   Yap Version: $(YAP_VERSION)"
	@if [ -f internal/yap/embedded/version.txt ]; then \
		echo "   Embedded Yap: $$(cat internal/yap/embedded/version.txt)"; \
	else \
		echo "   Embedded Yap: not available"; \
	fi

# Show all available targets with descriptions
.PHONY: help
help:
	@echo "🛠️  lil-whisper Makefile Help"
	@echo "============================================================================="
	@echo ""
	@echo "🔨 BUILD TARGETS:"
	@echo "  make build       - Build the main binary (default target)"
	@echo "  make dev         - Quick development build"
	@echo "  make release     - Build release with version (make release VERSION=v1.0.0)"
	@echo "  make build-all   - Build for Apple Silicon macOS (arm64 only)"
	@echo "  make build-darwin-arm64 - Build specifically for Apple Silicon macOS"
	@echo ""
	@echo "📥 DEPENDENCY MANAGEMENT:"
	@echo "  make download-yap- Download yap binary for embedding (YAP_VERSION=latest)"
	@echo "  make ensure-yap  - Ensure yap binary is available (auto-download if needed)"
	@echo ""
	@echo "📦 INSTALLATION:"
	@echo "  make install     - Install binary to /usr/local/bin (requires sudo)"
	@echo "  make clean       - Remove build artifacts (keeps embedded binaries)"
	@echo "  make clean-all   - Remove all artifacts including embedded binaries"
	@echo ""
	@echo "🧪 TESTING & QUALITY:"
	@echo "  make test        - Run all tests"
	@echo "  make test-coverage- Run tests with HTML coverage report"
	@echo "  make fmt         - Format code with gofmt"
	@echo "  make vet         - Run go vet"
	@echo "  make lint        - Run golangci-lint (install first if needed)"
	@echo "  make check       - Run all quality checks (fmt + vet + lint + test)"
	@echo ""
	@echo "ℹ️  INFORMATION:"
	@echo "  make version     - Show build version information"
	@echo "  make help        - Show this help message"
	@echo ""
	@echo "📚 EXAMPLES:"
	@echo "  make                              # Build main binary (auto-downloads yap)"
	@echo "  make build VERSION=v1.2.3        # Build with specific version"
	@echo "  make build YAP_VERSION=v1.0.0    # Build with specific yap version"
	@echo "  make release VERSION=v1.0.0      # Create release build"
	@echo "  make download-yap YAP_VERSION=latest # Download latest yap binary"
	@echo "  make build-darwin-arm64           # Build for Apple Silicon macOS"
	@echo "  make clean-all                    # Clean everything including yap binary"
	@echo "  make check                        # Run all quality checks"
	@echo ""
	@echo "⚠️  REQUIREMENTS:"
	@echo "  - Go 1.24+"
	@echo "  - Apple Silicon macOS 26+ (M1/M2/M3+ due to Yap speech recognition)"
	@echo "  - Internet connection for first build (to download yap binary)"
	@echo "  - golangci-lint for linting (optional)"
	@echo ""
	@echo "📋 YAP EMBEDDING:"
	@echo "  - Yap binary is automatically downloaded and embedded during build"
	@echo "  - Set YAP_VERSION environment variable to pin specific version"
	@echo "  - Always uses embedded yap binary for consistency and reliability"
	@echo ""
	@echo "💡 TIP: Run 'make' without arguments to build the main binary"