#!/usr/bin/env bash
# connect-remote-k3d.sh
# Fetches the k3d kubeconfig from a remote host, sets up a local SSH tunnel,
# and merges the result into ~/.kube/config with context names prefixed
# "remote-dev-<clustername>".
# Usage: ./connect-remote-k3d.sh [remote-host] [cluster-name]
set -eo pipefail

REMOTE_HOST="${1:-remote-dev.z65}"
CLUSTER_NAME="${2:-}"
set -u

MAIN_KUBECONFIG="${HOME}/.kube/config"
STAGING_KUBECONFIG="${HOME}/.kube/remote-k3d.yaml"
PREFIX="remote-dev-"

echo "==> Connecting to ${REMOTE_HOST}..."

# k3d's PATH is set up in ~/.bashrc on the remote, so we need an interactive shell
if [[ -n "$CLUSTER_NAME" ]]; then
  RAW_KUBECONFIG=$(ssh "${REMOTE_HOST}" "bash -ic 'k3d kubeconfig get ${CLUSTER_NAME}'" 2>/dev/null)
else
  RAW_KUBECONFIG=$(ssh "${REMOTE_HOST}" "bash -ic 'k3d kubeconfig get --all'" 2>/dev/null)
fi

if [[ -z "$RAW_KUBECONFIG" ]]; then
  echo "ERROR: Failed to fetch kubeconfig from ${REMOTE_HOST}" >&2
  exit 1
fi

echo "$RAW_KUBECONFIG" > "${STAGING_KUBECONFIG}"

# Rename k3d-* identifiers to remote-dev-* so they don't collide with local k3d
sed -i "s/k3d-/${PREFIX}/g" "${STAGING_KUBECONFIG}"

# Extract port from the server URL (e.g. https://0.0.0.0:43567)
API_PORT=$(grep 'server:' "${STAGING_KUBECONFIG}" | grep -oP ':\K[0-9]+$' | head -1)

if [[ -z "$API_PORT" ]]; then
  echo "ERROR: Could not determine API server port from kubeconfig" >&2
  exit 1
fi

echo "==> API server port: ${API_PORT}"

# Rewrite the server address to localhost
sed -i "s|https://[^:]*:${API_PORT}|https://127.0.0.1:${API_PORT}|g" "${STAGING_KUBECONFIG}"

# Set up SSH tunnel if not already active
if ss -tlnp 2>/dev/null | grep -q ":${API_PORT} " || \
   ss -tlnp 2>/dev/null | grep -q ":${API_PORT}$"; then
  echo "==> Tunnel on port ${API_PORT} already active, skipping"
else
  echo "==> Setting up SSH tunnel: localhost:${API_PORT} -> ${REMOTE_HOST}:${API_PORT}"
  ssh -f -N -L "${API_PORT}:127.0.0.1:${API_PORT}" "${REMOTE_HOST}"
  echo "==> Tunnel established"
fi

# Merge into ~/.kube/config
echo "==> Merging into ${MAIN_KUBECONFIG}"
mkdir -p "$(dirname "${MAIN_KUBECONFIG}")"
touch "${MAIN_KUBECONFIG}"

# Remove any pre-existing entries with the remote-dev- prefix so we can replace them cleanly
for kind in contexts clusters users; do
  singular="${kind%s}"
  existing=$(KUBECONFIG="${MAIN_KUBECONFIG}" kubectl config view -o jsonpath="{.${kind}[*].name}" 2>/dev/null || true)
  for name in $existing; do
    if [[ "$name" == ${PREFIX}* ]]; then
      KUBECONFIG="${MAIN_KUBECONFIG}" kubectl config unset "${kind}.${name}" >/dev/null 2>&1 || true
      # delete-context is the only first-class delete; clusters/users only via unset above
      if [[ "$kind" == "contexts" ]]; then
        KUBECONFIG="${MAIN_KUBECONFIG}" kubectl config delete-context "$name" >/dev/null 2>&1 || true
      fi
    fi
  done
done

MERGED=$(KUBECONFIG="${MAIN_KUBECONFIG}:${STAGING_KUBECONFIG}" kubectl config view --flatten)
echo "$MERGED" > "${MAIN_KUBECONFIG}.new"
mv "${MAIN_KUBECONFIG}.new" "${MAIN_KUBECONFIG}"
chmod 600 "${MAIN_KUBECONFIG}"

echo ""
echo "Done! Available remote contexts:"
KUBECONFIG="${MAIN_KUBECONFIG}" kubectl config get-contexts -o name | grep "^${PREFIX}" | sed 's/^/  /'
echo ""
echo "Switch with: kubectl config use-context ${PREFIX}<clustername>"
