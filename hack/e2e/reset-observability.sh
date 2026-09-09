#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Give this e2e invocation a clean observability slate: no retained Prometheus samples, and
# controller counters starting from zero.
#
# Order matters. The controller is stopped BEFORE Prometheus is replaced and started again after,
# so the fresh Prometheus can never scrape the previous run's in-process counter values. Resetting
# Prometheus while the old controller is still running reintroduces exactly the history the reset
# exists to remove.
#
# Runs once per invocation, outside the Task stamp graph, so a cached prepare cannot skip it.
# Does not clean up afterwards: the environment stays available for inspection, and the NEXT
# invocation's reset is what discards it.
#
# Required: CTX, NAMESPACE, CONTROLLER_DEPLOY_SELECTOR
set -euo pipefail

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${CONTROLLER_DEPLOY_SELECTOR:?CONTROLLER_DEPLOY_SELECTOR is required}"

PROM_NS="${PROM_NS:-prometheus-operator}"
PROM_NAME="${PROM_NAME:-prometheus-shared-e2e}"

kc() { kubectl --context "${CTX}" "$@"; }

# The emptyDir assumption, asserted rather than trusted: the Prometheus CR carries no spec.storage,
# so deleting the pod discards the TSDB. Attaching a volume would silently make this a no-op.
#
# The read must SUCCEED. Swallowing its error would make an unreachable API indistinguishable from
# an absent storage block, so a failed read would wave through a reset that then preserved history.
if ! storage="$(kc -n "${PROM_NS}" get prometheus "${PROM_NAME}" -o jsonpath='{.spec.storage}')"; then
	echo "ERROR: could not read Prometheus ${PROM_NS}/${PROM_NAME}; refusing to reset blind" >&2
	exit 1
fi
if [[ -n "${storage}" ]]; then
	echo "ERROR: Prometheus ${PROM_NS}/${PROM_NAME} declares spec.storage (${storage}), so deleting" >&2
	echo "       its pod would preserve the TSDB and this reset would do nothing." >&2
	exit 1
fi

deploy="$(kc -n "${NAMESPACE}" get deploy -l "${CONTROLLER_DEPLOY_SELECTOR}" \
	-o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [[ -z "${deploy}" ]]; then
	echo "ERROR: no Deployment matching '${CONTROLLER_DEPLOY_SELECTOR}' in namespace '${NAMESPACE}'" >&2
	exit 1
fi
replicas="$(kc -n "${NAMESPACE}" get deploy "${deploy}" -o jsonpath='{.spec.replicas}')"
replicas="${replicas:-1}"

# Anything after the scale-down that fails must not leave the controller stopped: the environment
# stays up after a failed run precisely so it can be inspected, and an operator-less cluster is the
# least inspectable state there is. The trap preserves the original exit status.
scaled_down=0
restore_replicas() {
	local status=$?
	if [[ "${scaled_down}" -eq 1 ]]; then
		echo "Reset failed; restoring deployment/${deploy} to ${replicas} replica(s)" >&2
		kc -n "${NAMESPACE}" scale "deployment/${deploy}" --replicas="${replicas}" || true
	fi
	exit "${status}"
}
trap restore_replicas EXIT

echo "Stopping deployment/${deploy} so no old counters can be scraped"
scaled_down=1
kc -n "${NAMESPACE}" scale "deployment/${deploy}" --replicas=0
kc -n "${NAMESPACE}" wait --for=delete pod -l "${CONTROLLER_DEPLOY_SELECTOR}" --timeout=120s

echo "Discarding Prometheus data (${PROM_NS}/${PROM_NAME})"
kc -n "${PROM_NS}" delete pod -l "prometheus=${PROM_NAME}" --wait=true --ignore-not-found=true

echo "Starting deployment/${deploy} with counters at zero"
kc -n "${NAMESPACE}" scale "deployment/${deploy}" --replicas="${replicas}"
scaled_down=0

if ! kc -n "${PROM_NS}" rollout status "statefulset/prometheus-${PROM_NAME}" --timeout=180s; then
	echo "ERROR: Prometheus did not become ready after its reset" >&2
	kc -n "${PROM_NS}" get pods -o wide >&2 || true
	exit 1
fi

if ! kc -n "${NAMESPACE}" rollout status "deployment/${deploy}" --timeout=300s; then
	echo "ERROR: controller did not become ready after its reset" >&2
	kc -n "${NAMESPACE}" get deploy,rs,pods -o wide >&2 || true
	exit 1
fi

echo "Observability reset complete"
