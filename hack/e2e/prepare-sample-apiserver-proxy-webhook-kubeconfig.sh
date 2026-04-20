#!/usr/bin/env bash
set -euo pipefail

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"

PROXY_NAMESPACE="${PROXY_NAMESPACE:-wardle}"
PROXY_WEBHOOK_SECRET_NAME="${PROXY_WEBHOOK_SECRET_NAME:-audit-pass-through-webhook-kubeconfig}"
AUDIT_CLUSTER_ID="${AUDIT_CLUSTER_ID:-kind-e2e}"
TMP_KUBECONFIG="$(mktemp "${TMPDIR:-/tmp}/audit-pass-through-proxy-kubeconfig.XXXXXX")"

cleanup() {
	rm -f "${TMP_KUBECONFIG}"
}
trap cleanup EXIT

AUDIT_WEBHOOK_SERVER_URL="https://gitops-reverser-audit.${NAMESPACE}.svc.cluster.local:9444/audit-webhook/${AUDIT_CLUSTER_ID}" \
AUDIT_TLS_SERVER_NAME="gitops-reverser-audit.${NAMESPACE}.svc" \
CTX="${CTX}" \
NAMESPACE="${NAMESPACE}" \
	bash hack/generate-audit-webhook-kubeconfig.sh > "${TMP_KUBECONFIG}"

kubectl --context "${CTX}" -n "${PROXY_NAMESPACE}" \
	create secret generic "${PROXY_WEBHOOK_SECRET_NAME}" \
	--from-file=kubeconfig="${TMP_KUBECONFIG}" \
	--dry-run=client -o yaml \
	| kubectl --context "${CTX}" apply -f -
