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
# Each host runs every named check, so it prints NAME... and writes no notice.
pm_host_checks() {
  shift 2
  printf '%s\n' "$@"
}

# pm_push_checks OS prints the check-docs.sh checks that the pre-push hook runs
# on the host OS (the output of `uname -s`), one per line. The names come from
# `check-docs.sh --list`. The hook runs the packages of the `unit` check with
# its own `go test`, so the list never has `unit`: a push runs one local unit
# run on every OS. The list has `vet`, and the hook runs go vet only through
# it: a push runs go vet one time.
pm_push_checks() {
  pm_push_names=$(sh scripts/ci/check-docs.sh --list) || return 1
  # Word splitting of the check names is intended.
  # shellcheck disable=SC2046,SC2086
  pm_host_checks pre-push "$1" $(printf '%s\n' $pm_push_names | grep -v -x unit)
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
# packages in scripts/ci/stack-packages.txt, which need the dependency stack,
# and the packages that CI excludes from its Go test runs. The rule is in
# scripts/ci/go-packages.sh, which the CI jobs also use. Call it from the
# repository root.
drop_stack_packages() {
  # shellcheck source=scripts/ci/go-packages.sh disable=SC1091
  . scripts/ci/go-packages.sh
  pm_drop_stack_packages
}
