#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fail if any tracked file names a `gitopsreverser_` metric that no instrument registers.
#
# This exists because a renamed metric leaves its callers behind silently. When the metric
# surface was rebuilt around the pipeline, `hack/attribution-diagnostics.sh` was left querying
# a deleted `audit_eventlist_events_total`; because operational queries are written defensively
# as `... or vector(0)`, the script kept printing "audit events INGESTED by us = 0" on a
# perfectly healthy cluster. A dead metric name does not error — it reads as a confident zero,
# which is worse than an error and is exactly the failure the metrics work exists to remove.
#
# The check is a set difference: every metric-shaped token in the tree, minus every name the
# exporter actually registers. It is not a parser and does not need to be.
set -euo pipefail

cd "$(dirname "$0")/.."

# Files that legitimately name metrics which no longer exist:
#   UPGRADING.md    the old -> new migration tables; naming the old name is the point
#   docs/design/    the plan's "Deleted" and "Renamed" sections argue about past names
#   docs/finished/  shipped-and-archived plans, frozen at their own moment
EXCLUDED_PATHS=(':!external-sources' ':!docs/UPGRADING.md' ':!docs/design' ':!docs/finished')

# Tokens that look like metric names but are not references to one. Keep this list short and
# justified; anything added here is a check that stopped checking something.
#
#   gitopsreverser_audit_     a prefix truncated mid-sentence in prose
#   gitopsreverser_does_not_  a deliberately-absent name in a negative test
#   gitopsreverser_foo_       the histogram teaching example in interpreting-metrics.md
#
# The truncated forms are matched as well as the full names, because the two comment lines
# above are themselves tracked text this check reads: written without them, the check failed
# on its own explanation of itself.
ALLOWED_NON_METRICS='^gitopsreverser_(audit_|does_not_(exist)?|foo_(seconds)?)$'

referenced="$(
  git grep -hoE 'gitopsreverser_[a-z0-9_]+' -- . "${EXCLUDED_PATHS[@]}" |
    sed -E 's/_(bucket|sum|count)$//' |
    grep -vE "${ALLOWED_NON_METRICS}" |
    sort -u
)"

# Registered names come from the two files that declare them: the instrument specs in
# exporter.go carry the full name, the observable-gauge source constants in gauges.go carry
# it with the prefix stripped.
registered="$(
  {
    grep -oE '"gitopsreverser_[a-z0-9_]+"' internal/telemetry/exporter.go | tr -d '"'
    grep -oE '= "[a-z0-9_]+"' internal/telemetry/gauges.go | sed 's/= "/gitopsreverser_/; s/"//'
  } | sort -u
)"

dangling="$(comm -23 <(echo "${referenced}") <(echo "${registered}") || true)"

if [ -n "${dangling}" ]; then
  echo "ERROR: these metric names are referenced but no instrument registers them:" >&2
  echo >&2
  while IFS= read -r name; do
    [ -z "${name}" ] && continue
    echo "  ${name}" >&2
    git grep -n --color=never -F "${name}" -- . "${EXCLUDED_PATHS[@]}" |
      sed 's/^/      /' >&2
  done <<<"${dangling}"
  echo >&2
  echo "Either fix the reference, or register the instrument. If the name is deliberately" >&2
  echo "absent (a negative test, a prose example), add it to ALLOWED_NON_METRICS in $0." >&2
  exit 1
fi

echo "metric names OK: $(echo "${referenced}" | grep -c .) referenced, all registered"
