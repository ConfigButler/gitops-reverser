#!/bin/bash
# Script to create Kind cluster with proper host path substitution for Docker-in-Docker

set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER:-gitops-reverser-test-e2e}"
TEMPLATE_FILE="test/e2e/kind/cluster-template.yaml"
CONFIG_FILE="test/e2e/kind/cluster.ignore.yaml"
KIND_CREATE_LOG_FILE="${TMPDIR:-/tmp}/kind-create-${CLUSTER_NAME}.log"
POD_SUBNET="${KIND_POD_SUBNET:-10.244.0.0/16}"

# Check if HOST_PROJECT_PATH is set
if [ -z "$HOST_PROJECT_PATH" ]; then
    echo "❌ ERROR: HOST_PROJECT_PATH environment variable is not set"
    echo "This should be set in .devcontainer/devcontainer.json"
    exit 1
fi

echo "🔧 Using HOST_PROJECT_PATH: $HOST_PROJECT_PATH"

# Use envsubst to replace ${HOST_PROJECT_PATH} in template
echo "📝 Generating Kind cluster configuration from template..."
envsubst < "$TEMPLATE_FILE" > "$CONFIG_FILE"

echo "✅ Generated configuration:"
cat "$CONFIG_FILE"
echo ""

create_primary_cluster() {
    kind create cluster --name "$CLUSTER_NAME" --config "$CONFIG_FILE" --wait 5m --retain 2>&1 | tee "$KIND_CREATE_LOG_FILE"
}

# Known Kind bootstrap race in DOOD setups: https://github.com/kubernetes-sigs/kind/issues/2867
is_known_kind_bootstrap_flake() {
    grep -Eq \
        "failed to apply overlay network|failed to remove control plane taint|failed to remove control plane load balancer label|failed to download openapi|couldn't get current server API group list|The connection to the server .*:6443 was refused" \
        "$KIND_CREATE_LOG_FILE"
}

run_dood_self_heal() {
    local control_plane_container="${CLUSTER_NAME}-control-plane"
    docker inspect "$control_plane_container" >/dev/null 2>&1 || return 1

    local ready=false
    for _ in $(seq 1 120); do
        if docker exec "$control_plane_container" kubectl --kubeconfig=/etc/kubernetes/admin.conf get --raw=/readyz >/dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 2
    done
    if [ "$ready" != "true" ]; then
        docker exec "$control_plane_container" crictl ps -a || true
        return 1
    fi

    local applied=false
    for _ in $(seq 1 40); do
        if docker exec "$control_plane_container" sh -lc \
            "sed 's|{{ \\.PodSubnet }}|${POD_SUBNET}|g' /kind/manifests/default-cni.yaml > /tmp/default-cni-rendered.yaml && \
             kubectl --kubeconfig=/etc/kubernetes/admin.conf apply --validate=false -f /tmp/default-cni-rendered.yaml && \
             kubectl --kubeconfig=/etc/kubernetes/admin.conf apply --validate=false -f /kind/manifests/default-storage.yaml"; then
            applied=true
            break
        fi
        sleep 2
    done
    [ "$applied" = "true" ] || return 1

    docker exec "$control_plane_container" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
        taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true
    docker exec "$control_plane_container" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
        label nodes --all node.kubernetes.io/exclude-from-external-load-balancers- >/dev/null 2>&1 || true

    docker exec "$control_plane_container" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
        -n kube-system rollout status daemonset/kindnet --timeout=300s

    docker exec "$control_plane_container" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
        wait --for=condition=Ready nodes --all --timeout=300s
}

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "♻️ Reusing existing Kind cluster '$CLUSTER_NAME' (no delete/recreate)"
else
    echo "🚀 Creating Kind cluster '$CLUSTER_NAME' with audit webhook support..."
    if create_primary_cluster; then
        echo "✅ Kind cluster created successfully"
    elif is_known_kind_bootstrap_flake && run_dood_self_heal; then
        echo "✅ Kind cluster self-healed successfully"
    else
        echo "❌ Kind cluster creation failed."
        echo "📄 See log: $KIND_CREATE_LOG_FILE"
        exit 1
    fi
fi

echo "📋 Configuring kubeconfig for cluster '$CLUSTER_NAME'..."
kind export kubeconfig --name "$CLUSTER_NAME"

current_cluster_name="$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')"
current_server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

if [[ "$current_server" =~ ^https://(127\.0\.0\.1|localhost|0\.0\.0\.0):([0-9]+)$ ]]; then
    apiserver_port="${BASH_REMATCH[2]}"
    echo "🔁 Rewriting kubeconfig server endpoint to host.docker.internal:${apiserver_port}..."
    kubectl config set-cluster "$current_cluster_name" \
        --server="https://host.docker.internal:${apiserver_port}" \
        --tls-server-name=localhost >/dev/null
    echo "✅ kubeconfig endpoint updated for devcontainer networking"
else
    echo "ℹ️ kubeconfig server is '$current_server' (no rewrite needed)"
fi

echo "✅ Cluster setup complete!"
