#!/bin/sh
# Shared helpers for the git hooks. Source it; do not run it.

# pm_inside_image reports whether the hook already runs in the developer
# container. PMCLI_IN_IMAGE=1 counts only together with /run/.containerenv,
# which Podman creates in each container. On a host the variable is ignored.
pm_inside_image() {
  [ "${PMCLI_IN_IMAGE:-}" = 1 ] && [ -f /run/.containerenv ]
}

# pm_in_image runs the hook again inside the pinned CI image on Linux, and does
# not return. Bootstrap accepts only the native environment of that image
# (build/native-environment.json) on Linux. On macOS, and inside the image
# (pm_inside_image), it does nothing. Call it from the repository root with
# the hook script path and the hook arguments, before the hook reads stdin.
pm_in_image() {
  if pm_inside_image; then
    return 0
  fi
  case "$(uname -s)" in
    Linux) exec sh scripts/ci/in-ci-image.sh dev -- sh "$@" ;;
  esac
}

# pm_host_checks HOOK OS NAME... prints the check-docs.sh checks from NAME...
# that the hook runs on the host OS (the output of `uname -s`), one per line.
# On Darwin it removes `docs` and writes one notice to stderr: the vendored
# wheels of the docs check are Linux-only. The CI `checks` job runs it.
pm_host_checks() {
  hook=$1
  os=$2
  shift 2
  for name in "$@"; do
    if [ "$os" = Darwin ] && [ "$name" = docs ]; then
      # The backticks in the notice are literal text, not a command.
      # shellcheck disable=SC2016
      printf '%s: skipped check-docs.sh docs on macOS: its vendored dependencies are Linux-only. The CI job `checks` runs it.\n' "$hook" >&2
      continue
    fi
    printf '%s\n' "$name"
  done
}

# pm_python_probe passes for Python 3.11 or later that imports the modules of
# the host checks: tomllib (test_secret_scan.py), yaml (check-setup.py and
# test_runner_admission.py), and markdown_it with its dependency mdurl
# (check-setup.py).
pm_python_probe='import sys
if sys.version_info < (3, 11):
    raise SystemExit(1)
import tomllib, yaml, markdown_it, mdurl'

# pm_select_python HOOK exports PMCLI_PYTHON for check-docs.sh on macOS. It
# selects the first of python3, python3.13, python3.12 and python3.11 on PATH
# that passes pm_python_probe, and prints its version. If no interpreter
# passes, it prints the prerequisite and returns 1.
pm_select_python() {
  for candidate in python3 python3.13 python3.12 python3.11; do
    if command -v "$candidate" >/dev/null 2>&1 &&
      "$candidate" -I -B -c "$pm_python_probe" >/dev/null 2>&1; then
      PMCLI_PYTHON=$candidate
      export PMCLI_PYTHON
      printf '%s: check-docs.sh uses Python %s\n' "$1" \
        "$("$candidate" -I -B -c 'import platform; print(platform.python_version())')"
      return 0
    fi
  done
  printf '%s: no Python 3.11 or later on PATH imports tomllib, yaml, markdown_it and mdurl. The hook tried python3, python3.13, python3.12 and python3.11. See docs/development/ci.md.\n' "$1" >&2
  return 1
}

# pgo runs the pinned Go toolchain.
pgo() {
  python3 tools/bootstrap.py exec go -- "$@"
}

# drop_stack_packages reads ./relative package paths on stdin and removes the
# packages in scripts/ci/stack-packages.txt, which need the dependency stack.
drop_stack_packages() {
  stack_list=$(mktemp)
  grep -v '^#' scripts/ci/stack-packages.txt | sed '/^$/d' > "$stack_list"
  grep -v -x -F -f "$stack_list" || true
  rm -f "$stack_list"
}
