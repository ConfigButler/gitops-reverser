#!/usr/bin/env bash
# Prints the commit message GitHub writes when it squash-merges BASE..HEAD, for this repository's
# settings (squash_merge_commit_title COMMIT_OR_PR_TITLE, squash_merge_commit_message
# COMMIT_MESSAGES):
#
#   one commit     → that commit's own message; the PR title is not used
#   several commits → the PR title, a blank line, then every commit message oldest first as "* <message>"
#
# Usage: hack/release-notes/squash-message.sh BASE HEAD [PR_TITLE]
# Locally, the title defaults to a placeholder, which is enough to prove the body parses; CI passes
# the real one.
set -euo pipefail

base=${1:?usage: squash-message.sh BASE HEAD [PR_TITLE]}
head=${2:?usage: squash-message.sh BASE HEAD [PR_TITLE]}
title=${3:-"chore: local squash-message check"}

range="$(git merge-base "$base" "$head")..$head"
count=$(git rev-list --count "$range")

case "$count" in
  0) echo "no commits in $range" >&2; exit 1 ;;
  1) git log -1 --format=%B "$head" ;;
  *)
    printf '%s\n\n' "$title"
    git log --reverse --format='* %B' "$range"
    ;;
esac
