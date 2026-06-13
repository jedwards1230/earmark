# =============================================================================
# Makefile for earmark
# =============================================================================
#
# Common development tasks. earmark is a Linux Go service (no native deps);
# it builds and runs anywhere Go does.
#
#   make           - Build the binary (default)
#   make dashboard - Run the status dashboard with synthetic data (no DB)
#   make check     - fmt + vet + lint + test
#   make help      - List all targets
#
# Variables can be overridden:  make build VERSION=v1.2.3
# =============================================================================

.DEFAULT_GOAL := build

MODULE_PATH := github.com/jedwards1230/earmark
VERSION     ?= dev
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME  := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GO_VERSION  := $(shell go version | awk '{print $$3}')

LDFLAGS := -X '$(MODULE_PATH)/internal/version.Version=$(VERSION)' \
           -X '$(MODULE_PATH)/internal/version.Commit=$(COMMIT)' \
           -X '$(MODULE_PATH)/internal/version.BuildTime=$(BUILD_TIME)' \
           -X '$(MODULE_PATH)/internal/version.GoVersion=$(GO_VERSION)'

# =============================================================================
# Build
# =============================================================================

# Build the main binary (default target).
.PHONY: build
build:
	@echo "🔨 Building earmark ($(VERSION), $(COMMIT))..."
	go build -ldflags "$(LDFLAGS)" -o earmark .
	@echo "✅ Build complete: ./earmark"

# Build a release binary; VERSION must be set explicitly.
.PHONY: release
release:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "❌ VERSION must be set: make release VERSION=v1.0.0"; exit 1; \
	fi
	@echo "🚀 Building release $(VERSION)..."
	go build -ldflags "$(LDFLAGS)" -o earmark .
	@echo "✅ Release build complete: ./earmark"

# Build the container image (matches the Dockerfile target arch).
.PHONY: docker
docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t earmark:$(VERSION) .

# =============================================================================
# Run
# =============================================================================

# Run the status dashboard with synthetic data — no Postgres, no DATABASE_URL.
# This is the accessible dev server for UI work and AI-agent visual verification
# (see CLAUDE.md "Visual Verification"). Serves http://localhost:8081/.
.PHONY: dashboard
dashboard:
	@echo "📊 Serving demo dashboard on http://localhost:8081/ (Ctrl-C to stop)..."
	go run . mcp --demo

# =============================================================================
# Quality
# =============================================================================

.PHONY: test
test:
	@echo "🧪 Running tests..."
	go test ./...

.PHONY: test-coverage
test-coverage:
	@echo "🧪 Running tests with coverage..."
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "✅ Coverage report: coverage.html"

.PHONY: lint
lint:
	@echo "🔍 Running linter..."
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "❌ golangci-lint not found: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	golangci-lint run ./...

.PHONY: fmt
fmt:
	@echo "🎨 Formatting code..."
	gofmt -w .

.PHONY: vet
vet:
	@echo "🔍 Vetting code..."
	go vet ./...

.PHONY: check
check: fmt vet lint test
	@echo "✅ All quality checks passed!"

# =============================================================================
# Misc
# =============================================================================

.PHONY: install
install: build
	@echo "📦 Installing to /usr/local/bin (requires sudo)..."
	sudo cp earmark /usr/local/bin/
	@echo "✅ Installed."

.PHONY: clean
clean:
	@echo "🧹 Cleaning build artifacts..."
	rm -f earmark coverage.out coverage.html
	rm -rf dist/
	@echo "✅ Clean complete."

.PHONY: version
version:
	@echo "Version:    $(VERSION)"
	@echo "Commit:     $(COMMIT)"
	@echo "Build time: $(BUILD_TIME)"
	@echo "Go version: $(GO_VERSION)"
	@echo "Module:     $(MODULE_PATH)"

.PHONY: help
help:
	@echo "🛠️  earmark Makefile"
	@echo "============================================================"
	@echo "BUILD:"
	@echo "  make build        - Build the binary (default)"
	@echo "  make release      - Release build (make release VERSION=v1.0.0)"
	@echo "  make docker       - Build the container image"
	@echo ""
	@echo "RUN:"
	@echo "  make dashboard    - Demo status dashboard, no DB (http://localhost:8081/)"
	@echo ""
	@echo "QUALITY:"
	@echo "  make test         - Run tests"
	@echo "  make test-coverage- Tests with HTML coverage report"
	@echo "  make fmt          - gofmt -w ."
	@echo "  make vet          - go vet"
	@echo "  make lint         - golangci-lint"
	@echo "  make check        - fmt + vet + lint + test"
	@echo ""
	@echo "MISC:"
	@echo "  make install      - Install to /usr/local/bin (sudo)"
	@echo "  make clean        - Remove build artifacts"
	@echo "  make version      - Show build info"
