#!/bin/sh
# Git commit-msg hook: the subject line must follow Conventional Commits 1.0.0,
# as AGENTS.md says. A failure stops the commit.
set -eu
message_file=${1:?commit message file required}
subject=$(sed -n '/^[^#]/{p;q;}' "$message_file")
case "$subject" in
  'Merge '* | 'Revert "'* | 'fixup! '* | 'squash! '* | 'amend! '*) exit 0 ;;
esac
types='feat|fix|docs|test|refactor|perf|build|ci|chore|revert|style'
fail() {
  printf 'commit-msg: %s\n' "$1" >&2
  printf '%s\n' 'Use Conventional Commits 1.0.0: type(scope): description' \
    'Example: feat(cli): show the connection id in pm runs show' \
    "Types: $types. The scope is optional. Add ! before the colon for a breaking change." >&2
  exit 1
}
printf '%s\n' "$subject" | grep -Eq "^($types)(\([a-z0-9][a-z0-9._/-]*\))?!?: " ||
  fail "the subject does not start with a type: $subject"
description=${subject#*: }
printf '%s\n' "$description" | grep -Eq '^[^A-Z ]' ||
  fail 'start the description with a lower-case word'
case "$description" in
  *.) fail 'do not end the description with a period' ;;
esac
[ "${#description}" -le 72 ] || fail 'keep the description to 72 characters or fewer'
