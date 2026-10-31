#!/bin/sh
# Run one CI command inside the pinned job image (build/ci/Dockerfile).
#
#   in-ci-image.sh run NAME -- COMMAND...   run COMMAND in a new job container
#   in-ci-image.sh stop NAME                remove a leftover job container
#   in-ci-image.sh dev -- COMMAND...        run COMMAND for a developer (the git hooks)
#
# The host engine is rootless Podman (PMCLI_CONTAINER_ENGINE, default podman).
# The workspace, RUNNER_TEMP and the tool cache keep their host paths inside the
# container, because the stack fixture's bind-mount sources are resolved by the
# host engine. The engine socket is mounted at /var/run/docker.sock. Do not use
# the workflow `container:` key: it moves the workspace to /__w.
#
# The dev mode needs no GitHub variables. It mounts the checkout, the Git common
# directory and the tool cache at their host paths. It mounts no engine socket
# and uses the default network, so COMMAND cannot start the dependency stack.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
engine=${PMCLI_CONTAINER_ENGINE:-podman}

usage() {
  printf '%s\n' 'usage: in-ci-image.sh run NAME -- COMMAND... | stop NAME | dev -- COMMAND...' >&2
  exit 2
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# ci_image sets image to the job image tag and builds the image when that tag
# is missing.
ci_image() {
  tag=$(cd "$repo_root/build/ci" && sha256 Dockerfile requirements.txt | sha256 | cut -c1-32)
  image="localhost/pmcli-ci:$tag"
  if ! "$engine" image inspect "$image" >/dev/null 2>&1; then
    printf 'Building CI image %s\n' "$image" >&2
    "$engine" build --platform linux/amd64 --tag "$image" \
      --file "$repo_root/build/ci/Dockerfile" "$repo_root/build/ci" >&2
  fi
  # Status goes to stderr so stdout carries only the child command's output
  # (capture-native-profile.py redirects stdout to a JSON file).
  printf 'CI image: %s\n' "$image" >&2
}

if [ "${1:-}" = dev ]; then
  [ "${2:-}" = -- ] || usage
  shift 2
  [ "$#" -ge 1 ] || usage
  container="pmcli-dev-$(id -u)-$$"
  cache=${PMCLI_CACHE_DIR:-"$HOME/.cache/pmcli"}
  mkdir -p "$cache" "$cache/home"
  chmod 700 "$cache"
  common=$(git -C "$repo_root" rev-parse --path-format=absolute --git-common-dir)
  ci_image
  set -- "$image" "$@"
  # A linked worktree keeps its Git data in the common directory of the main
  # checkout, outside this checkout.
  case "$common" in
    "$repo_root" | "$repo_root"/*) ;;
    *) set -- --volume "$common:$common" "$@" ;;
  esac
  # Same user, user namespace and limits as the run mode. Stdin stays open for
  # the pre-push hook. GIT_INDEX_FILE names the index of the commit under check
  # (for example, the temporary index of `git commit -a`).
  exec "$engine" run --rm --init --pull never --interactive --name "$container" \
    --userns keep-id --user "$(id -u):$(id -g)" \
    --memory "${PMCLI_JOB_MEMORY:-8g}" --cpus "${PMCLI_JOB_CPUS:-4}" \
    --pids-limit "${PMCLI_JOB_PIDS:-4096}" \
    --volume "$repo_root:$repo_root" \
    --volume "$cache:$cache" \
    --workdir "$repo_root" \
    --env PMCLI_CACHE_DIR="$cache" --env HOME="$cache/home" \
    --env PM_TOOLS_CACHE="$cache/pm-tools" \
    --env PMCLI_IN_IMAGE=1 --env GIT_INDEX_FILE \
    "$@"
fi

[ "$#" -ge 2 ] || usage
mode=$1
name=$2
shift 2
case "$name" in
  '' | *[!a-z0-9-]*) usage ;;
esac
: "${GITHUB_RUN_ID:?GitHub run identity required}"
: "${GITHUB_RUN_ATTEMPT:?GitHub run attempt required}"
container="pmcli-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$name"

case "$mode" in
  stop)
    [ "$#" -eq 0 ] || usage
    # A timed-out or cancelled step can leave its container running.
    "$engine" stop -t 10 "$container" >/dev/null 2>&1 || true
    "$engine" rm -f "$container" >/dev/null 2>&1 || true
    exit 0
    ;;
  run)
    [ "${1:-}" = -- ] || usage
    shift
    [ "$#" -ge 1 ] || usage
    ;;
  *) usage ;;
esac

: "${GITHUB_WORKSPACE:?GitHub workspace required}"
: "${RUNNER_TEMP:?GitHub runner job temporary directory required}"
case "$repo_root" in
  "$GITHUB_WORKSPACE" | "$GITHUB_WORKSPACE"/*) ;;
  *)
    printf '%s\n' 'checkout must be inside GITHUB_WORKSPACE' >&2
    exit 1
    ;;
esac
cache=${PMCLI_CACHE_DIR:-"$HOME/.cache/pmcli"}
socket=${PMCLI_ENGINE_SOCKET:-"${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock"}
if [ ! -S "$socket" ]; then
  printf 'container engine socket is unavailable: %s\n' "$socket" >&2
  exit 1
fi
mkdir -p "$cache" "$cache/home"
chmod 700 "$cache"

ci_image

# PM_TOOLS_CACHE matches check-foundation.sh, so a separate cleanup container
# (with-stack-fixture.py --cleanup after an abrupt stop) finds the pinned Compose.
# Run as the runner user (same UID inside and outside), not as the user
# namespace's root: root ignores file modes, which permission tests rely on,
# and files written to the workspace keep the runner user's ownership.
exec "$engine" run --rm --init --pull never --name "$container" \
  --userns keep-id --user "$(id -u):$(id -g)" \
  --network host \
  --memory "${PMCLI_JOB_MEMORY:-8g}" --cpus "${PMCLI_JOB_CPUS:-4}" \
  --pids-limit "${PMCLI_JOB_PIDS:-4096}" \
  --volume "$GITHUB_WORKSPACE:$GITHUB_WORKSPACE" \
  --volume "$RUNNER_TEMP:$RUNNER_TEMP" \
  --volume "$cache:$cache" \
  --volume "$socket:/var/run/docker.sock" \
  --workdir "$repo_root" \
  --env PMCLI_CACHE_DIR="$cache" --env HOME="$cache/home" \
  --env PM_TOOLS_CACHE="$cache/pm-tools" \
  --env CI --env GITHUB_ACTIONS --env GITHUB_WORKSPACE --env GITHUB_REPOSITORY \
  --env GITHUB_SHA --env GITHUB_REF --env GITHUB_EVENT_NAME --env GITHUB_EVENT_PATH \
  --env GITHUB_RUN_ID --env GITHUB_RUN_ATTEMPT --env RUNNER_TEMP --env RUNNER_NAME \
  "$image" "$@"
