#!/bin/bash
# Generates the final audit webhook kubeconfig from the audit root CA and
# kube-apiserver client certificate, then restarts the k3d server node so
# kube-apiserver picks it up.
#
# Must run after the operator is deployed and cert-manager has issued the audit TLS secrets.
# Updates the mounted audit webhook kubeconfig in place after certificate issuance.
#
# The server-node restart is destructive to readiness: it kills the manager pod
# (single-node k3d) AND re-arms the kube-apiserver audit webhook. Until the manager
# is back and the audit connection is live, audit events created by the very first
# specs are dropped — the "first events don't enter the system" race. So after the
# API recovers this script waits for the audit certificates, waits for the manager
# to roll back out, and then drives a real audited write until the manager logs that
# it is receiving audit requests, proving the API server -> manager path is warm.

set -euo pipefail

CTX="${CTX:-k3d-gitops-reverser-test-e2e}"
CLUSTER_NAME="${CLUSTER_NAME:-gitops-reverser-test-e2e}"
NAMESPACE="${NAMESPACE:-gitops-reverser}"
WEBHOOK_CONFIG="${WEBHOOK_CONFIG:-test/e2e/cluster/audit/webhook-config.yaml}"
CONTROLLER_DEPLOY_SELECTOR="${CONTROLLER_DEPLOY_SELECTOR:-app.kubernetes.io/part-of=gitops-reverser}"
CERT_READY_TIMEOUT="${CERT_READY_TIMEOUT:-120s}"
MANAGER_ROLLOUT_TIMEOUT="${MANAGER_ROLLOUT_TIMEOUT:-180s}"
AUDIT_WARMUP_TIMEOUT="${AUDIT_WARMUP_TIMEOUT:-150}"
SERVER_CONTAINER="k3d-${CLUSTER_NAME}-server-0"
WARMUP_NS="gitops-reverser-audit-warmup"
TMP_WEBHOOK_CONFIG="$(mktemp "${TMPDIR:-/tmp}/audit-webhook-config.XXXXXX")"

cleanup() {
    rm -f "${TMP_WEBHOOK_CONFIG}"
    kubectl --context "${CTX}" delete namespace "${WARMUP_NS}" \
        --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

# wait_for_audit_certificates blocks until cert-manager has the audit serving,
# client, and root CA certificates Ready — the TLS material the audit endpoint and
# the kube-apiserver webhook client both depend on.
wait_for_audit_certificates() {
    echo "⏳ Waiting for audit TLS certificates to be Ready..."
    kubectl --context "${CTX}" -n "${NAMESPACE}" wait --for=condition=Ready \
        --timeout="${CERT_READY_TIMEOUT}" \
        certificate/gitops-reverser-audit-root-ca \
        certificate/gitops-reverser-audit-server-cert \
        certificate/gitops-reverser-audit-client-cert
}

# resolve_manager_deploy discovers the manager Deployment by its part-of label and
# stores it in MANAGER_DEPLOY (e.g. "deployment/gitops-reverser").
MANAGER_DEPLOY=""
resolve_manager_deploy() {
    MANAGER_DEPLOY="$(kubectl --context "${CTX}" -n "${NAMESPACE}" get deploy \
        -l "${CONTROLLER_DEPLOY_SELECTOR}" -o name | head -1)"
    if [ -z "${MANAGER_DEPLOY}" ]; then
        echo "❌ No manager deployment found for selector '${CONTROLLER_DEPLOY_SELECTOR}'" >&2
        return 1
    fi
}

# wait_for_manager_rollout blocks until the manager Deployment the node restart
# killed has rolled a Ready pod back out, so its audit ingress endpoint is up.
wait_for_manager_rollout() {
    echo "⏳ Waiting for the manager (${MANAGER_DEPLOY}) to recover after the node restart..."
    kubectl --context "${CTX}" -n "${NAMESPACE}" rollout status "${MANAGER_DEPLOY}" \
        --timeout="${MANAGER_ROLLOUT_TIMEOUT}"
}

# warmup_audit_path drives a throwaway audited write on every iteration and waits
# until the manager logs that it received an audit request. The kube-apiserver audit
# webhook reconnects to the freshly restarted manager with backoff, so the first
# events can still be dropped; this loop proves the path is live before tests run.
# Logs are read from the deployment's current pod (not a label selector) so a stale
# terminating pod from before the restart cannot produce a false-positive match.
warmup_audit_path() {
    echo "⏳ Warming up the audit pipeline (kube-apiserver -> manager)..."
    kubectl --context "${CTX}" create namespace "${WARMUP_NS}" >/dev/null 2>&1 || true
    kubectl --context "${CTX}" -n "${WARMUP_NS}" create configmap audit-warmup \
        --from-literal=ping="initial" >/dev/null 2>&1 || true

    local deadline=$(( SECONDS + AUDIT_WARMUP_TIMEOUT ))
    while [ "${SECONDS}" -lt "${deadline}" ]; do
        # A fresh audited update each iteration keeps the webhook retrying.
        kubectl --context "${CTX}" -n "${WARMUP_NS}" annotate configmap audit-warmup \
            "gitops-reverser.io/ping=$(date +%s%N)" --overwrite >/dev/null 2>&1 || true
        if kubectl --context "${CTX}" -n "${NAMESPACE}" logs "${MANAGER_DEPLOY}" \
            --tail=-1 2>/dev/null | grep -q "Received first audit request"; then
            echo "✅ Audit pipeline live — manager is receiving audit requests"
            return 0
        fi
        sleep 3
    done
    echo "❌ Audit pipeline did not warm up: manager never logged an audit request" >&2
    return 1
}

echo "🔐 Writing webhook config with stable root CA trust and apiserver client cert..."
bash hack/generate-audit-webhook-kubeconfig.sh > "${TMP_WEBHOOK_CONFIG}"
mkdir -p "$(dirname "${WEBHOOK_CONFIG}")"
mv "${TMP_WEBHOOK_CONFIG}" "${WEBHOOK_CONFIG}"

echo "🔄 Restarting k3d server node '${SERVER_CONTAINER}' to reload webhook config..."
docker restart "${SERVER_CONTAINER}"

echo "⏳ Waiting for cluster API to come back..."
api_back=false
for _ in $(seq 1 40); do
    if kubectl --context "${CTX}" --request-timeout=5s get ns >/dev/null 2>&1; then
        echo "✅ Cluster API healthy"
        api_back=true
        break
    fi
    sleep 3
done

if [ "${api_back}" != "true" ]; then
    echo "❌ Cluster did not recover after node restart" >&2
    exit 1
fi

resolve_manager_deploy
wait_for_audit_certificates
wait_for_manager_rollout
warmup_audit_path

echo "✅ Webhook TLS injection complete — audit pipeline verified live"
