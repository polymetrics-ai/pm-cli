#!/bin/sh
# Setup-only validator. It never builds PM, installs tools, or contacts providers.
set -eu
root=${1:-.}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$script_dir/check-setup.py" --root "$root" --policy "$script_dir/setup-policy.json"
