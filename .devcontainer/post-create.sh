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

# Resolve Git identity from effective config first, then fallback env vars.
git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"

if [ -z "${git_name}" ] && [ -n "${GIT_USER_NAME:-}" ]; then
  git_name="${GIT_USER_NAME}"
fi

if [ -z "${git_email}" ] && [ -n "${GIT_USER_EMAIL:-}" ]; then
  git_email="${GIT_USER_EMAIL}"
fi

if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  fail "Missing Git identity. Set user.name and user.email in Git, or provide GIT_USER_NAME and GIT_USER_EMAIL to the devcontainer environment."
fi

# Persist identity globally in the container if it is not already configured there.
if ! git config --global --get user.name >/dev/null 2>&1; then
  git config --global user.name "${git_name}"
fi

if ! git config --global --get user.email >/dev/null 2>&1; then
  git config --global user.email "${git_email}"
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
