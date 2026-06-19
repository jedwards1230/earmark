#!/bin/bash
# SessionStart hook: provision the browser the Playwright MCP server needs.
#
# The Playwright MCP server is declared in .mcp.json / .claude/mcp.json with `--browser firefox`,
# but a fresh container ships without the browser binary. Without this hook the
# first browser_navigate call fails with "Browser firefox is not installed".
#
# Scoped to Claude Code on the web (remote) sessions; local machines are assumed
# to manage their own Playwright install. Safe to re-run: both commands are
# idempotent and the downloaded browser is cached in the container image.
set -euo pipefail

# Only provision in the remote (web) environment.
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

# Keep the browser in the well-known cache the container persists.
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-/opt/pw-browsers}"

echo "[session-start] installing Playwright firefox OS deps + browser..."

# OS-level shared libraries Firefox needs (apt; requires root).
npx -y playwright@latest install-deps firefox

# The browser build matching the version @playwright/mcp pins. Using the MCP's
# own installer keeps the build in lockstep with the server we actually run.
npx -y @playwright/mcp@latest install-browser firefox

echo "[session-start] Playwright firefox ready."
