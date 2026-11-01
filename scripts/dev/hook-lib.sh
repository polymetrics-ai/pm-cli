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
