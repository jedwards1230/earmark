# Contributing to earmark

earmark is a Go service that indexes audiobook transcriptions produced by an external ASR runner (Python) and exposes them for semantic search via an MCP server. It runs as a Linux container in Kubernetes.

## Prerequisites

- **Go 1.26+** — version is pinned in `go.mod`; use `go version` to verify.
- **Python 3.12+** — required for the `runner/` ASR component and its test suite.
- **golangci-lint** — install via the [official instructions](https://golangci-lint.run/welcome/install/); the version is resolved from `.golangci.yml` or the CI action.
- **gosec** and **govulncheck** — for security scanning (see CI).

## Build, test & lint

```bash
# Build
go build ./...

# Test (Go — with race detector and coverage)
go test -v -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1

# Test (Python runner — installs lightweight test deps only, no GPU stack needed)
pip install psycopg2-binary pytest
python -m pytest runner/test_runner.py -v

# Lint
golangci-lint run ./...

# Security scan
gosec ./...
govulncheck ./...
```

## Documentation

Keep documentation current as part of the change, not as a follow-up — update the README and any affected docs in the same PR. `docs/CONTRACT.md` is authoritative: changes to environment variable names, database column names, or the MCP upstream key must update it first.

## Before you open a PR

- Make sure all CI checks pass locally first — run the formatter, linter, and tests.
- For UI or template changes, verify the status dashboard visually in demo mode before opening a PR — it needs no database: `MCP_HTTP_ADDR=:9876 ./earmark mcp --demo` then open `http://localhost:9876/`.

## Branching & commits

- Branch off `main`; never commit directly to `main`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, …).
- Sign your commits where possible (`git commit -S`).
- Keep each PR focused; delete dead code rather than commenting it out.

## Pull requests

- Open the PR against `main`.
- Every PR runs CI and an automated code review. Resolve **all** review threads before the PR is merged.
- A PR is merged once CI is green and the review is approved.

## Releases

Releases are triggered automatically on every push to `main`. The release workflow reads the `semver:major`, `semver:minor`, or `semver:patch` label from the merged PR to determine the bump type; if no label is present, it defaults to a **patch** bump. Releases produce an immutable `vX.Y.Z` git tag, a Docker image pushed to GHCR, and a Helm chart pushed to the OCI registry. Release notes are AI-generated from the diff when `ANTHROPIC_API_KEY` is configured in the repo secrets, and fall back to GitHub's native generated notes otherwise.
