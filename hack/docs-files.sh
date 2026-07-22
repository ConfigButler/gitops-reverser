#!/usr/bin/env bash
# The single place that decides which markdown files the documentation linters see.
#
# markdownlint-cli2 and Vale each own their own *rules*, but they must agree on
# the *files*, and that list has to be built the same way locally and in CI.
# Centralizing it here is what makes `task lint-markdown` and the CI lint job
# provably the same check.
#
# Usage:
#   hack/docs-files.sh gated   the files .docs-lint-scope lists (default)
#   hack/docs-files.sh all     every tracked .md file
#
# Prints one repo-relative path per line, sorted, and nothing else. Callers use
# `xargs -r` so an empty list runs no linter rather than linting the whole tree.
#
# `gated` reads a committed list rather than deriving one from the git diff. The
# diff-based version needed a merge base, which needed history, which CI does not
# have by default -- and when the base was unreachable the file list silently
# became empty and the gate passed everything. A committed list has no such
# failure mode: it is identical on every machine, needs no history, and grows by
# review rather than by accident.
#
# Always `git ls-files`, never a recursive glob. external-sources/ holds
# gitignored upstream checkouts containing symlink cycles, and an unrooted `**`
# over them has OOM-killed the host once already (see the lint-doc-links comment
# in Taskfile-build.yml). Going through git also excludes gitignored files free.
#
# Exclusions are deliberately NOT applied here. CHANGELOG.md and docs/finished/
# are excluded by each tool's own config (`ignores` in .markdownlint-cli2.jsonc,
# the per-file stanzas in .vale.ini), so there is one list of files and one list
# of exclusions rather than two of each that can disagree.
set -euo pipefail

SCOPE_FILE=".docs-lint-scope"

mode="${1:-gated}"

case "${mode}" in
all)
	git ls-files '*.md'
	;;
gated)
	if [ ! -f "${SCOPE_FILE}" ]; then
		echo "docs-files.sh: ${SCOPE_FILE} is missing; cannot tell which files are gated." >&2
		exit 1
	fi

	# Strip comments and blank lines. Everything left is a path.
	listed="$(sed -e 's/#.*//' -e 's/[[:space:]]*$//' "${SCOPE_FILE}" | grep -v '^$' || true)"

	if [ -z "${listed}" ]; then
		echo "docs-files.sh: ${SCOPE_FILE} lists no files; the docs gate would pass vacuously." >&2
		exit 1
	fi

	# A path that git does not track is a typo, a rename, or a deletion. Any of
	# those quietly shrinks the gate, which is the one failure this file exists to
	# prevent, so it is an error rather than a warning.
	missing=""
	while IFS= read -r f; do
		git ls-files --error-unmatch -- "${f}" >/dev/null 2>&1 || missing="${missing}  ${f}"$'\n'
	done <<EOF
${listed}
EOF

	if [ -n "${missing}" ]; then
		echo "docs-files.sh: ${SCOPE_FILE} lists paths that git does not track:" >&2
		printf '%s' "${missing}" >&2
		echo "  Fix the path, or drop the line if the file is gone." >&2
		exit 1
	fi

	printf '%s\n' "${listed}"
	;;
*)
	echo "usage: $0 [gated|all]" >&2
	exit 2
	;;
esac | sort -u
