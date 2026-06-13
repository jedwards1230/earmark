# GitHub Actions Setup

## Secrets

| Secret | Required | Purpose |
|--------|----------|---------|
| `GITHUB_TOKEN` | Auto-provided | Tag pushing, GHCR login, release creation |
| `ANTHROPIC_API_KEY` | Required | AI-generated release notes (Claude); falls back to GitHub's native notes if absent |

## Repository Permissions

In **Settings → Actions → General → Workflow permissions**:
- "Read and write permissions" (release workflow pushes tags and publishes to GHCR)
- "Allow GitHub Actions to create and approve pull requests"

## Workflows

### CI (`ci.yml`)

Triggers on push to `main`/`develop` and PRs targeting `main`.

| Job | What it does |
|-----|-------------|
| `test` | `go test -race -coverprofile` on `ubuntu-latest` |
| `lint` | `golangci-lint` via the official action |
| `security` | `gosec` + `govulncheck` static analysis |
| `build` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`, uploads `earmark-linux-amd64` artifact (7-day retention) |

The build target is **linux/amd64** only — earmark is a Linux container service.

### Release (`release.yml`)

Triggers automatically on push to `main` (reads `semver:patch/minor/major` label from the merged PR; defaults to patch if no label) or manually via `workflow_dispatch`.

Pipeline:

1. **detect** — reads bump type from PR label or dispatch input; supports `dry_run` mode (previews version + notes without tagging)
2. **prepare** — calls `jedwards1230/release-workflows/.github/workflows/ai-release.yml@v1`; bumps `deploy/helm/earmark/Chart.yaml`, pushes git tag, generates AI release notes (Claude Haiku for patch, Sonnet for minor/major)
3. **build** — builds and pushes `ghcr.io/jedwards1230/earmark:{version}` and `:latest` to GHCR (`linux/amd64`, `CGO_ENABLED=0`, GHA layer cache)
4. **helm** — packages and pushes the OCI Helm chart to `oci://ghcr.io/jedwards1230/charts/earmark`
5. **release** — creates GitHub Release with Docker + Helm install snippets prepended to the AI-generated body

### Claude PR Review (`claude-pr-review.yml`)

Calls `jedwards1230/release-workflows/.github/workflows/claude-pr-review.yml@v1` on every PR open/sync/reopen. Reviews Go patterns specific to earmark: pgx pool usage, MCP stdio stderr discipline, embeddings client error handling, `DEBUG_DB_RESET` guard integrity.

## Troubleshooting

**Release fails with permission error** — verify "Read and write permissions" is set in workflow settings.

**Build fails** — run `CGO_ENABLED=0 go build ./...` and `go test ./...` locally to reproduce.

**Release created but no AI notes** — check that `ANTHROPIC_API_KEY` is set as a repository secret; the workflow falls back to GitHub's native notes if absent.

**Tag already exists** — delete the tag locally and on GitHub, then re-run the workflow.
