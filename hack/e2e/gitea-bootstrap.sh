#!/usr/bin/env bash
set -euo pipefail

# Cluster-scoped bootstrap for the shared e2e Gitea instance.
#
# This script assumes a localhost port-forward to Gitea HTTP is already running
# (typically via `make prepare-e2e` -> `portforward-ensure`).
#
# Expected outputs (under BOOTSTRAP_DIR):
# - api.ready
# - org-<ORG_NAME>.ready
# - ready

BOOTSTRAP_DIR="${BOOTSTRAP_DIR:-}"
API_URL="${API_URL:-http://localhost:13000/api/v1}"

GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-giteaadmin}"
GITEA_ADMIN_PASS="${GITEA_ADMIN_PASS:-giteapassword123}"
ORG_NAME="${ORG_NAME:-testorg}"

if [[ -z "${BOOTSTRAP_DIR}" ]]; then
  echo "ERROR: BOOTSTRAP_DIR must be set" >&2
  exit 2
fi

mkdir -p "${BOOTSTRAP_DIR}"

api_ready_file="${BOOTSTRAP_DIR}/api.ready"
org_ready_file="${BOOTSTRAP_DIR}/org-${ORG_NAME}.ready"
ready_file="${BOOTSTRAP_DIR}/ready"

wait_for_api() {
  echo "Checking Gitea API connectivity at ${API_URL}..."
  for i in {1..30}; do
    if curl -fsS "${API_URL}/version" >/dev/null 2>&1; then
      touch "${api_ready_file}"
      echo "Gitea API reachable"
      return 0
    fi
    echo "Waiting for Gitea API... (${i}/30)"
    sleep 2
  done

  echo "ERROR: Failed to reach Gitea API at ${API_URL}/version (is the port-forward running?)" >&2
  exit 1
}

ensure_org() {
  echo "Ensuring Gitea org exists: ${ORG_NAME}"

  local tmp
  tmp="$(mktemp)"
  # POST /orgs returns 201 on creation, 409/422 when already exists depending on Gitea version/config.
  local code
  code="$(
    curl -sS -o "${tmp}" -w "%{http_code}" \
      -X POST "${API_URL}/orgs" \
      -H "Content-Type: application/json" \
      -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
      -d "{\"username\":\"${ORG_NAME}\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}" \
      || true
  )"

  case "${code}" in
    201)
      echo "Org created"
      ;;
    409|422)
      echo "Org already exists"
      ;;
    *)
      echo "WARN: Unexpected response creating org (HTTP ${code}); continuing" >&2
      sed 's/^/  /' "${tmp}" >&2 || true
      ;;
  esac

  rm -f "${tmp}"
  touch "${org_ready_file}"
}

wait_for_api
ensure_org

if [[ -f "${api_ready_file}" && -f "${org_ready_file}" ]]; then
  touch "${ready_file}"
fi

