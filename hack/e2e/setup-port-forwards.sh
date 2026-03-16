#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
PROMETHEUS_NAMESPACE=${PROMETHEUS_NAMESPACE:-prometheus-operator}
PROMETHEUS_INSTANCE_NAME=${PROMETHEUS_INSTANCE_NAME:-prometheus-shared-e2e}
PROMETHEUS_SERVICE=${PROMETHEUS_SERVICE:-prometheus-operated}
VALKEY_NAMESPACE=${VALKEY_NAMESPACE:-valkey-e2e}
VALKEY_RELEASE_NAME=${VALKEY_RELEASE_NAME:-valkey}
KUBE_CONTEXT=${E2E_KUBECONTEXT:-${CTX:-${KUBECONTEXT:-}}}
GITEA_PORT=${GITEA_PORT:-13000}
PROMETHEUS_PORT=${PROMETHEUS_PORT:-19090}
VALKEY_PORT=${VALKEY_PORT:-16379}

if [[ -z "${KUBE_CONTEXT}" ]]; then
    KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
fi

if [[ -z "${KUBE_CONTEXT}" ]]; then
    echo "❌ Kubernetes context is required (set E2E_KUBECONTEXT or CTX)"
    exit 1
fi

has_expected_forward_processes() {
    local gitea_pf
    local prom_pf
    local valkey_pf

    gitea_pf="$(ps -ef | grep -E "kubectl( |.* )--context ${KUBE_CONTEXT}( |.* )port-forward( |.* )svc/${GITEA_SERVICE:-gitea-http}( |.* )${GITEA_PORT}:${GITEA_PORT}" | grep -v grep || true)"
    prom_pf="$(ps -ef | grep -E "kubectl( |.* )--context ${KUBE_CONTEXT}( |.* )port-forward( |.* )svc/${PROMETHEUS_SERVICE}( |.* )${PROMETHEUS_PORT}:9090" | grep -v grep || true)"
    valkey_pf="$(ps -ef | grep -E "kubectl( |.* )--context ${KUBE_CONTEXT}( |.* )port-forward( |.* )svc/${VALKEY_RELEASE_NAME}( |.* )${VALKEY_PORT}:6379" | grep -v grep || true)"

    [[ -n "${gitea_pf}" && -n "${prom_pf}" && -n "${valkey_pf}" ]]
}

# Fast path: keep existing healthy forwards only if they belong to this context.
if has_expected_forward_processes && \
   curl -fsS http://localhost:${GITEA_PORT}/api/healthz >/dev/null 2>&1 && \
   curl -fsS http://localhost:${PROMETHEUS_PORT}/-/healthy >/dev/null 2>&1 && \
   timeout 2 bash -c "echo >/dev/tcp/localhost/${VALKEY_PORT}" 2>/dev/null; then
    echo "✅ Existing port-forwards are healthy for context ${KUBE_CONTEXT}; skipping restart"
    exit 0
fi

# Cleanup old port-forwards
echo "🧹 Cleaning up old port-forwards..."
pkill -f "kubectl.*port-forward.*${PROMETHEUS_PORT}" || true
pkill -f "kubectl.*port-forward.*${GITEA_PORT}" || true
pkill -f "kubectl.*port-forward.*${VALKEY_PORT}" || true
sleep 1

echo "🔌 Setting up port-forwards for e2e testing..."
echo "🎯 Using kube context: ${KUBE_CONTEXT}"

echo "⏳ Waiting for Prometheus pod to be ready..."
kubectl --context "$KUBE_CONTEXT" wait --for=condition=ready pod \
    -l "prometheus=${PROMETHEUS_INSTANCE_NAME}" \
    -n "$PROMETHEUS_NAMESPACE" \
    --timeout=180s || {
    echo "❌ Prometheus pod failed to become ready"
    kubectl --context "$KUBE_CONTEXT" get pods -n "$PROMETHEUS_NAMESPACE" -l "prometheus=${PROMETHEUS_INSTANCE_NAME}" || true
    exit 1
}

echo "✅ Prometheus pod is ready"

echo "⏳ Waiting for Gitea pod to be ready..."
kubectl --context "$KUBE_CONTEXT" wait --for=condition=ready pod \
    -l app.kubernetes.io/name=gitea \
    -n "$GITEA_NAMESPACE" \
    --timeout=180s || {
    echo "❌ Gitea pod failed to become ready"
    exit 1
}

echo "✅ Gitea pod is ready"

echo "⏳ Waiting for Valkey pod to be ready..."
kubectl --context "$KUBE_CONTEXT" wait --for=condition=ready pod \
    -l app.kubernetes.io/name=valkey,app.kubernetes.io/instance="${VALKEY_RELEASE_NAME}" \
    -n "$VALKEY_NAMESPACE" \
    --timeout=120s || {
    echo "❌ Valkey pod failed to become ready"
    kubectl --context "$KUBE_CONTEXT" get pods -n "$VALKEY_NAMESPACE" || true
    exit 1
}

echo "✅ Valkey pod is ready"

# Generic function to setup a port-forward with verification
# Args: service_name namespace service local_port remote_port
setup_port_forward() {
    local service_name="$1"
    local namespace="$2"
    local service="$3"
    local local_port="$4"
    local remote_port="$5"
    
    # Check if already running
    if ps aux | grep -E "kubectl.*--context[= ]${KUBE_CONTEXT}.*port-forward.*${service}.*${local_port}:${remote_port}" | grep -v grep >/dev/null 2>&1; then
        echo "✅ ${service_name} port-forward already running"
        return 0
    fi

    echo "🔌 Starting ${service_name} port-forward..."
    local pf_log="/tmp/${service_name}-pf.log"
    setsid kubectl --context "$KUBE_CONTEXT" port-forward --address 127.0.0.1 \
        -n "$namespace" "svc/${service}" "${local_port}:${remote_port}" > "$pf_log" 2>&1 < /dev/null &
    local pf_pid=$!
    
    # Give it time to establish
    sleep 2
    
    # Verify port-forward is working
    echo "⏳ Verifying ${service_name} port-forward..."
    for i in {1..10}; do
        # Check if port-forward process is still alive
        if ! kill -0 $pf_pid 2>/dev/null; then
            echo "❌ ${service_name} port-forward process died"
            if [ -f "$pf_log" ]; then
                echo "   Error log:"
                cat "$pf_log" | sed 's/^/   /'
            fi
            return 1
        fi
        
        # Try to connect to the port
        if timeout 2 bash -c "echo >/dev/tcp/localhost/${local_port}" 2>/dev/null; then
            echo "✅ ${service_name} port-forward verified and working"
            return 0
        fi
        
        if [ $i -eq 10 ]; then
            echo "❌ Failed to verify ${service_name} port-forward after 10 attempts"
            return 1
        fi
        
        echo "   Attempt $i/10 failed, retrying in 1 second..."
        sleep 1
    done
}

# Setup port-forwards
setup_port_forward "Prometheus" "$PROMETHEUS_NAMESPACE" "${PROMETHEUS_SERVICE}" "${PROMETHEUS_PORT}" "9090"
setup_port_forward "Gitea" "$GITEA_NAMESPACE" "gitea-http" "${GITEA_PORT}" "${GITEA_PORT}"
setup_port_forward "Valkey" "$VALKEY_NAMESPACE" "${VALKEY_RELEASE_NAME}" "${VALKEY_PORT}" "6379"

# Validate HTTP endpoints before returning success.
curl -fsS http://localhost:${GITEA_PORT}/api/healthz >/dev/null || {
    echo "❌ Gitea health check failed after port-forward setup"
    exit 1
}
curl -fsS http://localhost:${PROMETHEUS_PORT}/-/healthy >/dev/null || {
    echo "❌ Prometheus health check failed after port-forward setup"
    exit 1
}
timeout 2 bash -c "echo >/dev/tcp/localhost/${VALKEY_PORT}" 2>/dev/null || {
    echo "❌ Valkey health check failed after port-forward setup"
    exit 1
}

echo ""
echo "Prometheus: http://localhost:${PROMETHEUS_PORT}"
echo "Gitea: http://localhost:${GITEA_PORT}"
echo "Valkey: localhost:${VALKEY_PORT}"
echo "✅ Port-forwards ready for e2e testing"
