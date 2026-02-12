#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

# Keep docker socket usable in the devcontainer (best-effort)
log "Setting permissions on /var/run/docker.sock (best-effort)"
sudo chmod 666 /var/run/docker.sock || true

# Resolve workspace path in a way that works both inside and outside
# VS Code-specific shell variable injection.
workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"
log "Using workspace directory: ${workspace_dir}"

# Ensure kind network exists (best-effort)
log "Checking docker network 'kind'"
if ! docker network inspect kind >/dev/null 2>&1; then
  log "Creating docker network 'kind'"
  docker network create -d=bridge --subnet=172.19.0.0/24 kind >/dev/null 2>&1 || true
else
  log "Docker network 'kind' already exists"
fi

# Ensure Go-related caches exist and are writable by vscode
log "Ensuring Go cache directories exist"
sudo mkdir -p \
  /home/vscode/.cache/go-build \
  /home/vscode/.cache/goimports \
  /home/vscode/.cache/golangci-lint

# Fix ownership for workspace and cache roots used by tooling
if [ -d "${workspace_dir}" ]; then
  log "Fixing ownership for workspace and cache directories"
  sudo chown -R vscode:vscode "${workspace_dir}" /home/vscode || true
else
  log "Workspace directory not found; fixing ownership for cache only"
  sudo chown -R vscode:vscode /home/vscode || true
fi

log "post-create completed"
