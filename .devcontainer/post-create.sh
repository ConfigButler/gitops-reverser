#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

fail() {
  echo "[post-create] ERROR: $*" >&2
  exit 1
}

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"
log "Using workspace directory: ${workspace_dir}"

# Resolve Git identity from effective config first, then fallback to mounted host file.
git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"

if [ -n "$git_name" ] && [ -n "$git_email" ]; then
  git config --global user.name "$git_name"
  git config --global user.email "$git_email"
else
  HOST_GIT_CONFIG="/home/vscode/.gitconfig-host"

  if [ -f "$HOST_GIT_CONFIG" ]; then
    git_name=$(git config -f "$HOST_GIT_CONFIG" user.name || true)
    git_email=$(git config -f "$HOST_GIT_CONFIG" user.email || true)

    if [ -n "$git_name" ] && [ -n "$git_email" ]; then
      git config --global user.name "$git_name"
      git config --global user.email "$git_email"
    else
      fail "user.name/user.email not found in host .gitconfig. Set them before rebuilding."
    fi
  else
    fail "Host .gitconfig not mounted. Git identity not configured in container."
  fi
fi

log "Refreshing Git SSH signing configuration"
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh"

log "Ensuring Go cache directories exist"
sudo mkdir -p \
  /home/vscode/.cache/go-build \
  /home/vscode/.cache/goimports \
  /home/vscode/.cache/golangci-lint

if [ -d "${workspace_dir}" ]; then
  log "Fixing ownership for workspace and home directories"
  sudo chown -R vscode:vscode "${workspace_dir}" /home/vscode || true
else
  log "Workspace directory not found; fixing ownership for home only"
  sudo chown -R vscode:vscode /home/vscode || true
fi

# Persist the ~/.claude.json file by making it a symlink (this trick can be used for other potenial config file in the home folder as well)
touch /home/vscode/persisted-home/.claude.json
rm -f /home/vscode/.claude.json && ln -s /home/vscode/persisted-home/.claude.json /home/vscode/.claude.json

log "post-create completed"
