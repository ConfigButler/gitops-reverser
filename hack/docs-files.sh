#!/usr/bin/env bash
# The single place that decides which markdown files the documentation linters see.
#
# markdownlint-cli2, Vale, and hack/doccheck each own their own *rules*, but they
# must agree on the *files*, and that list has to be built the same way locally and
# in CI. Centralizing it here is what makes `task lint-markdown` and the CI lint job
# provably the same check.
#
# Usage:
#   hack/docs-files.sh all       every tracked .md file
#   hack/docs-files.sh changed   the .md files this branch touches (default)
#
# Prints one repo-relative path per line, sorted, and nothing else. An empty list is
# a valid answer -- callers must use `xargs -r` so a branch that touches no docs runs
# no linter rather than linting the whole tree by accident.
#
# Always `git ls-files`, never a recursive glob. external-sources/ holds gitignored
# upstream checkouts containing symlink cycles, and an unrooted `**` over them has
# OOM-killed the host once already (see the lint-docs comment in Taskfile-build.yml).
# Going through git also means gitignored and untracked-but-unwanted files are
# excluded for free.
#
# Exclusions are deliberately NOT applied here. CHANGELOG.md and docs/finished/ are
# excluded by each tool's own config (`ignores` in .markdownlint-cli2.jsonc, the
# per-file stanzas in .vale.ini), so there is one list of files and one list of
# exclusions rather than two of each that can disagree.
set -euo pipefail

mode="${1:-changed}"

case "${mode}" in
all)
	git ls-files '*.md'
	;;
changed)
	# Base ref resolution, most specific first:
	#   DOCS_BASE_REF   explicit override, for a local run against another branch
	#   GITHUB_BASE_REF set by GitHub Actions on pull_request. Used rather than a
	#                   hardcoded main because this repo deliberately builds every
	#                   PR base, including stacked PRs whose base is another branch.
	#   origin/HEAD     the remote's default branch, whatever it is called
	base="${DOCS_BASE_REF:-}"
	if [ -z "${base}" ] && [ -n "${GITHUB_BASE_REF:-}" ]; then
		base="origin/${GITHUB_BASE_REF}"
	fi
	if [ -z "${base}" ]; then
		base="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || echo origin/main)"
	fi

	# A merge base needs history, and CI checks out shallow by default, so the lint
	# job sets fetch-depth: 0. If the base is unreachable the honest answer differs
	# by where we are:
	#
	#   in CI  fail. The working tree is clean there, so "lint what changed" would
	#          quietly become "lint nothing" and the gate would pass everything.
	#          A shallow checkout is a misconfiguration and must be loud.
	#   local  warn and carry on with the working tree. A detached HEAD or a missing
	#          remote is normal while developing and must not block a lint run.
	if merge_base="$(git merge-base HEAD "${base}" 2>/dev/null)"; then
		git diff --name-only --diff-filter=d "${merge_base}" -- '*.md'
	elif [ -n "${CI:-}" ]; then
		echo "docs-files.sh: base ref '${base}' is unreachable in CI." >&2
		echo "  The docs gate would pass vacuously. Check fetch-depth on the lint job's checkout." >&2
		exit 1
	else
		echo "docs-files.sh: base ref '${base}' unreachable, limiting to the working tree" >&2
	fi

	# Uncommitted edits and new files, so `task lint` sees what you are about to
	# commit and not only what you already did.
	git diff --name-only --diff-filter=d -- '*.md'
	git ls-files --others --exclude-standard -- '*.md'
	;;
*)
	echo "usage: $0 [all|changed]" >&2
	exit 2
	;;
esac | sort -u
