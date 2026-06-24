#!/bin/bash
set -euo pipefail

# Establish a robust port-forward to the mutation-capture lab's records API
# (the controller pod's :8081), mirroring hack/e2e/setup-port-forwards.sh: the
# forward is detached with setsid so it survives this shell, verified for both
# liveness and a healthy /healthz, and reused on the fast path when already up.
#
# The lab API is not on the controller Service (which exposes only audit/webhook/
# metrics), so this forwards to the Deployment, which re-resolves to the current
# pod — important because each image swap rolls a new pod.
#
# Inputs (env):
# - CTX / E2E_KUBECONTEXT (required): kube context
# - NAMESPACE (required): controller namespace
# - LAB_API_PORT (optional, default 18081): local port
# - CONTROLLER_DEPLOY_SELECTOR (optional): label selector for the controller
# - REMOTE_PORT (optional, default 8081): the lab API port in the pod

KUBE_CONTEXT="${E2E_KUBECONTEXT:-${CTX:-}}"
NAMESPACE="${NAMESPACE:?NAMESPACE is required}"
LAB_API_PORT="${LAB_API_PORT:-18081}"
SELECTOR="${CONTROLLER_DEPLOY_SELECTOR:-app.kubernetes.io/part-of=gitops-reverser}"
REMOTE_PORT="${REMOTE_PORT:-8081}"

if [[ -z "${KUBE_CONTEXT}" ]]; then
    KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
fi
[[ -n "${KUBE_CONTEXT}" ]] || { echo "❌ kube context is required (set CTX/E2E_KUBECONTEXT)"; exit 1; }

healthy() { curl -fsS "http://127.0.0.1:${LAB_API_PORT}/healthz" >/dev/null 2>&1; }

# No fast-path reuse: the lab pod is rolled by every image swap, so a surviving
# forward from a previous run points at the now-terminated old pod. Always tear
# down any existing forward and re-establish against the current ready pod.
echo "🧹 Cleaning up any stale lab port-forward on ${LAB_API_PORT}…"
pkill -f "kubectl.*port-forward.*${LAB_API_PORT}:${REMOTE_PORT}" 2>/dev/null || true
sleep 1

# The Deployment carries SELECTOR, but its Pods do not — derive the pod selector
# from the Deployment's own .spec.selector.matchLabels.
deploy="$(kubectl --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" get deploy -l "${SELECTOR}" -o name | head -n1)"
[[ -n "${deploy}" ]] || { echo "❌ no controller Deployment matching ${SELECTOR}"; exit 1; }
pod_selector="$(kubectl --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" get "${deploy}" \
    -o go-template='{{range $k,$v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}')"
pod_selector="${pod_selector%,}"
[[ -n "${pod_selector}" ]] || { echo "❌ ${deploy} has no pod selector"; exit 1; }

echo "⏳ Waiting for the lab pod to be ready (selector ${pod_selector})…"
deadline=$((SECONDS + 120))
pod=""
while (( SECONDS < deadline )); do
    pod="$(kubectl --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" get pods -l "${pod_selector}" \
        -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' \
        | head -n1)"
    if [[ -n "${pod}" ]] && kubectl --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" \
        wait --for=condition=ready "pod/${pod}" --timeout=15s >/dev/null 2>&1; then
        break
    fi
    pod=""
    sleep 2
done
[[ -n "${pod}" ]] || { echo "❌ no ready lab pod matching ${pod_selector}"; exit 1; }

echo "🔌 Starting lab API port-forward (${LAB_API_PORT} -> ${REMOTE_PORT})…"
pf_log="/tmp/lab-api-pf.log"
setsid kubectl --context "${KUBE_CONTEXT}" -n "${NAMESPACE}" port-forward --address 127.0.0.1 \
    "pod/${pod}" "${LAB_API_PORT}:${REMOTE_PORT}" >"${pf_log}" 2>&1 < /dev/null &
pf_pid=$!

for i in {1..15}; do
    if ! kill -0 "${pf_pid}" 2>/dev/null; then
        echo "❌ lab port-forward process died:"; sed 's/^/   /' "${pf_log}" 2>/dev/null || true
        exit 1
    fi
    if healthy; then
        echo "✅ lab API port-forward verified on http://127.0.0.1:${LAB_API_PORT}"
        exit 0
    fi
    sleep 1
done

echo "❌ lab API port-forward did not become healthy after 15s:"; sed 's/^/   /' "${pf_log}" 2>/dev/null || true
exit 1
