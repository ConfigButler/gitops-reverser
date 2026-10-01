#!/usr/bin/env bash
# Self-test for squash-message.sh and check.mjs: builds throwaway repositories and asserts the
# verdict for each shape of squash message. Needs `npm ci` in this directory first.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0

# expect NAME WANT(pass|fail) TITLE SUBJECT...
# Creates a repository with an empty base commit plus one commit per SUBJECT, then checks the
# squash message GitHub would write for merging them under TITLE.
expect() {
  local name=$1 want=$2 title=$3
  shift 3
  local repo="$work/$name"
  git init -q "$repo"
  git -C "$repo" -c commit.gpgsign=false -c user.name=t -c user.email=t@t commit -q --allow-empty -m "chore: base"
  local subject
  for subject in "$@"; do
    git -C "$repo" -c commit.gpgsign=false -c user.name=t -c user.email=t@t commit -q --allow-empty -m "$subject"
  done
  # Built first, and on its own: under set -e a crash in squash-message.sh aborts the run here,
  # instead of reading as the parse failure a "want fail" case expects.
  local message got=pass
  message=$(cd "$repo" && "$here/squash-message.sh" "HEAD~$#" HEAD "$title")
  node "$here/check.mjs" <<<"$message" >/dev/null || got=fail
  if [[ "$got" == "$want" ]]; then
    echo "ok   $name"
  else
    echo "FAIL $name: want $want, got $got"
    failures=$((failures + 1))
  fi
}

# One commit: GitHub uses the commit's own message, so a good PR title cannot hide a bad subject.
expect single-good pass "feat: title" "feat: work"
expect single-bad-subject fail "feat: good title" "feat(a(b)): work"
expect single-bad-body fail "feat: good title" $'feat: work\n\nf(a(b)) opens this line'
# Several commits: the PR title leads, and each message is bulleted, so a line-leading token
# inside a commit's subject is shielded by "* " while one in a body is not.
expect multi-good pass "feat: title" "feat: one" "fix: two"
expect multi-bad-title fail "feat(a(b)): title" "feat: one" "fix: two"
expect multi-bad-body fail "feat: title" "feat: one" $'fix: two\n\nf(a(b)) opens this line'

exit "$failures"
