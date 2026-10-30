#!/bin/sh
# Shared helpers for the git hooks. Source it; do not run it.

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
