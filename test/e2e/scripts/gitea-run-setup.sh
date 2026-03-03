#!/usr/bin/env bash
set -euo pipefail

# Run-scoped Gitea setup for e2e tests.
#
# This script is designed to be invoked by Make targets and writes artifacts under:
#   .stamps/cluster/$(CTX)/$(NAMESPACE)/repo/
#
# It assumes shared cluster prerequisites already exist:
# - Gitea is installed in the cluster
# - a localhost port-forward is running (prepare-e2e runs portforward-ensure)
#
# Inputs (env):
# - CTX (optional): kube context used for kubectl apply; defaults to current-context
# - CS (optional): stamp cluster dir; defaults to .stamps/cluster/${CTX}
# - NAMESPACE (optional): target namespace for secrets; defaults to sut
# - REPO_NAME (required): repo name to create/use as active repo
# - CHECKOUT_DIR (optional): checkout dir; defaults to $(CS)/$(NAMESPACE)/repo/<repo>/checkout
#
# Outputs:
# - $(CS)/$(NAMESPACE)/repo/active-repo.txt
# - $(CS)/$(NAMESPACE)/repo/token.txt
# - $(CS)/$(NAMESPACE)/repo/ssh/id_rsa{,.pub}
# - $(CS)/$(NAMESPACE)/repo/ssh/known_hosts
# - $(CS)/$(NAMESPACE)/repo/secrets.yaml
# - $(CS)/$(NAMESPACE)/repo/secrets.applied
# - $(CS)/$(NAMESPACE)/repo/repo.ready
# - $(CS)/$(NAMESPACE)/repo/<repo>/checkout.path
# - $(CS)/$(NAMESPACE)/repo/<repo>/checkout/.git/HEAD
# - $(CS)/$(NAMESPACE)/repo/checkout.ready

CTX="${CTX:-}"
if [[ -z "${CTX}" ]]; then
  CTX="$(kubectl config current-context 2>/dev/null || true)"
fi
if [[ -z "${CTX}" ]]; then
  echo "ERROR: CTX must be set (or kubectl current-context must be available)" >&2
  exit 2
fi

CS="${CS:-.stamps/cluster/${CTX}}"
NAMESPACE="${NAMESPACE:-sut}"

GITEA_NAMESPACE="${GITEA_NAMESPACE:-gitea-e2e}"
API_URL="${API_URL:-http://localhost:13000/api/v1}"

GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-giteaadmin}"
GITEA_ADMIN_PASS="${GITEA_ADMIN_PASS:-giteapassword123}"
ORG_NAME="${ORG_NAME:-testorg}"

REPO_NAME="${REPO_NAME:-}"
CHECKOUT_DIR="${CHECKOUT_DIR:-}"

SECRET_HTTP_NAME="${E2E_GIT_SECRET_HTTP:-git-creds}"
SECRET_SSH_NAME="${E2E_GIT_SECRET_SSH:-git-creds-ssh}"
SECRET_INVALID_NAME="${E2E_GIT_SECRET_INVALID:-git-creds-invalid}"

if [[ -z "${REPO_NAME}" ]]; then
  echo "ERROR: REPO_NAME must be set" >&2
  exit 2
fi

project_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cs_abs="${project_root}/${CS}"

run_dir="${cs_abs}/${NAMESPACE}/repo"
ssh_dir="${run_dir}/ssh"
repo_dir="${run_dir}/${REPO_NAME}"

active_repo_file="${run_dir}/active-repo.txt"
token_file="${run_dir}/token.txt"
secrets_yaml="${run_dir}/secrets.yaml"
secrets_applied="${run_dir}/secrets.applied"
repo_ready="${run_dir}/repo.ready"
checkout_ready="${run_dir}/checkout.ready"

repo_checkout_path_file="${repo_dir}/checkout.path"

mkdir -p "${ssh_dir}" "${repo_dir}"

if [[ -f "${active_repo_file}" ]]; then
  previous_repo="$(tr -d '\n' < "${active_repo_file}" | tr -d '\r' || true)"
  if [[ -n "${previous_repo}" && "${previous_repo}" != "${REPO_NAME}" ]]; then
    echo "Active repo changed: ${previous_repo} -> ${REPO_NAME}"
    rm -rf "${run_dir:?}/${previous_repo}" "${checkout_ready}" "${repo_ready}" || true
  fi
fi
printf '%s\n' "${REPO_NAME}" > "${active_repo_file}"

wait_for_api() {
  for i in {1..30}; do
    if curl -fsS "${API_URL}/version" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "ERROR: Gitea API not reachable at ${API_URL} (expected port-forward on localhost:13000)" >&2
  exit 1
}

create_token() {
  if [[ -f "${token_file}" ]]; then
    token="$(tr -d '\n' < "${token_file}" | tr -d '\r' || true)"
    if [[ -n "${token}" ]]; then
      echo "Reusing existing token from ${token_file}"
      printf '%s' "${token}"
      return 0
    fi
  fi

  echo "Creating new access token (run-scoped)"
  local token_name tmp code
  token_name="e2e-${NAMESPACE}-${REPO_NAME}-$(date +%s)-${RANDOM}"
  tmp="$(mktemp)"
  code="$(
    curl -sS -o "${tmp}" -w "%{http_code}" \
      -X POST "${API_URL}/users/${GITEA_ADMIN_USER}/tokens" \
      -H "Content-Type: application/json" \
      -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
      -d "{\"name\":\"${token_name}\",\"scopes\":[\"write:repository\",\"read:repository\",\"write:organization\",\"read:organization\"]}" \
      || true
  )"

  if [[ "${code}" != "201" ]]; then
    echo "ERROR: Failed to create access token (HTTP ${code})" >&2
    sed 's/^/  /' "${tmp}" >&2 || true
    rm -f "${tmp}"
    exit 1
  fi

  token="$(
    python3 - <<'PY' "${tmp}"
import json,sys
path=sys.argv[1]
with open(path,"r",encoding="utf-8") as f:
    obj=json.load(f)
print(obj.get("sha1",""))
PY
  )"
  rm -f "${tmp}"

  if [[ -z "${token}" ]]; then
    echo "ERROR: Token creation succeeded but response did not contain sha1" >&2
    exit 1
  fi

  printf '%s' "${token}" > "${token_file}"
  chmod 0600 "${token_file}" || true
  printf '%s' "${token}"
}

ensure_ssh_keys() {
  local priv pub
  priv="${ssh_dir}/id_rsa"
  pub="${ssh_dir}/id_rsa.pub"

  if [[ -f "${priv}" && -f "${pub}" ]]; then
    return 0
  fi

  rm -f "${priv}" "${pub}"
  # RSA 4096 to satisfy Gitea's minimum key size requirements.
  ssh-keygen -t rsa -b 4096 -f "${priv}" -N "" -C "e2e-test@gitops-reverser" >/dev/null 2>&1
  chmod 0600 "${priv}" || true
  chmod 0644 "${pub}" || true
}

configure_ssh_key_in_gitea() {
  local pub_key_content tmp keys_json key_ids
  pub_key_content="$(cat "${ssh_dir}/id_rsa.pub")"

  echo "Resetting Gitea user SSH keys"
  keys_json="$(
    curl -fsS -X GET "${API_URL}/user/keys" \
      -H "Content-Type: application/json" \
      -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
      || echo "[]"
  )"
  key_ids="$(
    python3 - <<'PY' "${keys_json}"
import json,sys
try:
    obj=json.loads(sys.argv[1])
except Exception:
    obj=[]
ids=[str(x.get("id")) for x in obj if isinstance(x,dict) and "id" in x]
print("\n".join([i for i in ids if i and i != "None"]))
PY
  )"
  if [[ -n "${key_ids}" ]]; then
    while IFS= read -r key_id; do
      [[ -n "${key_id}" ]] || continue
      curl -fsS -X DELETE "${API_URL}/user/keys/${key_id}" \
        -H "Content-Type: application/json" \
        -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" >/dev/null 2>&1 || true
    done <<< "${key_ids}"
  fi

  tmp="$(mktemp)"
  code="$(
    curl -sS -o "${tmp}" -w "%{http_code}" \
      -X POST "${API_URL}/user/keys" \
      -H "Content-Type: application/json" \
      -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
      -d "{\"title\":\"E2E Test Key\",\"key\":\"${pub_key_content}\"}" \
      || true
  )"

  case "${code}" in
    201)
      echo "SSH key registered in Gitea"
      ;;
    422)
      echo "WARN: Gitea rejected SSH key (HTTP 422); SSH tests may be skipped" >&2
      sed 's/^/  /' "${tmp}" >&2 || true
      ;;
    *)
      echo "WARN: Unexpected response registering SSH key (HTTP ${code}); continuing" >&2
      sed 's/^/  /' "${tmp}" >&2 || true
      ;;
  esac
  rm -f "${tmp}"
}

ensure_repo() {
  echo "Ensuring Gitea repo exists: ${ORG_NAME}/${REPO_NAME}"

  local tmp code
  tmp="$(mktemp)"
  code="$(
    curl -sS -o "${tmp}" -w "%{http_code}" \
      -X POST "${API_URL}/orgs/${ORG_NAME}/repos" \
      -H "Content-Type: application/json" \
      -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
      -d "{\"name\":\"${REPO_NAME}\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":false}" \
      || true
  )"

  case "${code}" in
    201)
      echo "Repo created"
      ;;
    409)
      echo "Repo already exists"
      ;;
    *)
      echo "WARN: Unexpected response creating repo (HTTP ${code}); continuing" >&2
      sed 's/^/  /' "${tmp}" >&2 || true
      ;;
  esac

  rm -f "${tmp}"
}

generate_known_hosts() {
  local known_hosts_file tmp pf_pid local_port ssh_host
  known_hosts_file="${ssh_dir}/known_hosts"
  tmp="$(mktemp)"
  local_port="12222"
  ssh_host="gitea-ssh.${GITEA_NAMESPACE}.svc.cluster.local"

  rm -f "${tmp}"
  : > "${known_hosts_file}"

  if ! command -v ssh-keyscan >/dev/null 2>&1; then
    echo "ssh-keyscan not found; leaving known_hosts empty (controller will use insecure host key verification)"
    return 0
  fi

  echo "Generating known_hosts via temporary port-forward (best effort)"
  kubectl --context "${CTX}" -n "${GITEA_NAMESPACE}" port-forward --address 127.0.0.1 \
    "svc/gitea-ssh" "${local_port}:2222" >/dev/null 2>&1 &
  pf_pid="$!"
  trap 'kill "${pf_pid}" 2>/dev/null || true' RETURN

  for i in {1..10}; do
    if timeout 2 bash -c "echo >/dev/tcp/127.0.0.1/${local_port}" 2>/dev/null; then
      break
    fi
    sleep 1
  done

  if timeout 10 ssh-keyscan -p "${local_port}" 127.0.0.1 > "${tmp}" 2>/dev/null && [[ -s "${tmp}" ]]; then
    sed -E "s/^\\[127\\.0\\.0\\.1\\]:${local_port}/[${ssh_host}]:2222/; s/^127\\.0\\.0\\.1\\s/[${ssh_host}]:2222 /" \
      "${tmp}" > "${known_hosts_file}" || true
  fi

  kill "${pf_pid}" 2>/dev/null || true
  trap - RETURN
  rm -f "${tmp}"
}

write_secrets_manifest() {
  local token secrets_tmp ssh_args=()
  token="$(tr -d '\n' < "${token_file}" | tr -d '\r')"
  [[ -n "${token}" ]] || { echo "ERROR: token.txt is empty" >&2; exit 1; }

  secrets_tmp="$(mktemp)"
  rm -f "${secrets_tmp}"

  # Ensure target namespace exists.
  kubectl --context "${CTX}" create namespace "${NAMESPACE}" --dry-run=client -o yaml \
    | kubectl --context "${CTX}" apply -f - >/dev/null

  if [[ -s "${ssh_dir}/known_hosts" ]]; then
    ssh_args+=(--from-file=known_hosts="${ssh_dir}/known_hosts")
  fi

  {
    kubectl --context "${CTX}" -n "${NAMESPACE}" create secret generic "${SECRET_HTTP_NAME}" \
      --from-literal=username="${GITEA_ADMIN_USER}" \
      --from-literal=password="${token}" \
      --dry-run=client -o yaml
    printf '\n---\n'
    kubectl --context "${CTX}" -n "${NAMESPACE}" create secret generic "${SECRET_SSH_NAME}" \
      --from-file=ssh-privatekey="${ssh_dir}/id_rsa" \
      "${ssh_args[@]}" \
      --dry-run=client -o yaml
    printf '\n---\n'
    kubectl --context "${CTX}" -n "${NAMESPACE}" create secret generic "${SECRET_INVALID_NAME}" \
      --from-literal=username="invaliduser" \
      --from-literal=password="invalidpassword" \
      --dry-run=client -o yaml
  } > "${secrets_tmp}"

  mv "${secrets_tmp}" "${secrets_yaml}"
}

apply_secrets() {
  kubectl --context "${CTX}" apply -f "${secrets_yaml}" >/dev/null
  touch "${secrets_applied}"
}

ensure_checkout() {
  local checkout_dir repo_url
  if [[ -n "${CHECKOUT_DIR}" ]]; then
    checkout_dir="${CHECKOUT_DIR}"
  else
    checkout_dir="${repo_dir}/checkout"
  fi

  mkdir -p "$(dirname "${checkout_dir}")"
  printf '%s\n' "${checkout_dir}" > "${repo_checkout_path_file}"

  repo_url="http://localhost:13000/${ORG_NAME}/${REPO_NAME}.git"

  if [[ -d "${checkout_dir}/.git" ]]; then
    git -C "${checkout_dir}" remote set-url origin "${repo_url}" >/dev/null 2>&1 || true
  else
    rm -rf "${checkout_dir}"
    git clone "${repo_url}" "${checkout_dir}" >/dev/null 2>&1 || {
      echo "ERROR: failed to clone ${repo_url} to ${checkout_dir}" >&2
      exit 1
    }
  fi

  git -C "${checkout_dir}" config user.name "E2E Test" >/dev/null 2>&1 || true
  git -C "${checkout_dir}" config user.email "e2e-test@gitops-reverser.local" >/dev/null 2>&1 || true

  if [[ ! -f "${checkout_dir}/.git/HEAD" ]]; then
    echo "ERROR: expected checkout to contain .git/HEAD at ${checkout_dir}" >&2
    exit 1
  fi

  touch "${repo_ready}"
  touch "${checkout_ready}"
}

wait_for_api

token="$(create_token)"
printf '%s' "${token}" >/dev/null

ensure_ssh_keys
generate_known_hosts
configure_ssh_key_in_gitea
ensure_repo
write_secrets_manifest
apply_secrets
ensure_checkout
