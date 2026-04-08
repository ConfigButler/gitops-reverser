#!/usr/bin/env bash
# connect-remote-k3d.sh
# Fetches the k3d kubeconfig from a remote host and sets up a local SSH tunnel.
# Usage: ./connect-remote-k3d.sh [remote-host] [cluster-name]
set -euo pipefail

REMOTE_HOST="${1:-remote-dev.z65}"
CLUSTER_NAME="${2:-}"
KUBECONFIG_PATH="${HOME}/.kube/remote-k3d.yaml"

echo "==> Connecting to ${REMOTE_HOST}..."

# k3d needs a proper login shell on the remote side to work correctly
if [[ -n "$CLUSTER_NAME" ]]; then
  RAW_KUBECONFIG=$(ssh "${REMOTE_HOST}" bash -l -c "k3d kubeconfig get ${CLUSTER_NAME}")
else
  RAW_KUBECONFIG=$(ssh "${REMOTE_HOST}" bash -l -c "k3d kubeconfig get --all")
fi

if [[ -z "$RAW_KUBECONFIG" ]]; then
  echo "ERROR: Failed to fetch kubeconfig from ${REMOTE_HOST}" >&2
  exit 1
fi

echo "$RAW_KUBECONFIG" > "${KUBECONFIG_PATH}"
echo "==> Kubeconfig saved to ${KUBECONFIG_PATH}"

# Extract port from the server URL (e.g. https://0.0.0.0:43567)
API_PORT=$(grep 'server:' "${KUBECONFIG_PATH}" | grep -oP ':\K[0-9]+$' | head -1)

if [[ -z "$API_PORT" ]]; then
  echo "ERROR: Could not determine API server port from kubeconfig" >&2
  exit 1
fi

echo "==> API server port: ${API_PORT}"

# Rewrite the server address to localhost
sed -i "s|https://[^:]*:${API_PORT}|https://127.0.0.1:${API_PORT}|g" "${KUBECONFIG_PATH}"

# Set up SSH tunnel if not already active
if ss -tlnp 2>/dev/null | grep -q ":${API_PORT} " || \
   ss -tlnp 2>/dev/null | grep -q ":${API_PORT}$"; then
  echo "==> Tunnel on port ${API_PORT} already active, skipping"
else
  echo "==> Setting up SSH tunnel: localhost:${API_PORT} -> ${REMOTE_HOST}:${API_PORT}"
  ssh -f -N -L "${API_PORT}:127.0.0.1:${API_PORT}" "${REMOTE_HOST}"
  echo "==> Tunnel established"
fi

echo ""
echo "Done! Use your cluster with:"
echo "  export KUBECONFIG=${KUBECONFIG_PATH}"
echo "  kubectl get nodes"
