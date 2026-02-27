#!/bin/bash
# Script to create Kind cluster with proper host path substitution for Docker-in-Docker

set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER:-gitops-reverser-test-e2e}"
TEMPLATE_FILE="test/e2e/kind/cluster-template.yaml"
CONFIG_FILE="test/e2e/kind/cluster.ignore.yaml"
KIND_CREATE_LOG_FILE="${TMPDIR:-/tmp}/kind-create-${CLUSTER_NAME}.log"
POD_SUBNET="${KIND_POD_SUBNET:-10.244.0.0/16}"

docker_can_mount_repo() {
    local candidate="$1"
    docker run --rm -v "${candidate}:/hostproj:ro" busybox:1.36.1 sh -c \
        'test -f /hostproj/test/e2e/kind/audit/policy.yaml' >/dev/null 2>&1
}

resolve_host_project_path() {
    local repo_pwd
    repo_pwd="$(pwd -P)"

    local candidates=()
    if [ -n "${HOST_PROJECT_PATH:-}" ]; then
        candidates+=("${HOST_PROJECT_PATH}")
    fi
    candidates+=("${repo_pwd}")

    local candidate
    for candidate in "${candidates[@]}"; do
        if docker_can_mount_repo "$candidate"; then
            echo "$candidate"
            return 0
        fi
    done

    echo "❌ ERROR: Unable to determine a usable HOST_PROJECT_PATH for Kind extraMounts." >&2
    echo "Tried:" >&2
    if [ -n "${HOST_PROJECT_PATH:-}" ]; then
        echo "  - HOST_PROJECT_PATH=${HOST_PROJECT_PATH}" >&2
    else
        echo "  - HOST_PROJECT_PATH=<unset>" >&2
    fi
    echo "  - pwd=${repo_pwd}" >&2
    echo "" >&2
    echo "Fix: set HOST_PROJECT_PATH to the path visible to the Docker daemon that contains this repo." >&2
    echo "Examples:" >&2
    echo "  - Docker-in-Docker (daemon in this container): export HOST_PROJECT_PATH='${repo_pwd}'" >&2
    echo "  - Docker-outside-of-Docker (daemon on your host): export HOST_PROJECT_PATH='<host absolute path to repo>'" >&2
    return 1
}

create_primary_cluster() {
    kind create cluster --name "$CLUSTER_NAME" --config "$CONFIG_FILE" --wait 5m --retain 2>&1 | tee "$KIND_CREATE_LOG_FILE"
}

# Known Kind bootstrap race in DOOD setups: https://github.com/kubernetes-sigs/kind/issues/2867
is_known_kind_bootstrap_flake() {
    grep -Eq \
        "failed to apply overlay network|failed to remove control plane taint|failed to remove control plane load balancer label|failed to download openapi|couldn't get current server API group list|The connection to the server .*:6443 was refused" \
        "$KIND_CREATE_LOG_FILE"
}

create_cluster_with_retries() {
    local retries="${KIND_CREATE_RETRIES:-2}"
    local attempt=0

    while true; do
        attempt=$((attempt + 1))
        if create_primary_cluster; then
            return 0
        fi

        if is_known_kind_bootstrap_flake && [ "$attempt" -le "$retries" ]; then
            echo "⚠️ Kind bootstrap flake detected; retrying create (attempt ${attempt}/${retries})"
            kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
            sleep 2
            continue
        fi

        return 1
    done
}

run_dood_self_heal() {
    local control_plane_container="${CLUSTER_NAME}-control-plane"
    local container_found=false
    for _ in $(seq 1 30); do
        if docker inspect "$control_plane_container" >/dev/null 2>&1; then
            container_found=true
            break
        fi
        sleep 1
    done
    [ "$container_found" = "true" ] || return 1

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
    export HOST_PROJECT_PATH
    HOST_PROJECT_PATH="$(resolve_host_project_path)"

    echo "🔧 Using HOST_PROJECT_PATH: $HOST_PROJECT_PATH"
    echo "📝 Generating Kind cluster configuration from template..."
    envsubst < "$TEMPLATE_FILE" > "$CONFIG_FILE"
    echo "✅ Generated configuration:"
    cat "$CONFIG_FILE"
    echo ""

    echo "🚀 Creating Kind cluster '$CLUSTER_NAME' with audit webhook support..."
    if create_cluster_with_retries; then
        echo "✅ Kind cluster created successfully"
    else
        if is_known_kind_bootstrap_flake; then
            echo "⚠️ Kind bootstrap flake detected; attempting self-heal"
        else
            echo "⚠️ Kind cluster creation failed; attempting self-heal"
        fi

        if run_dood_self_heal; then
            echo "✅ Kind cluster self-healed successfully"
        else
            echo "❌ Kind cluster creation failed."
            echo "📄 See log: $KIND_CREATE_LOG_FILE"
            exit 1
        fi
    fi
fi

echo "📋 Configuring kubeconfig for cluster '$CLUSTER_NAME'..."
kind export kubeconfig --name "$CLUSTER_NAME"

current_cluster_name="$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')"
current_server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

if [[ "$current_server" =~ ^https://(127\.0\.0\.1|localhost|0\.0\.0\.0):([0-9]+)$ ]]; then
    apiserver_port="${BASH_REMATCH[2]}"
    if getent hosts host.docker.internal >/dev/null 2>&1; then
        echo "🔁 Rewriting kubeconfig server endpoint to host.docker.internal:${apiserver_port}..."
        kubectl config set-cluster "$current_cluster_name" \
            --server="https://host.docker.internal:${apiserver_port}" \
            --tls-server-name=localhost >/dev/null
        echo "✅ kubeconfig endpoint updated for devcontainer networking"
    else
        echo "ℹ️ host.docker.internal not resolvable; keeping server as ${current_server} (--network host or native environment)"
    fi
else
    echo "ℹ️ kubeconfig server is '$current_server' (no rewrite needed)"
fi

echo "✅ Cluster setup complete!"
