#!/bin/bash
# Generates the final audit webhook kubeconfig from the audit root CA and
# kube-apiserver client certificate, then restarts the k3d server node so
# kube-apiserver picks it up.
#
# Must run after the operator is deployed and cert-manager has issued the audit TLS secrets.
# Updates the mounted audit webhook kubeconfig in place after certificate issuance.

set -euo pipefail

CTX="${CTX:-k3d-gitops-reverser-test-e2e}"
CLUSTER_NAME="${CLUSTER_NAME:-gitops-reverser-test-e2e}"
NAMESPACE="${NAMESPACE:-gitops-reverser}"
WEBHOOK_CONFIG="${WEBHOOK_CONFIG:-test/e2e/cluster/audit/webhook-config.yaml}"
SERVER_CONTAINER="k3d-${CLUSTER_NAME}-server-0"
TMP_WEBHOOK_CONFIG="$(mktemp "${TMPDIR:-/tmp}/audit-webhook-config.XXXXXX")"

cleanup() {
    rm -f "${TMP_WEBHOOK_CONFIG}"
}
trap cleanup EXIT

echo "🔐 Writing webhook config with stable root CA trust and apiserver client cert..."
bash hack/generate-audit-webhook-kubeconfig.sh > "${TMP_WEBHOOK_CONFIG}"
mkdir -p "$(dirname "${WEBHOOK_CONFIG}")"
mv "${TMP_WEBHOOK_CONFIG}" "${WEBHOOK_CONFIG}"

echo "🔄 Restarting k3d server node '${SERVER_CONTAINER}' to reload webhook config..."
docker restart "${SERVER_CONTAINER}"

echo "⏳ Waiting for cluster API to come back..."
for i in $(seq 1 40); do
    if kubectl --context "${CTX}" --request-timeout=5s get ns >/dev/null 2>&1; then
        echo "✅ Cluster healthy — webhook TLS injection complete"
        exit 0
    fi
    sleep 3
done

echo "❌ Cluster did not recover after node restart" >&2
exit 1
