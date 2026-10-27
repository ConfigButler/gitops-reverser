#!/usr/bin/env bash
set -euo pipefail

# Forward a host port to the mutation-capture lab's records API, detached with setsid so it
# outlives the task that starts it, and verified healthy before returning.
#
# A forward left from an earlier run is always replaced: it points at the pod that was current
# then, and a new image rolls a new pod. Only this cluster's forward is touched, so the e2e
# cluster's forwards keep running beside it.
#
# Inputs (env):
# - LAB_CTX (required): the lab's kube context
# - LAB_CS (required): its stamp directory, where the forward's log goes
# - LAB_NAMESPACE (optional, default mutation-capture-lab)
# - LAB_API_PORT (optional, default 18081): the host port
# - KUBECTL (optional): tool override

: "${LAB_CTX:?LAB_CTX is required}"
: "${LAB_CS:?LAB_CS is required}"
LAB_NAMESPACE="${LAB_NAMESPACE:-mutation-capture-lab}"
LAB_API_PORT="${LAB_API_PORT:-18081}"
KUBECTL="${KUBECTL:-kubectl}"
REMOTE_PORT=8080

healthy() { curl -fsS "http://127.0.0.1:${LAB_API_PORT}/healthz" >/dev/null 2>&1; }

pkill -f "port-forward --context ${LAB_CTX} .* ${LAB_API_PORT}:${REMOTE_PORT}" 2>/dev/null || true
sleep 1

"${KUBECTL}" --context "${LAB_CTX}" -n "${LAB_NAMESPACE}" rollout status deployment/mutation-capture-lab \
    --timeout=120s >/dev/null

echo "🔌 Forwarding 127.0.0.1:${LAB_API_PORT} to the lab's records API…"
mkdir -p "${LAB_CS}"
pf_log="${LAB_CS}/port-forward.log"
# The context comes first so the pkill pattern above matches this cluster's forward only.
setsid "${KUBECTL}" port-forward --context "${LAB_CTX}" -n "${LAB_NAMESPACE}" --address 127.0.0.1 \
    deployment/mutation-capture-lab "${LAB_API_PORT}:${REMOTE_PORT}" >"${pf_log}" 2>&1 </dev/null &
pf_pid=$!

for _ in $(seq 1 15); do
    if ! kill -0 "${pf_pid}" 2>/dev/null; then
        echo "❌ the lab port-forward exited:" >&2
        sed 's/^/   /' "${pf_log}" >&2 || true
        exit 1
    fi
    if healthy; then
        echo "✅ The lab's records API answers on http://127.0.0.1:${LAB_API_PORT}"
        exit 0
    fi
    sleep 1
done

echo "❌ the lab port-forward was not healthy after 15s:" >&2
sed 's/^/   /' "${pf_log}" >&2 || true
exit 1
