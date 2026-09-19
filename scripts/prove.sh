#!/bin/sh
# Runs one connector lane's authored proof cases through the actual public PM
# path and writes immutable receipts for that exact deployment. It refuses to
# run without the explicit artifact, session and provider-observation inputs:
# a missing input is a blocked proof, never a passing one.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

connector=''
lane=''
mode='retry'
server=${PM_PROOF_SERVER:-}
workspace=${PM_PROOF_WORKSPACE:-}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --connector) [ "$#" -ge 2 ] || { printf '%s\n' '--connector requires a value' >&2; exit 2; }; connector=$2; shift 2 ;;
    --lane) [ "$#" -ge 2 ] || { printf '%s\n' '--lane requires a value' >&2; exit 2; }; lane=$2; shift 2 ;;
    --mode) [ "$#" -ge 2 ] || { printf '%s\n' '--mode requires a value' >&2; exit 2; }; mode=$2; shift 2 ;;
    --server) [ "$#" -ge 2 ] || { printf '%s\n' '--server requires a value' >&2; exit 2; }; server=$2; shift 2 ;;
    --workspace) [ "$#" -ge 2 ] || { printf '%s\n' '--workspace requires a value' >&2; exit 2; }; workspace=$2; shift 2 ;;
    *) printf 'unsupported argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

require() {
  name=$1
  value=$2
  if [ -z "$value" ]; then
    printf 'missing required proof input: %s\n' "$name" >&2
    exit 2
  fi
}

require '--connector' "$connector"
require '--lane' "$lane"
require '--server' "$server"
require '--workspace' "$workspace"
require 'PM_PROOF_PROFILE' "${PM_PROOF_PROFILE:-}"
require 'PM_PROOF_CONNECTION' "${PM_PROOF_CONNECTION:-}"
require 'PM_PROOF_RECEIPTS' "${PM_PROOF_RECEIPTS:-}"
require 'PM_PROOF_PROVIDER_JOURNAL' "${PM_PROOF_PROVIDER_JOURNAL:-}"
require 'PM_PROOF_PM_BINARY' "${PM_PROOF_PM_BINARY:-}"
require 'PM_PROOF_API_BINARY' "${PM_PROOF_API_BINARY:-}"
require 'PM_PROOF_WORKER_BINARY' "${PM_PROOF_WORKER_BINARY:-}"
require 'PM_PROOF_HOST_CLASS' "${PM_PROOF_HOST_CLASS:-}"

bundle=${PM_PROOF_BUNDLE:-"$repo_root/internal/connectors/data/$connector.bundle.json"}
lane_inventory=${PM_PROOF_LANE_INVENTORY:-"$repo_root/connectors/$connector/lanes/$lane.json"}
for path in "$bundle" "$lane_inventory" "${PM_PROOF_PM_BINARY}" "${PM_PROOF_API_BINARY}" "${PM_PROOF_WORKER_BINARY}" "${PM_PROOF_PROVIDER_JOURNAL}"; do
  if [ ! -f "$path" ]; then
    printf 'declared proof artifact is absent: %s\n' "$path" >&2
    exit 2
  fi
done

binary="$repo_root/.cache/bin/pmproof"
if [ ! -x "$binary" ] || [ -n "${PM_PROOF_REBUILD:-}" ]; then
  mkdir -p "$repo_root/.cache/bin"
  GOWORK=off CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o "$binary" ./cmd/pmproof
fi

exec "$binary" \
  --connector "$connector" \
  --lane "$lane" \
  --mode "$mode" \
  --server "$server" \
  --workspace "$workspace" \
  --profile "${PM_PROOF_PROFILE}" \
  --connection "${PM_PROOF_CONNECTION}" \
  --profiles-path "${PM_PROOF_PROFILES_PATH:-}" \
  --session-store "${PM_PROOF_SESSION_STORE:-os}" \
  --bundle "$bundle" \
  --lane-inventory "$lane_inventory" \
  --receipts "${PM_PROOF_RECEIPTS}" \
  --provider-journal "${PM_PROOF_PROVIDER_JOURNAL}" \
  --pm-binary "${PM_PROOF_PM_BINARY}" \
  --api-binary "${PM_PROOF_API_BINARY}" \
  --worker-binary "${PM_PROOF_WORKER_BINARY}" \
  --query-helper "${PM_PROOF_QUERY_HELPER:-}" \
  --host-class "${PM_PROOF_HOST_CLASS}"
