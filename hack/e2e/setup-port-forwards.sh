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

wait_for_ready_active_pod() {
    local service_name="$1"
    local namespace="$2"
    local selector="$3"
    local timeout_seconds="$4"
    local deadline=$((SECONDS + timeout_seconds))
    local pod_name

    while (( SECONDS < deadline )); do
        pod_name="$(
            kubectl --context "$KUBE_CONTEXT" get pods \
                -n "$namespace" \
                -l "$selector" \
                -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' \
                | head -n 1
        )"

        if [[ -n "${pod_name}" ]]; then
            kubectl --context "$KUBE_CONTEXT" wait --for=condition=ready "pod/${pod_name}" \
                -n "$namespace" \
                --timeout=15s && return 0
        fi

        sleep 2
    done

    echo "❌ ${service_name} pod failed to become ready"
    kubectl --context "$KUBE_CONTEXT" get pods -n "$namespace" -l "$selector" || true
    return 1
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
wait_for_ready_active_pod "Prometheus" "$PROMETHEUS_NAMESPACE" "prometheus=${PROMETHEUS_INSTANCE_NAME}" 180 || exit 1

echo "✅ Prometheus pod is ready"

echo "⏳ Waiting for Gitea pod to be ready..."
wait_for_ready_active_pod "Gitea" "$GITEA_NAMESPACE" "app.kubernetes.io/name=gitea" 180 || exit 1

echo "✅ Gitea pod is ready"

echo "⏳ Waiting for Valkey pod to be ready..."
wait_for_ready_active_pod \
    "Valkey" \
    "$VALKEY_NAMESPACE" \
    "app.kubernetes.io/name=valkey,app.kubernetes.io/instance=${VALKEY_RELEASE_NAME}" \
    120 || exit 1

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
