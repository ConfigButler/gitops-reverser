#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-}"
NAMESPACE="gitops-reverser"
HELM_CHART_SOURCE="${HELM_CHART_SOURCE:-charts/gitops-reverser}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-60s}"
PROJECT_IMAGE="${PROJECT_IMAGE:-}"

if [[ -z "${MODE}" ]]; then
  echo "usage: $0 <helm|manifest>"
  exit 1
fi

get_controller_pod_selector() {
  local selector
  selector="$(kubectl -n "${NAMESPACE}" get deployment gitops-reverser \
    -o jsonpath='{range $k,$v := .spec.selector.matchLabels}{$k}={$v},{end}' 2>/dev/null || true)"
  selector="${selector%,}"

  if [[ -z "${selector}" ]]; then
    # Fallback selector used by chart/manifests if deployment query is not available yet.
    selector="app.kubernetes.io/name=gitops-reverser"
  fi

  printf '%s' "${selector}"
}

install_helm() {
  local helm_image_args=()

  if [[ -n "${PROJECT_IMAGE}" ]]; then
    # Helm chart image is repository + tag. For smoke tests, parse PROJECT_IMAGE and override both.
    local image_no_digest image_repo image_tag
    image_no_digest="${PROJECT_IMAGE%%@*}"
    if [[ "${image_no_digest##*/}" == *:* ]]; then
      image_repo="${image_no_digest%:*}"
      image_tag="${image_no_digest##*:}"
    else
      image_repo="${image_no_digest}"
      image_tag="latest"
    fi
    helm_image_args+=(--set "image.repository=${image_repo}" --set "image.tag=${image_tag}")
    echo "Overriding chart image from PROJECT_IMAGE=${PROJECT_IMAGE}"
  fi

  echo "Installing from Helm chart (mode=helm, source=${HELM_CHART_SOURCE})"
  helm upgrade --install "name-is-cool-but-not-relevant" "${HELM_CHART_SOURCE}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set fullnameOverride=gitops-reverser \
    "${helm_image_args[@]}"
}

install_manifest() {
  echo "Installing from generated dist/install.yaml (mode=manifest)"
  kubectl apply -f dist/install.yaml

  if [[ -n "${PROJECT_IMAGE}" ]]; then
    echo "Overriding manifest deployment image from PROJECT_IMAGE=${PROJECT_IMAGE}"
    kubectl -n "${NAMESPACE}" set image deployment/gitops-reverser manager="${PROJECT_IMAGE}"
  fi
}

print_debug_info() {
  local pod_selector
  pod_selector="$(get_controller_pod_selector)"

  echo
  echo "Install smoke test diagnostics (${MODE})"
  echo "Namespace: ${NAMESPACE}"
  echo "Pod selector: ${pod_selector}"
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
  kubectl -n "${NAMESPACE}" describe pod -l "${pod_selector}" || true
  echo
  echo "Controller-manager logs (last 200 lines):"
  kubectl -n "${NAMESPACE}" logs -l "${pod_selector}" --tail=200 --all-containers=true || true
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
  local pod_selector
  pod_selector="$(get_controller_pod_selector)"

  run_or_debug \
    "Waiting for deployment rollout (timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" rollout status deployment/gitops-reverser --timeout="${WAIT_TIMEOUT}"

  run_or_debug \
    "Checking deployment availability (timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" wait --for=condition=available deployment/gitops-reverser --timeout="${WAIT_TIMEOUT}"

  run_or_debug \
    "Checking pod readiness (selector=${pod_selector}, timeout=${WAIT_TIMEOUT})" \
    kubectl -n "${NAMESPACE}" wait --for=condition=ready pod -l "${pod_selector}" --timeout="${WAIT_TIMEOUT}"

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
