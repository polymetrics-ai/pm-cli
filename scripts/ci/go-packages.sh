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
#   - the stack run (jobs stack-1, stack-2 and stack-3): every stack list
#     entry except tests/acceptance and the excluded packages. This is the
#     derived set D. scripts/ci/stack-shards.txt puts each path of D in one
#     shard;
#   - the Gherkin run (job stack-3): tests/acceptance;
#   - the exclusion list below.
# The selection is a rule, so a new package goes to the unit run without an
# edit here. See docs/development/ci.md.

pm_stack_list=scripts/ci/stack-packages.txt
pm_shard_list=scripts/ci/stack-shards.txt
# Deliberate fault for the #286a canary K4, only on the throwaway branch
# agent/286a-canary-k4-3, which never merges. Only in CI (GITHUB_ACTIONS is
# "true"), the shard check reads a faulty copy of the shard file. The git
# hooks keep the real file, so they can commit and push this branch.
if [ "${GITHUB_ACTIONS:-}" = true ]; then
  pm_shard_list=scripts/ci/canary-k4-stack-shards.txt
fi
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

# pm_check_shards checks scripts/ci/stack-shards.txt against the derived set
# D (pm_stack_entries | pm_stack_run_packages). The file is valid only when:
#   - each line is "<shard-id> <path>";
#   - each shard ID is 1, 2 or 3, and each of the three shards has a path;
#   - each path of D is in the file exactly once;
#   - no line names a path outside D: not the Gherkin package, not an
#     excluded package and not an unknown path.
# It prints each problem on stderr and returns 1 when it finds one. The checks
# job (check-docs.sh setup) and the start of each shard (check-bdd.sh) run it.
pm_check_shards() {
  if [ ! -f "$pm_shard_list" ]; then
    printf 'go-packages: %s is missing\n' "$pm_shard_list" >&2
    return 1
  fi
  pm_shard_set=$(mktemp) || return 1
  pm_shard_status=0
  { pm_stack_entries | pm_stack_run_packages; } > "$pm_shard_set" || pm_shard_status=1
  if [ ! -s "$pm_shard_set" ]; then
    printf 'go-packages: the stack run has no package, so %s cannot be checked\n' "$pm_shard_list" >&2
    pm_shard_status=1
  fi
  if [ "$pm_shard_status" -eq 0 ]; then
    pm_shard_excluded=$(pm_excluded_packages | cut -d ' ' -f 1 | tr '\n' ' ')
    awk -v list="$pm_shard_list" -v gherkin="$pm_gherkin_package" -v excluded=" $pm_shard_excluded" '
      FNR == NR { order[++count] = $0; derived[$0] = 1; next }
      /^#/ || /^$/ { next }
      {
        where = "go-packages: " list ":" FNR ": "
        if (NF != 2) { print where "a line must be \"<shard-id> <path>\": " $0; bad = 1; next }
        if ($1 != "1" && $1 != "2" && $1 != "3") { print where "the shard ID " $1 " is not 1, 2 or 3"; bad = 1 }
        else size[$1]++
        if ($2 == gherkin) { print where $2 " is the Gherkin package, which no shard runs"; bad = 1 }
        else if (index(excluded, " " $2 " ")) { print where $2 " is an excluded package, which no shard runs"; bad = 1 }
        else if (!($2 in derived)) { print where $2 " is not a stack run package"; bad = 1 }
        if ($2 in seen) { print where $2 " is also on line " seen[$2]; bad = 1 }
        else seen[$2] = FNR
      }
      END {
        for (i = 1; i <= count; i++)
          if (!(order[i] in seen)) { print "go-packages: " list ": the stack run package " order[i] " is in no shard"; bad = 1 }
        for (id = 1; id <= 3; id++)
          if (!(id in size)) { print "go-packages: " list ": shard " id " has no package"; bad = 1 }
        exit bad
      }' "$pm_shard_set" "$pm_shard_list" >&2 || pm_shard_status=1
  fi
  rm -f "$pm_shard_set"
  return "$pm_shard_status"
}

# pm_shard_packages ID prints the paths of shard ID in file order. Call it
# only after pm_check_shards passed.
pm_shard_packages() {
  sed -e '/^#/d' -e '/^$/d' "$pm_shard_list" | awk -v id="$1" '$1 == id { print $2 }'
}
