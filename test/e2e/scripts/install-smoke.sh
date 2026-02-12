#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-}"
NAMESPACE="gitops-reverser"
HELM_CHART_SOURCE="${HELM_CHART_SOURCE:-charts/gitops-reverser}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-60s}"

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

print_debug_info() {
  echo
  echo "Install smoke test diagnostics (${MODE})"
  echo "Namespace: ${NAMESPACE}"
  echo "Deployment status:"
  kubectl -n "${NAMESPACE}" get deployment gitops-reverser -o wide || true
  echo
  echo "Deployment describe:"
  kubectl -n "${NAMESPACE}" describe deployment gitops-reverser || true
  echo
  echo "Pods:"
  kubectl -n "${NAMESPACE}" get pods -o wide || true
  echo
  echo "Controller-manager pod describe:"
  kubectl -n "${NAMESPACE}" describe pod -l control-plane=controller-manager || true
  echo
  echo "Controller-manager logs (last 200 lines):"
  kubectl -n "${NAMESPACE}" logs -l control-plane=controller-manager --tail=200 --all-containers=true || true
  echo
  echo "Recent namespace events:"
  kubectl -n "${NAMESPACE}" get events --sort-by=.metadata.creationTimestamp | tail -n 50 || true
}

run_or_debug() {
  local description="$1"
  shift
  echo "${description}"
  if ! "$@"; then
    echo "FAILED: ${description}" >&2
    print_debug_info
    return 1
  fi
}

verify_installation() {
  run_or_debug \
    "Waiting for deployment rollout (timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" rollout status deployment/gitops-reverser --timeout="${WAIT_TIMEOUT}"

  run_or_debug \
    "Checking deployment availability (timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" wait --for=condition=available deployment/gitops-reverser --timeout="${WAIT_TIMEOUT}"

  run_or_debug \
    "Checking pod readiness (timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" wait --for=condition=ready pod -l control-plane=controller-manager --timeout="${WAIT_TIMEOUT}"

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
