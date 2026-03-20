#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: checkout-gitea-repo.sh <repo-name> [checkout-dir]

Checks out a Gitea repository using the Kubernetes Secret convention:
  git-creds-<repo-name>

Defaults:
  GITEA_NAMESPACE=gitea-e2e
  GITEA_SERVICE=gitea-http
  GITEA_LOCAL_PORT=13000
  GITEA_ORG_NAME=testorg
  GITEA_SECRET_PREFIX=git-creds-

Optional environment variables:
  KUBECTL_CONTEXT         kubeconfig context to use
  SECRET_NAMESPACE        namespace that contains the git credential Secret
  GIT_AUTHOR_NAME         git author name to set locally when absent
  GIT_AUTHOR_EMAIL        git author email to set locally when absent
EOF
}

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || {
    echo "ERROR: required command not found: ${cmd}" >&2
    exit 1
  }
}

for cmd in kubectl curl git base64 nohup awk grep; do
  require_cmd "${cmd}"
done

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

REPO_NAME="${1:-}"
CHECKOUT_DIR_INPUT="${2:-}"

if [[ -z "${REPO_NAME}" ]]; then
  usage >&2
  exit 2
fi

GITEA_NAMESPACE="${GITEA_NAMESPACE:-gitea-e2e}"
GITEA_SERVICE="${GITEA_SERVICE:-gitea-http}"
GITEA_LOCAL_PORT="${GITEA_LOCAL_PORT:-13000}"
GITEA_ORG_NAME="${GITEA_ORG_NAME:-testorg}"
GITEA_SECRET_PREFIX="${GITEA_SECRET_PREFIX:-git-creds-}"
SECRET_NAMESPACE="${SECRET_NAMESPACE:-}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-}"
GIT_AUTHOR_NAME="${GIT_AUTHOR_NAME:-Gitea Repo User}"
GIT_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-gitea-repo-user@example.local}"

SECRET_NAME="${GITEA_SECRET_PREFIX}${REPO_NAME}"
CHECKOUT_DIR="${CHECKOUT_DIR_INPUT:-${PWD}/${REPO_NAME}}"
PORT_FORWARD_PID_FILE="${TMPDIR:-/tmp}/gitea-port-forward-${GITEA_NAMESPACE}-${GITEA_SERVICE}-${GITEA_LOCAL_PORT}.pid"
PORT_FORWARD_LOG_FILE="${TMPDIR:-/tmp}/gitea-port-forward-${GITEA_NAMESPACE}-${GITEA_SERVICE}-${GITEA_LOCAL_PORT}.log"

kubectl_cmd() {
  if [[ -n "${KUBECTL_CONTEXT}" ]]; then
    kubectl --context "${KUBECTL_CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

gitea_api_url() {
  printf 'http://127.0.0.1:%s/api/v1' "${GITEA_LOCAL_PORT}"
}

repo_url() {
  printf 'http://127.0.0.1:%s/%s/%s.git' "${GITEA_LOCAL_PORT}" "${GITEA_ORG_NAME}" "${REPO_NAME}"
}

repo_url_with_auth() {
  printf 'http://%s:%s@127.0.0.1:%s/%s/%s.git' \
    "${SECRET_USERNAME}" \
    "${SECRET_PASSWORD}" \
    "${GITEA_LOCAL_PORT}" \
    "${GITEA_ORG_NAME}" \
    "${REPO_NAME}"
}

api_ready() {
  curl -fsS "$(gitea_api_url)/version" >/dev/null 2>&1
}

discover_service_port() {
  local service_port

  service_port="$(
    kubectl_cmd -n "${GITEA_NAMESPACE}" get svc "${GITEA_SERVICE}" \
      -o go-template='{{range .spec.ports}}{{if eq .name "http"}}{{.port}}{{"\n"}}{{end}}{{end}}' 2>/dev/null \
      | head -n1
  )"

  if [[ -z "${service_port}" ]]; then
    service_port="$(
      kubectl_cmd -n "${GITEA_NAMESPACE}" get svc "${GITEA_SERVICE}" \
        -o go-template='{{(index .spec.ports 0).port}}' 2>/dev/null
    )"
  fi

  if [[ -z "${service_port}" ]]; then
    echo "ERROR: could not determine service port for ${GITEA_NAMESPACE}/${GITEA_SERVICE}" >&2
    exit 1
  fi

  printf '%s' "${service_port}"
}

ensure_port_forward() {
  local service_port existing_pid

  if api_ready; then
    return 0
  fi

  if [[ -f "${PORT_FORWARD_PID_FILE}" ]]; then
    existing_pid="$(tr -d '\n\r' < "${PORT_FORWARD_PID_FILE}" || true)"
    if [[ -n "${existing_pid}" ]] && kill -0 "${existing_pid}" 2>/dev/null; then
      for _ in {1..15}; do
        if api_ready; then
          return 0
        fi
        sleep 1
      done
    fi
    rm -f "${PORT_FORWARD_PID_FILE}"
  fi

  service_port="$(discover_service_port)"

  echo "Starting persistent port-forward: ${GITEA_NAMESPACE}/${GITEA_SERVICE} ${GITEA_LOCAL_PORT}:${service_port}"
  nohup kubectl_cmd -n "${GITEA_NAMESPACE}" port-forward --address 127.0.0.1 \
    "svc/${GITEA_SERVICE}" "${GITEA_LOCAL_PORT}:${service_port}" \
    >"${PORT_FORWARD_LOG_FILE}" 2>&1 &
  echo $! > "${PORT_FORWARD_PID_FILE}"

  for _ in {1..30}; do
    if api_ready; then
      return 0
    fi
    sleep 1
  done

  echo "ERROR: Gitea API did not become reachable at $(gitea_api_url)" >&2
  echo "Port-forward log: ${PORT_FORWARD_LOG_FILE}" >&2
  exit 1
}

find_secret_namespace() {
  local matches

  if [[ -n "${SECRET_NAMESPACE}" ]]; then
    printf '%s' "${SECRET_NAMESPACE}"
    return 0
  fi

  matches="$(
    kubectl_cmd get secret -A --no-headers 2>/dev/null \
      | awk -v secret_name="${SECRET_NAME}" '$2 == secret_name {print $1}'
  )"

  if [[ -z "${matches}" ]]; then
    echo "ERROR: could not find Secret named ${SECRET_NAME} in any namespace" >&2
    exit 1
  fi

  if [[ "$(printf '%s\n' "${matches}" | sed '/^$/d' | wc -l | tr -d ' ')" != "1" ]]; then
    echo "ERROR: found multiple namespaces for Secret ${SECRET_NAME}; set SECRET_NAMESPACE explicitly" >&2
    printf '%s\n' "${matches}" >&2
    exit 1
  fi

  printf '%s' "${matches}"
}

secret_data() {
  local namespace="$1"
  local key="$2"

  kubectl_cmd -n "${namespace}" get secret "${SECRET_NAME}" \
    -o go-template="{{index .data \"${key}\"}}" 2>/dev/null
}

decode_b64() {
  printf '%s' "$1" | base64 -d
}

ensure_secret_credentials() {
  local namespace="$1"
  local encoded_username encoded_password

  encoded_username="$(secret_data "${namespace}" username)"
  encoded_password="$(secret_data "${namespace}" password)"

  if [[ -z "${encoded_username}" || -z "${encoded_password}" ]]; then
    echo "ERROR: Secret ${namespace}/${SECRET_NAME} does not contain username/password" >&2
    exit 1
  fi

  SECRET_USERNAME="$(decode_b64 "${encoded_username}")"
  SECRET_PASSWORD="$(decode_b64 "${encoded_password}")"

  if [[ -z "${SECRET_USERNAME}" || -z "${SECRET_PASSWORD}" ]]; then
    echo "ERROR: decoded username/password from ${namespace}/${SECRET_NAME} are empty" >&2
    exit 1
  fi
}

setup_repo_credentials() {
  local credential_file="$1"

  git -C "${CHECKOUT_DIR}" config credential.helper "store --file=${credential_file}"
  git -C "${CHECKOUT_DIR}" config credential.useHttpPath true
  git -C "${CHECKOUT_DIR}" remote set-url origin "$(repo_url_with_auth)"
  printf '%s\n' "$(repo_url_with_auth)" > "${credential_file}"
  chmod 0600 "${credential_file}" || true
}

clone_or_update_repo() {
  local credential_file
  credential_file="${CHECKOUT_DIR}/.git/.gitea-credentials"

  mkdir -p "$(dirname "${CHECKOUT_DIR}")"

  if [[ -d "${CHECKOUT_DIR}/.git" ]]; then
    setup_repo_credentials "${credential_file}"
    git -C "${CHECKOUT_DIR}" fetch origin --prune >/dev/null
  else
    if [[ -e "${CHECKOUT_DIR}" ]]; then
      echo "ERROR: checkout path exists but is not a git repository: ${CHECKOUT_DIR}" >&2
      exit 1
    fi

    git clone "$(repo_url_with_auth)" "${CHECKOUT_DIR}" >/dev/null 2>&1
    setup_repo_credentials "${credential_file}"
  fi

  if [[ -z "$(git -C "${CHECKOUT_DIR}" config --get user.name || true)" ]]; then
    git -C "${CHECKOUT_DIR}" config user.name "${GIT_AUTHOR_NAME}"
  fi

  if [[ -z "$(git -C "${CHECKOUT_DIR}" config --get user.email || true)" ]]; then
    git -C "${CHECKOUT_DIR}" config user.email "${GIT_AUTHOR_EMAIL}"
  fi
}

ensure_port_forward
SECRET_NAMESPACE="$(find_secret_namespace)"
ensure_secret_credentials "${SECRET_NAMESPACE}"
clone_or_update_repo

cat <<EOF
Repo ready.
  repo: ${REPO_NAME}
  checkout: ${CHECKOUT_DIR}
  secret: ${SECRET_NAMESPACE}/${SECRET_NAME}
  remote: $(repo_url)
  port-forward pid file: ${PORT_FORWARD_PID_FILE}
  port-forward log: ${PORT_FORWARD_LOG_FILE}
EOF
