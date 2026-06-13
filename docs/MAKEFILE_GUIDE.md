# Makefile Guide

earmark is a Linux Go service deployed as a container in Kubernetes. The Makefile covers local development and container builds; production deployment is handled via ArgoCD from the homelab-k8s repo.

## Quick reference

```bash
make              # build the earmark binary (default)
make test         # run all tests
make check        # fmt + vet + lint + test
make docker       # build the container image
make dashboard    # demo status dashboard, no DB (http://localhost:8081/)
make help         # full target list
```

## Targets

### Build

| Target | What it does |
|--------|--------------|
| `make build` | Compiles `./earmark` with version/commit ldflags |
| `make release VERSION=v1.2.3` | Same, but requires an explicit version |
| `make docker` | Builds the container image tagged `earmark:<VERSION>` |

### Run

| Target | What it does |
|--------|--------------|
| `make dashboard` | `go run . mcp --demo` — serves the htmx status dashboard with synthetic data at `http://localhost:8081/`; no Postgres required |

Use `DEMO_SCENARIO=empty|active|stale|failed` to switch dashboard fixture states.

### Quality

| Target | What it does |
|--------|--------------|
| `make test` | `go test ./...` |
| `make test-coverage` | Tests with race detector + HTML coverage report |
| `make fmt` | `gofmt -w .` |
| `make vet` | `go vet ./...` |
| `make lint` | `golangci-lint run ./...` (must be installed) |
| `make check` | All four in sequence |

### Misc

| Target | What it does |
|--------|--------------|
| `make install` | Copies binary to `/usr/local/bin` (sudo) |
| `make clean` | Removes `./earmark`, `coverage.*`, `dist/` |
| `make version` | Prints version/commit/Go version/module |

## Variables

```bash
make build VERSION=v1.2.3   # override version string
make docker VERSION=v1.2.3  # image tagged earmark:v1.2.3
```

## Typical dev workflow

```bash
# Edit code, then:
make check          # fmt + vet + lint + test — must be clean before PR

# Verify UI changes without a database:
make dashboard
# open http://localhost:8081/ (or use Playwright MCP)

# Build the container locally:
make docker VERSION=dev
```

## Deployment

earmark is not distributed as a standalone binary. Production deployment uses:

1. Push to `main` triggers a container image build (GitHub Actions → `ghcr.io/jedwards1230/earmark`)
2. Update the image tag in the homelab-k8s Helm chart
3. ArgoCD syncs the two Deployments (`ingest` and `mcp`) into the cluster

For local iteration, `make dashboard` (no DB) or point `DATABASE_URL` at a dev Postgres instance and run `go run . mcp`.

## Troubleshooting

**`golangci-lint: command not found`**
```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
# or skip linting: make fmt vet test
```

**Go module issues**
```bash
go mod tidy && make build
```
