#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:-}
if [ "$#" -ne 1 ]; then
  printf '%s\n' 'usage: scripts/browser.sh install|build|generate|check' >&2
  exit 2
fi

case "$mode" in
  install|build|generate|check) ;;
  *)
    printf '%s\n' 'usage: scripts/browser.sh install|build|generate|check' >&2
    exit 2
    ;;
esac

cd "$repo_root"
python=${PYTHON:-python3}
tools_cache=${PM_TOOLS_CACHE:-"$repo_root/.cache/pm-tools"}
export PM_TOOLS_CACHE="$tools_cache"
browser_cache="$repo_root/.cache/browser-npm"

run_npm() {
  env -i \
    PATH="$PATH" \
    HOME="$browser_cache/home" \
    PM_TOOLS_CACHE="$tools_cache" \
    NPM_CONFIG_USERCONFIG="$browser_cache/user.npmrc" \
    NPM_CONFIG_GLOBALCONFIG="$browser_cache/global.npmrc" \
    NPM_CONFIG_CACHE="$browser_cache/cache" \
    NPM_CONFIG_REGISTRY=https://registry.npmjs.org \
    NPM_CONFIG_ENGINE_STRICT=true \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    "$python" tools/bootstrap.py exec npm -- "$@"
}

install_dependencies() {
  mkdir -m 700 -p "$browser_cache/home" "$browser_cache/cache"
  install -m 600 /dev/null "$browser_cache/user.npmrc"
  install -m 600 /dev/null "$browser_cache/global.npmrc"
  run_npm --prefix sdk/browser ci --ignore-scripts --no-audit --no-fund
  run_npm --prefix tests/client-parity ci --ignore-scripts --no-audit --no-fund
}

case "$mode" in
  install)
    install_dependencies
    ;;
  build)
    run_npm --prefix sdk/browser run generate:check
    run_npm --prefix sdk/browser run build
    run_npm --prefix tests/client-parity run check
    "$python" tools/bootstrap.py exec chromium -- --version
    ;;
  generate)
    run_npm --prefix sdk/browser run generate
    ;;
  check)
    run_npm --prefix sdk/browser run generate:check
    ;;
esac
