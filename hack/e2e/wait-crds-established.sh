#!/bin/bash
# Waits for CRDs that were JUST applied to be Established.
#
# Usage: wait-crds-established.sh <timeout-seconds> <what kubectl wait takes: crd/<name>... or -f FILE>
# Env:   CTX (required), KUBECTL (default kubectl)
#
# `kubectl wait --for=condition=Established` cannot be run straight after the apply that created
# the CRDs. Until the apiextensions controller writes its first conditions, a new CRD carries
# `status.conditions: null`, and kubectl's condition check (ConditionalWait.checkCondition,
# k8s.io/kubectl pkg/cmd/wait/condition.go) fails on that instead of waiting:
#
#   .status.conditions accessor error: <nil> is of the type <nil>, expected []interface{}
#
# so the wait exits 1 within seconds, well inside its timeout. That aborted prepare-e2e on CI runs
# 34824995173 and 35235710425. A `--for=jsonpath=` wait fails the same way on the null list. This
# retries ONLY that error, inside one overall deadline; any other failure, including the timeout,
# fails as before.

set -euo pipefail

: "${CTX:?CTX is required}"
KUBECTL="${KUBECTL:-kubectl}"
timeout_seconds="${1:?timeout in seconds is required}"
shift
[ "$#" -gt 0 ] || { echo "wait-crds-established.sh: name the CRDs to wait for" >&2; exit 2; }

deadline=$(( SECONDS + timeout_seconds ))
while :; do
	remaining=$(( deadline - SECONDS ))
	[ "${remaining}" -gt 0 ] || remaining=1
	if output="$("${KUBECTL}" --context "${CTX}" wait --for=condition=Established \
		--timeout="${remaining}s" "$@" 2>&1)"; then
		printf '%s\n' "${output}"
		exit 0
	fi
	if ! grep -q '\.status\.conditions accessor error: <nil>' <<<"${output}" || [ "${SECONDS}" -ge "${deadline}" ]; then
		printf '%s\n' "${output}" >&2
		exit 1
	fi
	echo "CRD status not written yet; waiting again" >&2
	sleep 1
done
