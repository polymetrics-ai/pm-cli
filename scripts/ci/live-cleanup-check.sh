#!/bin/sh
# The cleanup check of the live runner (D2-A, L-e1 P6), as pmlive from /etc/pm-live/hooks:
# in-job (always-run step) or post-stop (owner). No job variable or label (F6). Each step
# runs; any failure exits 1, an uncertain cleanup (step 14). See ci.md "Live runner".
if [ "${1:-}" != --clean-env ]; then
  exec /usr/bin/env -i PATH=/usr/bin:/bin /bin/sh "$0" --clean-env "$@"
fi
shift
set -u
# The fixed paths and tools. The developer checks replace only these lines.
root=
proc=/proc
podman=/usr/bin/podman
python=/usr/bin/python3
home_of() { /usr/bin/getent passwd "$1" | /usr/bin/cut -d: -f6; }

# The sinks of the scan receipt, which the live harness writes (L-e2 P7).
sinks='service-logs container-logs container-records temporal-history postgresql object-store step-summary test-output host-processes'
status=0
pass() { printf 'live-cleanup-check: ok: %s\n' "$1"; }
fail() {
  printf 'live-cleanup-check: FAILED: %s\n' "$1"
  status=1
}

mode=${1:-}
case "$mode" in
  in-job | post-stop) ;;
  *)
    printf '%s\n' 'usage: live-cleanup-check.sh in-job|post-stop' >&2
    exit 2
    ;;
esac
uid=$(/usr/bin/id -u)
runtime="$root/run/user/$uid"
# env -i removed the caller's value. Podman needs this one (crrao I3).
XDG_RUNTIME_DIR=$runtime
export XDG_RUNTIME_DIR
runs="$runtime/pm-live-run"
home=$(home_of "$uid")

# 1. in-job: the scan receipt (M1).
check_receipt() {
  set -- "$runs"/*/scan-receipt
  if [ "$#" -ne 1 ] || [ ! -f "$1" ] || [ -L "$1" ] || [ "$(/usr/bin/wc -c < "$1")" -gt 4096 ]; then
    fail 'scan receipt: missing, too large, or more than one run root'
    return
  fi
  seen='' complete='' before=$status
  while IFS= read -r line || [ -n "$line" ]; do
    if [ -n "$complete" ]; then
      fail 'scan receipt: a line after complete=true'
      return
    fi
    case "$line" in
      complete=true) complete=yes ;;
      sink=*)
        # The exact grammar: sink=NAME count=DIGITS complete|incomplete (#403 F6).
        name=${line#sink=}
        name=${name%% *}
        rest=${line#"sink=$name count="}
        count=${rest%% *}
        state=${rest#"$count "}
        case "$count" in '' | *[!0-9]*) fail 'scan receipt: a line with an unknown format' && return ;; esac
        if [ "$line" != "sink=$name count=$count $state" ] || { [ "$state" != complete ] && [ "$state" != incomplete ]; }; then
          fail 'scan receipt: a line with an unknown format' && return
        fi
        case " $sinks " in *" $name "*) ;; *) fail 'scan receipt: an unknown sink' && return ;; esac
        case " $seen " in *" $name "*) fail "scan receipt: $name twice" && return ;; esac
        seen="$seen $name"
        case "$count" in '' | *[!0-9]*) fail "scan receipt: $name has no count" && return ;; esac
        [ "$count" -eq 0 ] || fail "scan receipt: $name has $count matches"
        [ "$state" = complete ] || fail "scan receipt: $name is incomplete"
        ;;
      *) fail 'scan receipt: a line with an unknown format' && return ;;
    esac
  done < "$1"
  for name in $sinks; do
    case " $seen " in *" $name "*) ;; *) fail "scan receipt: $name is missing" ;; esac
  done
  [ -n "$complete" ] || fail 'scan receipt: no final complete=true line'
  [ "$status" != "$before" ] || pass 'scan receipt: every sink is complete with 0 matches'
}
[ "$mode" = post-stop ] || check_receipt

# 2. Remove the token file by unlinkat in the token directory opened once with O_NOFOLLOW,
# checked by fstat and listed through that descriptor (#403 F4). The directory stays (R2-3).
tokens="$runtime/pm-live"
token_state=$("$python" -I -B -c '
import os, stat, sys
directory = os.open(sys.argv[1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
status = os.fstat(directory)
if status.st_uid != os.getuid() or stat.S_IMODE(status.st_mode) != 0o700:
    sys.exit(3)
try:
    os.unlink("w2-token", dir_fd=directory)
    state = "removed"
except FileNotFoundError:
    state = "absent"
print(state + ("-empty" if not os.listdir(directory) else "-not-empty"))
' "$tokens" 2> /dev/null) || token_state=error
case "$token_state" in
  removed-*) printf '%s\n' 'live-cleanup-check: the token file was still there; it is removed now' ;;
esac
case "$token_state" in
  *-not-empty) fail 'token directory is not empty' ;;
  *-empty) pass 'token directory is empty' ;;
  *) fail 'token directory: missing, a link, or a wrong owner or mode' ;;
esac

# 3. Reset the account's engine, then check that nothing is left.
if "$podman" system reset --force > /dev/null 2>&1; then pass 'engine reset'; else fail 'engine reset'; fi
engine_empty() {
  word=$1
  shift
  if left=$("$podman" "$@" 2> /dev/null); then
    if [ -z "$left" ]; then pass "engine: no $word"; else fail "engine: $word left"; fi
  else
    fail "engine: cannot list $word"
  fi
}
engine_empty containers ps --all --quiet
engine_empty pods pod ps --quiet
engine_empty volumes volume ls --quiet
engine_empty images images --quiet
if networks=$("$podman" network ls --format '{{.Name}}' 2> /dev/null) && [ "$networks" = podman ]; then
  pass 'engine: only the default network'
else
  fail 'engine: a network other than the default, or no list'
fi

# 4. Remove the run roots and the caches, in the engine's user namespace (mapped UIDs).
if [ -z "$home" ]; then
  fail 'the home folder of the live UID is unknown'
  set -- "$runs"
else
  set -- "$runs" "$home/pm-live-cache"
fi
for path in "$@"; do
  if [ -e "$path" ] || [ -L "$path" ]; then
    "$podman" unshare /bin/rm -rf -- "$path" > /dev/null 2>&1 || fail "$path: the removal failed"
  fi
  if [ -e "$path" ] || [ -L "$path" ]; then fail "$path is still there"; else pass "$path is absent"; fi
done

# 5. in-job: a report, not a gate (R2-4, N1): the live UID can move its processes.
if [ "$mode" = in-job ]; then
  # The live UID and its subordinate UID range in /etc/subuid (crrao m1).
  range=$(/usr/bin/awk -F : -v user="$(/usr/bin/id -un)" -v uid="$uid" '
    ($1 == user || $1 == uid) && NF == 3 { print $2, $2 + $3; exit }' "$root/etc/subuid" 2> /dev/null)
  [ -n "$range" ] || {
    printf '%s\n' 'live-cleanup-check: report: the subordinate UID range is unknown'
    range='-1 -1'
  }
  count=0
  for entry in "$proc"/[0-9]*; do
    state=$(/usr/bin/awk -v uid="$uid" -v low="${range% *}" -v high="${range#* }" '$1 == "Uid:" {
      for (i = 2; i <= 5; i++) if ($i == uid || ($i >= low && $i < high)) { print "live"; exit } }' \
      "$entry/status" 2> /dev/null) || continue
    [ "$state" = live ] || continue
    group=$(/usr/bin/awk -F '::' '/^0::/ { print $2; exit }' "$entry/cgroup" 2> /dev/null)
    case "$group" in /system.slice/pm-live-runner.service | /system.slice/pm-live-runner.service/*) continue ;; esac
    count=$((count + 1))
  done
  printf 'live-cleanup-check: report, not a gate: %s processes of the live UID or its subordinate UIDs outside the runner service\n' "$count"
fi

if [ "$status" -eq 0 ]; then pass "$mode cleanup"; else fail "$mode cleanup: uncertain"; fi
exit "$status"
