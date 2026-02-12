#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-}"
NAMESPACE="gitops-reverser"
HELM_CHART_SOURCE="${HELM_CHART_SOURCE:-charts/gitops-reverser}"

if [[ -z "${MODE}" ]]; then
  echo "usage: $0 <helm|manifest>"
  exit 1
fi

install_helm() {
  echo "Installing from Helm chart (mode=helm, source=${HELM_CHART_SOURCE})"
  helm upgrade --install "name-is-cool-but-not-relevant" "${HELM_CHART_SOURCE}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set fullnameOverride=gitops-reverser
}

install_manifest() {
  echo "Installing from generated dist/install.yaml (mode=manifest)"
  kubectl apply -f dist/install.yaml
}

verify_installation() {
  echo "Waiting for deployment rollout"
  kubectl -n "${NAMESPACE}" rollout status deployment/gitops-reverser --timeout=30s

  echo "Checking deployment availability"
  kubectl -n "${NAMESPACE}" wait --for=condition=available deployment/gitops-reverser --timeout=30s

  echo "Checking pod readiness"
  kubectl -n "${NAMESPACE}" wait --for=condition=ready pod -l control-plane=controller-manager --timeout=30s

  echo "Checking CRDs"
  kubectl get crd \
    gitproviders.configbutler.ai \
    gittargets.configbutler.ai \
    watchrules.configbutler.ai \
    clusterwatchrules.configbutler.ai >/dev/null

  echo "Checking validating webhook configuration"
  kubectl get validatingwebhookconfiguration gitops-reverser-validating-webhook-configuration >/dev/null
}

case "${MODE}" in
  helm)
    install_helm
    ;;
  manifest)
    install_manifest
    ;;
  *)
    echo "unsupported mode: ${MODE} (expected helm or manifest)"
    exit 1
    ;;
esac

verify_installation
echo "Install smoke test passed (${MODE})"
