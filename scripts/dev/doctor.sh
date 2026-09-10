#!/bin/sh
# Read-only availability report. Missing optional tools are reported, not installed.
set -eu
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
printf '%s\n' 'PM CLI developer setup'
if [ -f "$repo_root/go.mod" ]; then
  printf '%s\n' 'Go module: present; this doctor does not build or test it.'
else
  printf '%s\n' 'Go module: absent; application build and tests are not available yet.'
fi
for tool in go golangci-lint govulncheck; do
  if command -v "$tool" >/dev/null 2>&1; then
    case "$tool" in
      go) go version ;;
      golangci-lint) golangci-lint version ;;
      govulncheck) govulncheck -version ;;
    esac
  else
    printf '%s\n' "$tool: unavailable (no installation attempted)."
  fi
done
if command -v codegraph >/dev/null 2>&1; then
  CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph --version
  if [ -d "$repo_root/.codegraph" ]; then
    CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json "$repo_root"
  else
    printf '%s\n' 'CodeGraph index: absent; initialization is an explicit separate action.'
  fi
else
  printf '%s\n' 'CodeGraph: unavailable; use bounded rg/read navigation.'
fi
printf '%s\n' 'This availability report is not application validation or connector proof.'
