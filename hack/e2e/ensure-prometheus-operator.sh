#!/bin/bash
set -euo pipefail

PROM_OPERATOR_VERSION="${PROM_OPERATOR_VERSION:-0.89.0}"
PROM_OPERATOR_NAMESPACE="${PROM_OPERATOR_NAMESPACE:-prometheus-operator}"
KUBE_CONTEXT="${KUBECONTEXT:-${CTX:-}}"

if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
fi

if [[ -z "${KUBE_CONTEXT}" ]]; then
  echo "❌ Kubernetes context is required (set KUBECONTEXT or CTX)"
  exit 1
fi

is_fully_installed() {
  kubectl --context "${KUBE_CONTEXT}" get deployment prometheus-operator -n "${PROM_OPERATOR_NAMESPACE}" >/dev/null 2>&1 &&
    kubectl --context "${KUBE_CONTEXT}" get crd prometheuses.monitoring.coreos.com >/dev/null 2>&1 &&
    kubectl --context "${KUBE_CONTEXT}" get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1
}

if is_fully_installed; then
  echo "✅ Prometheus Operator already installed and configured in ${PROM_OPERATOR_NAMESPACE}"
  exit 0
fi

echo "Installing Prometheus Operator v${PROM_OPERATOR_VERSION} in namespace ${PROM_OPERATOR_NAMESPACE}..."
kubectl --context "${KUBE_CONTEXT}" create namespace "${PROM_OPERATOR_NAMESPACE}" --dry-run=client -o yaml | \
  kubectl --context "${KUBE_CONTEXT}" apply -f -

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

cat > "${tmp_dir}/kustomization.yaml" <<EOF
resources:
  - https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/refs/tags/v${PROM_OPERATOR_VERSION}/bundle.yaml
EOF

(
  cd "${tmp_dir}"
  NAMESPACE="${PROM_OPERATOR_NAMESPACE}" kustomize edit set namespace "${PROM_OPERATOR_NAMESPACE}"
  if ! kubectl --context "${KUBE_CONTEXT}" create -k .; then
    echo "kubectl create -k failed (likely already exists), reconciling with server-side apply ..."
    kubectl --context "${KUBE_CONTEXT}" apply --server-side -k .
  fi
)

kubectl --context "${KUBE_CONTEXT}" rollout status deployment/prometheus-operator -n "${PROM_OPERATOR_NAMESPACE}" \
  --timeout=180s

echo "✅ Prometheus Operator is ready"
