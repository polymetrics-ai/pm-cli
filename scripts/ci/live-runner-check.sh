#!/bin/sh
# The pre-run check of the live runner (D2-A, L-e1 P6, crrao C4). The owner runs
# the root-owned copy in /etc/pm-live/hooks as root, with env -i PATH=/usr/bin:/bin:
# with no option before each start (owner step 12), with --gate after
# terminate-user (step 13a: only the /proc gate, no change), and with --record
# once at install, last (step 6a). The check never records (R2-6). Any failure
# exits 1. See "Live runner (D2-A)" in docs/development/ci.md.
if [ "${1:-}" != --clean-env ]; then
  exec /usr/bin/env -i PATH=/usr/bin:/bin /bin/sh "$0" --clean-env "$@"
fi
shift
set -u
# The fixed paths and tools. The developer checks replace only these lines.
root=
proc=/proc
uid_of() { /usr/bin/id -u "$1"; }
home_of() { /usr/bin/getent passwd "$1" | /usr/bin/cut -d: -f6; }
entry_of() { /usr/bin/stat -c '%u %g %f' -- "$1"; }
sha256_of() { /usr/bin/sha256sum -- "$1"; }
crontab_of() { /usr/bin/crontab -u "$1" -l; }
put() { printf '%s %s %s\n' "$@"; }
sync_file() { /usr/bin/sync -- "$1"; }
grep=/usr/bin/grep
python=/usr/bin/python3

user=pmlive
# absent PATH...: 0 when each is proved absent (ENOENT), 1 when one exists, 2 on
# another lookup error, for example ELOOP or EACCES on an ancestor (#403 N3).
absent() {
  "$python" -I -B -c '
import os, sys
for path in sys.argv[1:]:
    try:
        os.lstat(path)
    except FileNotFoundError:
        continue
    except OSError:
        sys.exit(2)
    sys.exit(1)' "$@"
}
# The at jobs, from atq. Without atq, no at scheduler or spool may exist (F5).
at_jobs() {
  if [ -x "$root/usr/bin/atq" ]; then
    "$root/usr/bin/atq"
    return
  fi
  absent "$root/usr/sbin/atd" "$root/usr/bin/atd" "$root/var/spool/cron/atjobs" "$root/var/spool/cron/atspool" \
    "$root/var/spool/at" "$root/var/spool/atjobs"
}
runner="$root/opt/pm-live/actions-runner"
work="$root/var/lib/pm-live/work"
unit="$root/etc/systemd/system/pm-live-runner.service"
list="$root/etc/pm-live/runner-files.sha256"
status=0
pass() { printf 'live-runner-check: ok: %s\n' "$1"; }
fail() {
  printf 'live-runner-check: FAILED: %s\n' "$1"
  status=1
}
case "${1:-}" in
  '' | --record | --gate) ;;
  *)
    printf '%s\n' 'usage: live-runner-check.sh [--record | --gate]' >&2
    exit 2
    ;;
esac
uid=$(uid_of "$user") && home=$(home_of "$user") && [ -n "$home" ] && [ -d "$home" ] && [ ! -L "$home" ] || {
  fail "the account $user or its home folder is missing"
  exit 1
}

# owner, group, raw mode, SHA-256 (or link target) and path of each runner, unit and
# home file, except the emptied folders. Any failure fails the list (#403 F2, N1).
current_list() {
  listing=$(/usr/bin/mktemp) || return 1
  if ! /usr/bin/find "$runner" "$unit" "$home" \( -path "$runner/_diag" -o -path "$home/pm-live-cache" \
    -o -path "$home/.local/share/containers" \) -prune -o -print > "$listing" ||
    ! /usr/bin/sort -o "$listing" -- "$listing"; then
    /bin/rm -f -- "$listing"
    return 1
  fi
  while IFS= read -r path; do
    if [ -L "$path" ]; then
      content=$(/usr/bin/readlink -- "$path") || break
      content="link:$content"
    elif [ -f "$path" ]; then
      content=$(sha256_of "$path") || break
      content=${content%% *}
      case "$content" in '' | *[!0-9a-f]*) break ;; esac
      [ "${#content}" -eq 64 ] || break
    else
      content=-
    fi
    entry=$(entry_of "$path") || break
    put "$entry" "$content" "${path#"$root"}" || break
    path=
  done < "$listing"
  /bin/rm -f -- "$listing"
  # read empties PATH at the end of the list; a break keeps it.
  [ -z "$path" ]
}
forbidden_files() {
  if [ -e "$home/.ssh" ] || [ -L "$home/.ssh" ]; then fail "$home/.ssh exists"; fi
  if [ -n "$(/bin/ls -A -- "$home/.config/systemd/user" 2> /dev/null)" ]; then fail 'a user unit or timer exists'; fi
}

if [ "${1:-}" = --record ]; then
  forbidden_files
  [ "$status" -eq 0 ] || exit 1
  # Only a complete, flushed new list replaces the old one (#403 N1).
  temporary=$(/usr/bin/mktemp "$list.XXXXXX") || exit 1
  if current_list > "$temporary" && sync_file "$temporary" && /bin/chmod 0644 "$temporary" &&
    /bin/mv -f -- "$temporary" "$list"; then
    pass "recorded $(/usr/bin/wc -l < "$list" | /usr/bin/tr -d ' ') entries in $list"
    exit 0
  fi
  /bin/rm -f -- "$temporary"
  fail 'the list was not recorded'
  exit 1
fi

# 1. The gate: no process of the live UID or of its one /etc/subuid range (m1), in any
# cgroup (N1), and no runtime folder. An unreadable entry or range fails.
gate=0
range=$(/usr/bin/awk -F : -v user="$user" -v uid="$uid" '
  $1 == user || $1 == uid { lines++; if (NF == 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[1-9][0-9]*$/) { low = $2; high = $2 + $3 } }
  END { if (lines == 1 && high != "") print low, high }' "$root/etc/subuid" 2> /dev/null)
if [ -z "$range" ]; then
  fail "the subordinate UID range of $user in /etc/subuid is missing, unreadable or not one range" && gate=1
  range='-1 -1'
fi
# A real /proc always lists PID 1. An unlisted folder proves nothing (#403 F3).
if ! entries=$(/bin/ls -- "$proc" 2> /dev/null); then
  fail "the process folder $proc cannot be listed" && gate=1
elif ! printf '%s\n' "$entries" | /usr/bin/grep -qx '[0-9][0-9]*'; then
  fail "the process folder $proc lists no process" && gate=1
fi
for entry in "$proc"/[0-9]*; do
  [ -d "$entry" ] || continue
  state=$(/usr/bin/awk -v uid="$uid" -v low="${range% *}" -v high="${range#* }" '$1 == "Uid:" {
    for (i = 2; i <= 5; i++) if ($i == uid || ($i >= low && $i < high)) { print "live"; exit }
    print "other"; exit }' "$entry/status" 2> /dev/null)
  if [ -z "$state" ]; then
    # An entry that is gone ended after the listing.
    [ ! -d "$entry" ] || { fail "process entry ${entry##*/} cannot be read" && gate=1; }
    continue
  fi
  [ "$state" = other ] || { fail "process ${entry##*/} runs as $user or a subordinate UID of it" && gate=1; }
done
if [ -e "$root/run/user/$uid" ]; then fail "$root/run/user/$uid exists" && gate=1; fi
[ "$gate" -ne 0 ] || pass "no $user process and no runtime folder"
if [ "${1:-}" = --gate ]; then
  if [ "$status" -eq 0 ]; then pass 'the gate passed'; else fail 'the gate failed: the cleanup is uncertain'; fi
  exit "$status"
fi

# 2. Empty the work folder, _diag, the engine storage and the cache folder, only after
# the gate (no live process can change a path). No path component may be a link.
for folder in "$work" "$runner/_diag" "$home/.local/share/containers" "$home/pm-live-cache"; do
  if [ "$gate" -ne 0 ]; then
    fail "$folder: not emptied, because the gate failed"
  elif [ -L "$folder" ] || [ -L "$home/.local" ] || [ -L "$home/.local/share" ]; then
    fail "$folder: a link in the path"
  elif [ -d "$folder" ]; then
    # Each status counts: a failed deletion or listing is no proof (#403 N2).
    if ! /usr/bin/find "$folder" -mindepth 1 -delete 2> /dev/null; then
      fail "$folder: the deletion failed"
    elif ! left=$(/bin/ls -A -- "$folder" 2> /dev/null); then
      fail "$folder: cannot be listed"
    elif [ -z "$left" ]; then pass "$folder is empty"; else fail "$folder is not empty"; fi
  else
    pass "$folder is absent"
  fi
done

# 3. Compare the files with the root-held list, only after the gate (crrao I2).
if [ "$gate" -ne 0 ]; then
  fail 'the files were not compared, because the gate failed'
elif [ ! -f "$list" ] || [ -L "$list" ]; then
  fail 'the root-held file list is missing'
else
  temporary=$(/usr/bin/mktemp) || exit 1
  if ! current_list > "$temporary"; then
    fail 'the runner, unit or home files cannot be listed, read or hashed'
  elif /usr/bin/cmp -s -- "$list" "$temporary"; then
    pass 'the runner, unit and home files match the list'
  else
    fail 'the runner, unit or home files differ from the list:'
    /usr/bin/diff -- "$list" "$temporary" | /usr/bin/sed -n 's/^[<>] /  /p'
  fi
  /bin/rm -f -- "$temporary"
fi
[ "$gate" -ne 0 ] || forbidden_files

# 4. No crontab and no at job.
if crontab=$(crontab_of "$user" 2>&1); then
  fail "$user has a crontab"
elif [ "$crontab" = "no crontab for $user" ]; then
  pass 'no crontab'
else
  fail 'the crontab check cannot run'
fi
if ! jobs=$(at_jobs 2> /dev/null); then
  fail 'the at job check cannot run'
elif printf '%s\n' "$jobs" | /usr/bin/awk -v user="$user" '$NF == user { found = 1 } END { exit !found }'; then
  fail "$user has an at job"
else
  pass 'no at job'
fi

# 5. The runner settings and the unit, only after the gate (#403 N5).
if [ "$gate" -ne 0 ]; then
  fail 'the runner and unit settings were not read, because the gate failed'
else
  if "$grep" -Eq '"disableUpdate"[[:space:]]*:[[:space:]]*true' -- "$runner/.runner" 2> /dev/null; then
    pass 'the runner was configured with --disableupdate'
  else
    fail 'the runner was not configured with --disableupdate'
  fi
  for line in "User=$user" ExecStart=/opt/pm-live/actions-runner/run.sh EnvironmentFile=/opt/pm-live/actions-runner/.env; do
    "$grep" -qxF -- "$line" "$unit" 2> /dev/null || fail "the unit has no line $line"
  done
  if "$grep" -E '^[[:space:]]*Slice=' -- "$unit" 2> /dev/null | "$grep" -qvxF Slice=system.slice; then
    fail 'the unit runs outside system.slice'
  fi
  # The service stop must kill each process of the cgroup, whatever its UID.
  if "$grep" -E '^[[:space:]]*KillMode=' -- "$unit" 2> /dev/null | "$grep" -qvxF KillMode=control-group; then
    fail 'the unit has a KillMode other than control-group'
  fi
  # A drop-in folder could change the unit (crrao I9).
  absent "$root/etc/systemd/system/pm-live-runner.service.d" "$root/run/systemd/system/pm-live-runner.service.d" \
    "$root/usr/lib/systemd/system/pm-live-runner.service.d" || fail 'a drop-in folder of the unit exists or cannot be checked'
fi

if [ "$status" -eq 0 ]; then pass 'the runner may start'; else fail 'do not start the runner'; fi
exit "$status"
