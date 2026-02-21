#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-}"
NAMESPACE="gitops-reverser"
QUICKSTART_NAMESPACE="sut"
HELM_CHART_SOURCE="${HELM_CHART_SOURCE:-charts/gitops-reverser}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-60s}"
QUICKSTART_TIMEOUT_SECONDS="${QUICKSTART_TIMEOUT_SECONDS:-180}"
PROJECT_IMAGE="${PROJECT_IMAGE:-}"

TEST_ID="$(date +%s)-$RANDOM"
REPO_NAME="quickstart-smoke-${MODE}-${TEST_ID}"
CHECKOUT_DIR="/tmp/gitops-reverser/${REPO_NAME}"

GIT_PROVIDER_NAME="quickstart-provider-${TEST_ID}"
GIT_TARGET_NAME="quickstart-target-${TEST_ID}"
WATCHRULE_NAME="quickstart-watchrule-${TEST_ID}"
INVALID_PROVIDER_NAME="quickstart-invalid-provider-${TEST_ID}"
ENCRYPTION_SECRET_NAME="quickstart-sops-age-key-${TEST_ID}"
CONFIGMAP_NAME="quickstart-config-${TEST_ID}"
EXPECTED_CONFIGMAP_FILE="${CHECKOUT_DIR}/live-cluster/v1/configmaps/${QUICKSTART_NAMESPACE}/${CONFIGMAP_NAME}.yaml"
SECRET_NAME="quickstart-secret-${TEST_ID}"
SECRET_VALUE_ONE="quickstart-plaintext-one-${TEST_ID}"
SECRET_VALUE_TWO="quickstart-plaintext-two-${TEST_ID}"
EXPECTED_SECRET_FILE="${CHECKOUT_DIR}/live-cluster/v1/secrets/${QUICKSTART_NAMESPACE}/${SECRET_NAME}.sops.yaml"
GITEA_PORT_FORWARD_PID=""

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
    selector="app.kubernetes.io/name=gitops-reverser"
  fi

  printf '%s' "${selector}"
}

install_helm() {
  local helm_image_args=()
  local helm_crd_args=()

  if [[ -n "${PROJECT_IMAGE}" ]]; then
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

  if kubectl get crd gitproviders.configbutler.ai gittargets.configbutler.ai \
    watchrules.configbutler.ai clusterwatchrules.configbutler.ai >/dev/null 2>&1; then
    helm_crd_args+=(--skip-crds)
    echo "Detected existing CRDs in cluster; installing Helm release with --skip-crds"
  fi

  echo "Installing from Helm chart (mode=helm, source=${HELM_CHART_SOURCE})"
  helm upgrade --install "name-is-cool-but-not-relevant" "${HELM_CHART_SOURCE}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set fullnameOverride=gitops-reverser \
    "${helm_crd_args[@]}" \
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
  echo "Install quickstart smoke diagnostics (${MODE})"
  echo "Namespace: ${NAMESPACE}"
  echo "Quickstart namespace: ${QUICKSTART_NAMESPACE}"
  echo "Pod selector: ${pod_selector}"
  echo
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
  echo "Quickstart resources:"
  kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider,gittarget,watchrule \
    "${GIT_PROVIDER_NAME}" "${GIT_TARGET_NAME}" "${WATCHRULE_NAME}" "${INVALID_PROVIDER_NAME}" 2>/dev/null || true
  echo
  echo "Quickstart resource status dumps:"
  kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${GIT_PROVIDER_NAME}" -o yaml 2>/dev/null || true
  kubectl -n "${QUICKSTART_NAMESPACE}" get gittarget "${GIT_TARGET_NAME}" -o yaml 2>/dev/null || true
  kubectl -n "${QUICKSTART_NAMESPACE}" get watchrule "${WATCHRULE_NAME}" -o yaml 2>/dev/null || true
  kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${INVALID_PROVIDER_NAME}" -o yaml 2>/dev/null || true
  echo
  echo "Recent namespace events (${NAMESPACE}):"
  kubectl -n "${NAMESPACE}" get events --sort-by=.metadata.creationTimestamp | tail -n 50 || true
  echo
  echo "Recent namespace events (${QUICKSTART_NAMESPACE}):"
  kubectl -n "${QUICKSTART_NAMESPACE}" get events --sort-by=.metadata.creationTimestamp | tail -n 50 || true
  echo
  echo "Git checkout state (${CHECKOUT_DIR}):"
  git -C "${CHECKOUT_DIR}" status --short 2>/dev/null || true
  git -C "${CHECKOUT_DIR}" --no-pager log --oneline -n 10 2>/dev/null || true
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

cleanup_gitea_port_forward() {
  if [[ -n "${GITEA_PORT_FORWARD_PID}" ]]; then
    kill "${GITEA_PORT_FORWARD_PID}" >/dev/null 2>&1 || true
    wait "${GITEA_PORT_FORWARD_PID}" >/dev/null 2>&1 || true
    GITEA_PORT_FORWARD_PID=""
  fi
}

ensure_gitea_api_port_forward() {
  cleanup_gitea_port_forward
  pkill -f "kubectl.*port-forward.*13000:13000" >/dev/null 2>&1 || true

  kubectl -n gitea-e2e port-forward svc/gitea-http 13000:13000 >/tmp/gitea-port-forward.log 2>&1 &
  GITEA_PORT_FORWARD_PID="$!"
  trap cleanup_gitea_port_forward EXIT

  for _ in {1..30}; do
    if curl -s -f "http://localhost:13000/api/v1/version" >/dev/null 2>&1; then
      echo "Gitea API port-forward is ready on localhost:13000"
      return 0
    fi
    sleep 2
  done

  echo "Timed out waiting for Gitea API port-forward readiness"
  return 1
}

reset_install_state() {
  echo "Resetting install state for clean first-time install validation"
  kubectl delete clusterrole \
    gitops-reverser-manager-role \
    gitops-reverser-metrics-reader \
    gitops-reverser-proxy-role \
    --ignore-not-found=true >/dev/null || true
  kubectl delete clusterrolebinding \
    gitops-reverser-manager-rolebinding \
    gitops-reverser-proxy-rolebinding \
    --ignore-not-found=true >/dev/null || true
  kubectl delete validatingwebhookconfiguration gitops-reverser-validating-webhook-configuration \
    --ignore-not-found=true >/dev/null || true
  kubectl delete namespace "${NAMESPACE}" --wait=true --ignore-not-found=true >/dev/null || true
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

wait_for_ready() {
  local kind="$1"
  local name="$2"
  local namespace="$3"
  local timeout_seconds="${4}"
  local start_epoch now status reason message
  start_epoch="$(date +%s)"

  while true; do
    status="$(kubectl -n "${namespace}" get "${kind}" "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    reason="$(kubectl -n "${namespace}" get "${kind}" "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"
    message="$(kubectl -n "${namespace}" get "${kind}" "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true)"

    if [[ "${status}" == "True" ]]; then
      echo "${kind}/${name} Ready=True (${reason})"
      return 0
    fi

    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for ${kind}/${name} Ready=True (status=${status} reason=${reason})"
      echo "Last message: ${message}"
      kubectl -n "${namespace}" get "${kind}" "${name}" -o yaml || true
      return 1
    fi
    sleep 2
  done
}

wait_for_connection_failed_actionable_message() {
  local name="$1"
  local timeout_seconds="${2}"
  local start_epoch now status reason message
  start_epoch="$(date +%s)"

  while true; do
    status="$(kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    reason="$(kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"
    message="$(kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${name}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true)"

    if [[ "${status}" == "False" && "${reason}" == "ConnectionFailed" && -n "${message}" ]]; then
      shopt -s nocasematch
      if [[ "${message}" =~ auth|credential|connect|repository|secret ]]; then
        shopt -u nocasematch
        echo "gitprovider/${name} exposes actionable failure message"
        return 0
      fi
      shopt -u nocasematch
    fi

    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for actionable ConnectionFailed message on gitprovider/${name}"
      echo "Last seen status=${status} reason=${reason} message=${message}"
      kubectl -n "${QUICKSTART_NAMESPACE}" get gitprovider "${name}" -o yaml || true
      return 1
    fi
    sleep 2
  done
}

git_commit_count() {
  git -C "${CHECKOUT_DIR}" rev-list --count --all 2>/dev/null || echo 0
}

wait_for_file_exists() {
  local file_path="$1"
  local timeout_seconds="$2"
  local start_epoch now
  start_epoch="$(date +%s)"

  while true; do
    git -C "${CHECKOUT_DIR}" pull --ff-only >/dev/null 2>&1 || true
    if [[ -f "${file_path}" ]]; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for file to exist: ${file_path}"
      return 1
    fi
    sleep 2
  done
}

wait_for_file_contains() {
  local file_path="$1"
  local expected_text="$2"
  local timeout_seconds="$3"
  local start_epoch now
  start_epoch="$(date +%s)"

  while true; do
    git -C "${CHECKOUT_DIR}" pull --ff-only >/dev/null 2>&1 || true
    if [[ -f "${file_path}" ]] && grep -Fq "${expected_text}" "${file_path}"; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for file content: ${file_path} to contain '${expected_text}'"
      return 1
    fi
    sleep 2
  done
}

wait_for_file_absent() {
  local file_path="$1"
  local timeout_seconds="$2"
  local start_epoch now
  start_epoch="$(date +%s)"

  while true; do
    git -C "${CHECKOUT_DIR}" pull --ff-only >/dev/null 2>&1 || true
    if [[ ! -f "${file_path}" ]]; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for file to be deleted: ${file_path}"
      return 1
    fi
    sleep 2
  done
}

wait_for_file_not_contains() {
  local file_path="$1"
  local unexpected_text="$2"
  local timeout_seconds="$3"
  local start_epoch now
  start_epoch="$(date +%s)"

  while true; do
    git -C "${CHECKOUT_DIR}" pull --ff-only >/dev/null 2>&1 || true
    if [[ -f "${file_path}" ]] && ! grep -Fq "${unexpected_text}" "${file_path}"; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - start_epoch >= timeout_seconds )); then
      echo "Timed out waiting for file content: ${file_path} to NOT contain '${unexpected_text}'"
      return 1
    fi
    sleep 2
  done
}

extract_generated_age_key() {
  local secret_name="$1"
  local key_data age_key

  key_data="$(
    kubectl -n "${QUICKSTART_NAMESPACE}" get secret "${secret_name}" \
      -o go-template='{{ range $k, $v := .data }}{{ printf "%s=%s\n" $k $v }}{{ end }}' 2>/dev/null |
      grep '\.agekey=' | head -n1 | cut -d'=' -f2- || true
  )"
  if [[ -z "${key_data}" ]]; then
    echo "No .agekey entry found in secret ${QUICKSTART_NAMESPACE}/${secret_name}"
    return 1
  fi

  age_key="$(printf '%s' "${key_data}" | base64 -d 2>/dev/null | awk '/AGE-SECRET-KEY-/{print; exit}')"
  if [[ -z "${age_key}" ]]; then
    echo "Failed to decode AGE-SECRET-KEY from secret ${QUICKSTART_NAMESPACE}/${secret_name}"
    return 1
  fi

  printf '%s' "${age_key}"
}

decrypt_file_with_controller_sops() {
  local file_path="$1"
  local age_key="$2"
  local pod_selector pod_name

  pod_selector="$(get_controller_pod_selector)"
  pod_name="$(
    kubectl -n "${NAMESPACE}" get pod -l "${pod_selector}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
  )"
  if [[ -z "${pod_name}" ]]; then
    echo "Failed to find controller pod in namespace ${NAMESPACE}"
    return 1
  fi

  kubectl -n "${NAMESPACE}" exec -i "${pod_name}" -- \
    env "SOPS_AGE_KEY=${age_key}" \
    /usr/local/bin/sops --decrypt --input-type yaml --output-type yaml /dev/stdin < "${file_path}"
}

run_quickstart_flow() {
  ensure_gitea_api_port_forward

  echo "Setting up quickstart repo and credentials"
  run_or_debug \
    "Creating dedicated Gitea repo for quickstart smoke (${REPO_NAME})" \
    bash test/e2e/scripts/setup-gitea.sh "${REPO_NAME}" "${CHECKOUT_DIR}"

  echo "Applying minimal GitProvider/GitTarget/WatchRule quickstart resources"
  cat <<EOF | kubectl apply -f -
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: ${GIT_PROVIDER_NAME}
  namespace: ${QUICKSTART_NAMESPACE}
spec:
  url: "http://gitea-http.gitea-e2e.svc.cluster.local:13000/testorg/${REPO_NAME}.git"
  allowedBranches: ["*"]
  secretRef:
    name: git-creds
  push:
    interval: "5s"
    maxCommits: 10
---
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: ${GIT_TARGET_NAME}
  namespace: ${QUICKSTART_NAMESPACE}
spec:
  providerRef:
    name: ${GIT_PROVIDER_NAME}
  branch: main
  path: live-cluster
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        extractFromSecret: true
        generateWhenMissing: true
    secretRef:
      name: ${ENCRYPTION_SECRET_NAME}
---
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: ${WATCHRULE_NAME}
  namespace: ${QUICKSTART_NAMESPACE}
spec:
  targetRef:
    name: ${GIT_TARGET_NAME}
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps", "secrets"]
EOF

  wait_for_ready gitprovider "${GIT_PROVIDER_NAME}" "${QUICKSTART_NAMESPACE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_ready gittarget "${GIT_TARGET_NAME}" "${QUICKSTART_NAMESPACE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_ready watchrule "${WATCHRULE_NAME}" "${QUICKSTART_NAMESPACE}" "${QUICKSTART_TIMEOUT_SECONDS}"

  echo "Validating generated SOPS key secret and backup-warning annotation"
  kubectl -n "${QUICKSTART_NAMESPACE}" get secret "${ENCRYPTION_SECRET_NAME}" >/dev/null
  local warning_anno recipient_anno
  warning_anno="$(kubectl -n "${QUICKSTART_NAMESPACE}" get secret "${ENCRYPTION_SECRET_NAME}" \
    -o jsonpath='{.metadata.annotations.configbutler\.ai/backup-warning}')"
  recipient_anno="$(kubectl -n "${QUICKSTART_NAMESPACE}" get secret "${ENCRYPTION_SECRET_NAME}" \
    -o jsonpath='{.metadata.annotations.configbutler\.ai/age-recipient}')"
  if [[ "${warning_anno}" != "REMOVE_AFTER_BACKUP" ]]; then
    echo "Expected backup warning annotation REMOVE_AFTER_BACKUP, got '${warning_anno}'"
    return 1
  fi
  if [[ "${recipient_anno}" != age1* ]]; then
    echo "Expected age recipient annotation to start with age1, got '${recipient_anno}'"
    return 1
  fi

  echo "Running create/update/delete quickstart functional checks"
  local commits_before_create commits_after_create commits_after_update commits_after_delete
  commits_before_create="$(git_commit_count)"

  kubectl -n "${QUICKSTART_NAMESPACE}" delete configmap "${CONFIGMAP_NAME}" --ignore-not-found=true >/dev/null
  kubectl -n "${QUICKSTART_NAMESPACE}" create configmap "${CONFIGMAP_NAME}" --from-literal=value=one >/dev/null
  wait_for_file_exists "${EXPECTED_CONFIGMAP_FILE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_contains "${EXPECTED_CONFIGMAP_FILE}" "value: one" "${QUICKSTART_TIMEOUT_SECONDS}"
  commits_after_create="$(git_commit_count)"
  if (( commits_after_create <= commits_before_create )); then
    echo "Expected commit count to increase after create (${commits_before_create} -> ${commits_after_create})"
    return 1
  fi

  kubectl -n "${QUICKSTART_NAMESPACE}" patch configmap "${CONFIGMAP_NAME}" --type merge --patch '{"data":{"value":"two"}}' >/dev/null
  wait_for_file_contains "${EXPECTED_CONFIGMAP_FILE}" "value: two" "${QUICKSTART_TIMEOUT_SECONDS}"
  commits_after_update="$(git_commit_count)"
  if (( commits_after_update <= commits_after_create )); then
    echo "Expected commit count to increase after update (${commits_after_create} -> ${commits_after_update})"
    return 1
  fi

  kubectl -n "${QUICKSTART_NAMESPACE}" delete configmap "${CONFIGMAP_NAME}" --ignore-not-found=true >/dev/null
  wait_for_file_absent "${EXPECTED_CONFIGMAP_FILE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  commits_after_delete="$(git_commit_count)"
  if (( commits_after_delete <= commits_after_update )); then
    echo "Expected commit count to increase after delete (${commits_after_update} -> ${commits_after_delete})"
    return 1
  fi

  echo "Running encrypted Secret commit check"
  local commits_before_secret commits_after_secret secret_value_one_b64 secret_value_two_b64
  commits_before_secret="$(git_commit_count)"
  secret_value_one_b64="$(printf '%s' "${SECRET_VALUE_ONE}" | base64 | tr -d '\n')"
  secret_value_two_b64="$(printf '%s' "${SECRET_VALUE_TWO}" | base64 | tr -d '\n')"

  kubectl -n "${QUICKSTART_NAMESPACE}" delete secret "${SECRET_NAME}" --ignore-not-found=true >/dev/null
  kubectl -n "${QUICKSTART_NAMESPACE}" create secret generic "${SECRET_NAME}" \
    --from-literal=password="${SECRET_VALUE_ONE}" >/dev/null
  kubectl -n "${QUICKSTART_NAMESPACE}" patch secret "${SECRET_NAME}" --type merge \
    --patch "{\"stringData\":{\"password\":\"${SECRET_VALUE_TWO}\"}}" >/dev/null

  wait_for_file_exists "${EXPECTED_SECRET_FILE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_contains "${EXPECTED_SECRET_FILE}" "sops:" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_not_contains "${EXPECTED_SECRET_FILE}" "${SECRET_VALUE_ONE}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_not_contains "${EXPECTED_SECRET_FILE}" "${SECRET_VALUE_TWO}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_not_contains "${EXPECTED_SECRET_FILE}" "${secret_value_one_b64}" "${QUICKSTART_TIMEOUT_SECONDS}"
  wait_for_file_not_contains "${EXPECTED_SECRET_FILE}" "${secret_value_two_b64}" "${QUICKSTART_TIMEOUT_SECONDS}"

  echo "Validating encrypted Secret is decryptable with generated key"
  local generated_age_key decrypted_secret
  generated_age_key="$(extract_generated_age_key "${ENCRYPTION_SECRET_NAME}")"
  decrypted_secret="$(decrypt_file_with_controller_sops "${EXPECTED_SECRET_FILE}" "${generated_age_key}")"
  if ! grep -Fq "${secret_value_two_b64}" <<<"${decrypted_secret}"; then
    echo "Expected decrypted secret payload to contain updated value (base64) after patch"
    return 1
  fi

  commits_after_secret="$(git_commit_count)"
  if (( commits_after_secret <= commits_before_secret )); then
    echo "Expected commit count to increase after secret write (${commits_before_secret} -> ${commits_after_secret})"
    return 1
  fi

  kubectl -n "${QUICKSTART_NAMESPACE}" delete secret "${SECRET_NAME}" --ignore-not-found=true >/dev/null

  echo "Running invalid-credentials quickstart UX check"
  cat <<EOF | kubectl apply -f -
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: ${INVALID_PROVIDER_NAME}
  namespace: ${QUICKSTART_NAMESPACE}
spec:
  url: "http://gitea-http.gitea-e2e.svc.cluster.local:13000/testorg/${REPO_NAME}.git"
  allowedBranches: ["*"]
  secretRef:
    name: git-creds-invalid
EOF
  wait_for_connection_failed_actionable_message "${INVALID_PROVIDER_NAME}" "${QUICKSTART_TIMEOUT_SECONDS}"
}

case "${MODE}" in
  helm)
    reset_install_state
    install_helm
    ;;
  manifest)
    reset_install_state
    install_manifest
    ;;
  *)
    echo "unsupported mode: ${MODE} (expected helm or manifest)"
    exit 1
    ;;
esac

verify_installation
run_or_debug "Running quickstart functional smoke checks" run_quickstart_flow
echo "Install quickstart smoke test passed (${MODE})"
