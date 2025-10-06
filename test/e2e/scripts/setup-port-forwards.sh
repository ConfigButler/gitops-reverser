#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
PROMETHEUS_NAMESPACE=${PROMETHEUS_NAMESPACE:-prometheus-e2e}

echo "🔌 Setting up port-forwards for e2e testing..."

# Wait for pods to be ready before attempting port-forwards
echo "⏳ Waiting for Prometheus pod to be ready..."
kubectl wait --for=condition=ready pod \
    -l app=prometheus \
    -n "$PROMETHEUS_NAMESPACE" \
    --timeout=60s || {
    echo "❌ Prometheus pod failed to become ready"
    exit 1
}

echo "⏳ Waiting for Gitea pod to be ready..."
kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=gitea \
    -n "$GITEA_NAMESPACE" \
    --timeout=60s || {
    echo "❌ Gitea pod failed to become ready"
    exit 1
}

echo "✅ All pods are ready"

# Generic function to setup a port-forward with verification
# Args: service_name namespace service port health_check_url
setup_port_forward() {
    local service_name="$1"
    local namespace="$2"
    local service="$3"
    local port="$4"
    local health_url="$5"
    
    # Check if already running
    if ps aux | grep -E "kubectl.*port-forward.*${service}.*${port}" | grep -v grep >/dev/null 2>&1; then
        echo "✅ ${service_name} port-forward already running"
        return 0
    fi

    echo "🔌 Starting ${service_name} port-forward..."
    nohup kubectl port-forward --address 0.0.0.0 -n "$namespace" "svc/${service}" "${port}:${port}" >/dev/null 2>&1 &
    
    # Give it time to establish
    sleep 3
    
    # Verify port-forward is working
    echo "⏳ Verifying ${service_name} port-forward..."
    for i in {1..5}; do
        if curl -s -f "$health_url" >/dev/null 2>&1; then
            echo "✅ ${service_name} port-forward verified and working"
            return 0
        fi
        
        if [ $i -eq 5 ]; then
            echo "❌ Failed to verify ${service_name} port-forward after 5 attempts"
            return 1
        fi
        
        echo "   Attempt $i/5 failed, retrying in 2 seconds..."
        sleep 2
    done
}

# Setup port-forwards
setup_port_forward "Prometheus" "$PROMETHEUS_NAMESPACE" "prometheus" "9090" "http://localhost:9090/-/ready"
setup_port_forward "Gitea" "$GITEA_NAMESPACE" "gitea-http" "3000" "http://localhost:3000/api/v1/version"

echo ""
echo "📊 Prometheus: http://localhost:9090"
echo "📁 Gitea: http://localhost:3000"
echo "✅ Port-forwards ready for e2e testing"
