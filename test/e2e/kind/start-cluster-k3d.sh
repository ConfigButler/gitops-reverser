#!/bin/bash
# Script to create k3d cluster with audit webhook support for Docker-outside-Docker devcontainers
# Keeps the "HOST_PROJECT_PATH must be mountable by the Docker daemon" logic from the kind version.
# Lets k3d pick the API port. Rewrites kubeconfig to host.docker.internal:<picked-port> when available.

set -euo pipefail

CLUSTER_NAME="${K3D_CLUSTER:-gitops-reverser-test-e2e}"
AUDIT_DIR_REL="test/e2e/kind/audit"
K3D_CREATE_LOG_FILE="${TMPDIR:-/tmp}/k3d-create-${CLUSTER_NAME}.log"
REPO_PWD="$(pwd -P)"

# These filenames match your previous kind cluster-template.yaml
AUDIT_POLICY_FILE="policy.yaml"
AUDIT_WEBHOOK_CONFIG_FILE="webhook-config.yaml"

docker_can_mount_repo() {
    local candidate="$1"
    docker run --rm -v "${candidate}:/hostproj:ro" busybox:1.36.1 sh -c \
        "test -f /hostproj/${AUDIT_DIR_REL}/${AUDIT_POLICY_FILE} && test -f /hostproj/${AUDIT_DIR_REL}/${AUDIT_WEBHOOK_CONFIG_FILE}" \
        >/dev/null 2>&1
}

resolve_host_project_path() {
    local candidates=()
    candidates+=("${REPO_PWD}")
    if [ -n "${HOST_PROJECT_PATH:-}" ]; then
        candidates+=("${HOST_PROJECT_PATH}")
    fi

    local candidate
    for candidate in "${candidates[@]}"; do
        if docker_can_mount_repo "$candidate"; then
            echo "$candidate"
            return 0
        fi
    done

    echo "❌ ERROR: Unable to determine a usable HOST_PROJECT_PATH for k3d volume mounts." >&2
    echo "Tried:" >&2
    if [ -n "${HOST_PROJECT_PATH:-}" ]; then
        echo "  - HOST_PROJECT_PATH=${HOST_PROJECT_PATH}" >&2
    else
        echo "  - HOST_PROJECT_PATH=<unset>" >&2
    fi
    echo "  - pwd=${REPO_PWD}" >&2
    echo "" >&2
    echo "Fix: set HOST_PROJECT_PATH to the path visible to the Docker daemon that contains this repo." >&2
    echo "Examples:" >&2
    echo "  - Docker-in-Docker (daemon in this container): export HOST_PROJECT_PATH='${REPO_PWD}'" >&2
    echo "  - Docker-outside-of-Docker (daemon on your host): export HOST_PROJECT_PATH='<host absolute path to repo>'" >&2
    return 1
}

cluster_exists() {
    # Use JSON output to avoid formatting differences across versions
    k3d cluster list -o json 2>/dev/null | grep -q "\"name\":\"${CLUSTER_NAME}\""
}

ensure_k3d_stat_compat_path() {
    local host_project_path="$1"

    if [ "${host_project_path}" = "${REPO_PWD}" ]; then
        return 0
    fi

    # k3d validates -v SOURCE with a local stat() before creating containers.
    # In Docker-outside-of-Docker setups, HOST_PROJECT_PATH can exist on the host
    # daemon but not inside this devcontainer, which causes a noisy warning.
    # Creating a local symlink keeps k3d's preflight happy without changing the
    # real bind source path passed to Docker.
    # Example of the warning that we prevent: WARN[0000] failed to stat file/directory '/home/runner/work/gitops-reverser/gitops-reverser/test/e2e/kind/audit' volume mount '/home/runner/work/gitops-reverser/gitops-reverser/test/e2e/kind/audit:/etc/kubernetes/audit': please make sure it exists
    local parent_dir
    parent_dir="$(dirname "${host_project_path}")"
    mkdir -p "${parent_dir}" 2>/dev/null || {
        echo "⚠️ Could not create parent '${parent_dir}' for HOST_PROJECT_PATH compatibility symlink" >&2
        return 0
    }

    if [ -L "${host_project_path}" ]; then
        ln -sfn "${REPO_PWD}" "${host_project_path}" 2>/dev/null || {
            echo "⚠️ Could not refresh HOST_PROJECT_PATH compatibility symlink '${host_project_path}'" >&2
            return 0
        }
        return 0
    fi

    if [ -e "${host_project_path}" ]; then
        return 0
    fi

    ln -s "${REPO_PWD}" "${host_project_path}" 2>/dev/null || {
        echo "⚠️ Could not create HOST_PROJECT_PATH compatibility symlink '${host_project_path}'" >&2
        return 0
    }
}

create_cluster() {
    local host_project_path="$1"
    local audit_host_dir="${host_project_path}/${AUDIT_DIR_REL}"

    echo "🚀 Creating k3d cluster '${CLUSTER_NAME}' with audit webhook support..."
    echo "🔧 Using HOST_PROJECT_PATH: ${host_project_path}"
    echo "🔧 Mounting audit dir: ${audit_host_dir} -> /etc/kubernetes/audit"

    # NOTE: no --api-port: let k3d pick an available port.
    #
    # Mount audit dir into the server container and pass kube-apiserver audit flags
    # via k3s "--kube-apiserver-arg=..." options.
    k3d cluster create "${CLUSTER_NAME}" \
      --kubeconfig-update-default \
      --kubeconfig-switch-context \
      -v "${audit_host_dir}:/etc/kubernetes/audit@server:0" \
      --k3s-arg "--kube-apiserver-arg=audit-policy-file=/etc/kubernetes/audit/${AUDIT_POLICY_FILE}@server:0" \
      --k3s-arg "--kube-apiserver-arg=audit-webhook-config-file=/etc/kubernetes/audit/${AUDIT_WEBHOOK_CONFIG_FILE}@server:0" \
      --k3s-arg "--kube-apiserver-arg=audit-webhook-batch-max-wait=1s@server:0" \
      --k3s-arg "--kube-apiserver-arg=audit-webhook-batch-max-size=10@server:0" \
      2>&1 | tee "${K3D_CREATE_LOG_FILE}"
}

merge_kubeconfig() {
    # Ensure kubeconfig is present and current-context points at this cluster
    k3d kubeconfig merge "${CLUSTER_NAME}" \
      --kubeconfig-switch-context \
      --kubeconfig-merge-default >/dev/null
}

rewrite_kubeconfig_for_devcontainer() {
    local cluster_entry server host port

    cluster_entry="$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')"
    server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"

    # Parse https://HOST:PORT
    if [[ "$server" =~ ^https://([^:/]+):([0-9]+)$ ]]; then
        host="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[2]}"
    else
        echo "ℹ️ kubeconfig server is '${server}' (couldn't parse host/port; no rewrite)"
        return 0
    fi

    # In devcontainers we want to talk to the docker host.
    if getent hosts host.docker.internal >/dev/null 2>&1; then
        # Only rewrite if it's something that won't work inside the container
        if [[ "$host" == "0.0.0.0" || "$host" == "127.0.0.1" || "$host" == "localhost" ]]; then
            echo "🔁 Rewriting kubeconfig server endpoint to host.docker.internal:${port}..."
            kubectl config set-cluster "$cluster_entry" \
                --server="https://host.docker.internal:${port}" \
                --tls-server-name=localhost >/dev/null
            echo "✅ kubeconfig endpoint updated for devcontainer networking"
        else
            echo "ℹ️ kubeconfig server host is '${host}' (no rewrite needed)"
        fi
    else
        echo "ℹ️ host.docker.internal not resolvable; keeping server as ${server}"
    fi
}

main() {
    if cluster_exists; then
        echo "♻️ Reusing existing k3d cluster '${CLUSTER_NAME}' (no delete/recreate)"
    else
        export HOST_PROJECT_PATH
        HOST_PROJECT_PATH="$(resolve_host_project_path)"
        ensure_k3d_stat_compat_path "${HOST_PROJECT_PATH}"
        create_cluster "${HOST_PROJECT_PATH}"
    fi

    echo "📋 Configuring kubeconfig for cluster '${CLUSTER_NAME}'..."
    merge_kubeconfig
    rewrite_kubeconfig_for_devcontainer

    echo "✅ Cluster setup complete!"
    echo "Try:"
    echo "  kubectl get nodes"
}

main "$@"
