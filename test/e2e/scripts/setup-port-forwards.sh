#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
PROMETHEUS_NAMESPACE=${PROMETHEUS_NAMESPACE:-prometheus-e2e}

# Cleanup old port-forwards
echo "🧹 Cleaning up old port-forwards..."
pkill -f "kubectl port-forward.*19090" || true
pkill -f "kubectl port-forward.*13000" || true
sleep 1

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
    local pf_log="/tmp/${service_name}-pf.log"
    kubectl port-forward --address 127.0.0.1 -n "$namespace" "svc/${service}" "${port}:${port}" > "$pf_log" 2>&1 &
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
        if timeout 2 bash -c "echo >/dev/tcp/localhost/${port}" 2>/dev/null; then
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
setup_port_forward "Prometheus" "$PROMETHEUS_NAMESPACE" "prometheus" "19090" "http://localhost:19090/-/ready"
setup_port_forward "Gitea" "$GITEA_NAMESPACE" "gitea-http" "13000" "http://localhost:13000/api/v1/version"

echo ""
echo "📊 Prometheus: http://localhost:19090"
echo "📁 Gitea: http://localhost:13000"
echo "✅ Port-forwards ready for e2e testing"
