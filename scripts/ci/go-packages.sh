#!/bin/sh
# Package selection for the Go test runs of CI and the git hooks. Source this
# file from the repository root. Do not run it.
#
# scripts/ci/stack-packages.txt is the one stack list. The checks job
# (check-docs.sh), the bdd job (check-bdd.sh) and the git hooks
# (scripts/dev/hook-lib.sh) read it only through these functions.
#
# Each root-module package that `go list ./...` prints goes to one place:
#   - the unit run (checks job): every package that is not in the stack list
#     and not excluded;
#   - the stack run (bdd job): every stack list entry except tests/acceptance
#     and the excluded packages;
#   - the Gherkin run (bdd job): tests/acceptance;
#   - the exclusion list below.
# The selection is a rule, so a new package goes to the unit run without an
# edit here. See docs/development/ci.md.

pm_stack_list=scripts/ci/stack-packages.txt
pm_gherkin_package=./tests/acceptance

# pm_stack_entries prints the entries of the stack list, one ./relative package
# path on each line, without comment lines and empty lines.
pm_stack_entries() {
  if [ ! -f "$pm_stack_list" ]; then
    printf 'go-packages: %s is missing\n' "$pm_stack_list" >&2
    return 1
  fi
  sed -e '/^#/d' -e '/^$/d' "$pm_stack_list"
}

# pm_excluded_packages prints the root-module packages that neither Go test
# run runs, one "PATH REASON" line each. The bdd job log prints each line.
pm_excluded_packages() {
  printf '%s\n' './tests/client-parity it also runs the Playwright browser tests, which need a browser as well as the stack. Issue #274 runs it in CI.'
}

# pm_drop_paths FILE reads ./relative package paths on stdin and prints the
# paths that are not a line of FILE.
pm_drop_paths() {
  pm_drop_status=0
  grep -v -x -F -f "$1" || pm_drop_status=$?
  # grep exits with 1 when it prints no line. That is a valid result.
  [ "$pm_drop_status" -le 1 ]
}

# pm_drop_stack_packages reads ./relative package paths on stdin. It prints
# the paths that are not stack list entries and not excluded packages.
pm_drop_stack_packages() {
  pm_drop_list=$(mktemp) || return 1
  if { pm_stack_entries && pm_excluded_packages | cut -d ' ' -f 1; } > "$pm_drop_list" &&
    pm_drop_paths "$pm_drop_list"; then
    rm -f "$pm_drop_list"
    return 0
  fi
  rm -f "$pm_drop_list"
  return 1
}

# pm_stack_run_packages reads the stack list entries on stdin (the output of
# pm_stack_entries). It prints the entries that the stack run runs: every
# entry except the Gherkin package and the excluded packages.
pm_stack_run_packages() {
  pm_drop_list=$(mktemp) || return 1
  if { printf '%s\n' "$pm_gherkin_package" && pm_excluded_packages | cut -d ' ' -f 1; } > "$pm_drop_list" &&
    pm_drop_paths "$pm_drop_list"; then
    rm -f "$pm_drop_list"
    return 0
  fi
  rm -f "$pm_drop_list"
  return 1
}
