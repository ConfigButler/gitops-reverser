#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-}"
NAMESPACE="${INSTALL_SMOKE_NAMESPACE:-gitops-reverser}"
RELEASE_NAME="${INSTALL_SMOKE_RELEASE:-gitops-reverser}"
DEPLOYMENT_NAME="${INSTALL_SMOKE_DEPLOYMENT:-gitops-reverser}"

if [[ -z "${MODE}" ]]; then
  echo "usage: $0 <helm|manifest>"
  exit 1
fi

cleanup_install() {
  helm uninstall "${RELEASE_NAME}" --namespace "${NAMESPACE}" >/dev/null 2>&1 || true
  kubectl delete namespace "${NAMESPACE}" --wait=true --ignore-not-found=true >/dev/null 2>&1 || true
}

configure_project_image() {
  if [[ -z "${PROJECT_IMAGE:-}" ]]; then
    return
  fi

  if [[ "${PROJECT_IMAGE}" != *":"* ]]; then
    echo "Skipping image override for PROJECT_IMAGE=${PROJECT_IMAGE} (no tag present)"
    return
  fi

  local image_repo image_tag
  image_repo="${PROJECT_IMAGE%:*}"
  image_tag="${PROJECT_IMAGE##*:}"

  helm upgrade --install "${RELEASE_NAME}" charts/gitops-reverser \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set "image.repository=${image_repo}" \
    --set "image.tag=${image_tag}"
}

install_helm() {
  echo "Installing from Helm chart (mode=helm)"
  cleanup_install

  if [[ -n "${PROJECT_IMAGE:-}" && "${PROJECT_IMAGE}" == *":"* ]]; then
    configure_project_image
    return
  fi

  helm upgrade --install "${RELEASE_NAME}" charts/gitops-reverser \
    --namespace "${NAMESPACE}" \
    --create-namespace
}

install_manifest() {
  echo "Installing from generated dist/install.yaml (mode=manifest)"
  cleanup_install

  if [[ ! -f dist/install.yaml ]]; then
    echo "dist/install.yaml not found; generating locally"
    make build-installer
  fi

  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f dist/install.yaml

  if [[ -n "${PROJECT_IMAGE:-}" ]]; then
    kubectl -n "${NAMESPACE}" set image deployment/"${DEPLOYMENT_NAME}" manager="${PROJECT_IMAGE}"
  fi
}

verify_installation() {
  echo "Waiting for deployment rollout"
  kubectl -n "${NAMESPACE}" rollout status deployment/"${DEPLOYMENT_NAME}" --timeout=180s

  echo "Checking deployment availability"
  kubectl -n "${NAMESPACE}" wait --for=condition=available deployment/"${DEPLOYMENT_NAME}" --timeout=120s

  echo "Checking pod readiness"
  kubectl -n "${NAMESPACE}" wait --for=condition=ready pod -l control-plane=controller-manager --timeout=120s

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
