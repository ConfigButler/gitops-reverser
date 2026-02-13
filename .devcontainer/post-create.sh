#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

# Resolve workspace path in a way that works both inside and outside
# VS Code-specific shell variable injection.
workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"
log "Using workspace directory: ${workspace_dir}"

# Keep ~/.gitconfig writable inside the container while still importing host settings.
if [ -f /home/vscode/.gitconfig-host ]; then
  log "Configuring git to include /home/vscode/.gitconfig-host"
  touch /home/vscode/.gitconfig
  if git config --global --get-all include.path | grep -Fxq "/home/vscode/.gitconfig-host"; then
    log "Host gitconfig include already present"
  else
    git config --global --add include.path /home/vscode/.gitconfig-host
    log "Added host gitconfig include"
  fi
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
