#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"
log "Using workspace directory: ${workspace_dir}"

# Git identity is developer personalization, not part of building a usable
# development environment, so its absence is reported and not fatal. Whatever
# created this container (VS Code copying the host Git config, a platform's own
# personalization step) may supply it before, during or after this hook.
git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"

if [ -z "${git_name}" ] && [ -n "${GIT_USER_NAME:-}" ]; then
  git_name="${GIT_USER_NAME}"
fi

if [ -z "${git_email}" ] && [ -n "${GIT_USER_EMAIL:-}" ]; then
  git_email="${GIT_USER_EMAIL}"
fi

if [ -n "${git_name}" ] && [ -z "$(git config --global --get user.name || true)" ]; then
  git config --global user.name "${git_name}"
fi

if [ -n "${git_email}" ] && [ -z "$(git config --global --get user.email || true)" ]; then
  git config --global user.email "${git_email}"
fi

if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  log "WARNING: no Git identity yet. Set user.name and user.email, or provide GIT_USER_NAME and"
  log "WARNING: GIT_USER_EMAIL to the devcontainer environment. Commits need one; the rest of the"
  log "WARNING: environment does not, so setup continues."
fi

# Signing is not configured here. post-start.sh runs on every start, including
# the first one right after this hook, and that is where an SSH agent can
# actually be expected to exist.

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
