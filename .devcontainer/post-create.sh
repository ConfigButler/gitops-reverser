#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[post-create] $*"
}

fail() {
  echo "[post-create] ERROR: $*" >&2
  exit 1
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

# Require basic Git identity information.
git_name="$(git config --global --includes --get user.name || true)"
git_email="$(git config --global --includes --get user.email || true)"
if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  fail "Missing Git identity. Configure both user.name and user.email in your host ~/.gitconfig."
fi

# Respect existing signing settings (OpenPGP/SSH). Fallback to SSH signing only when missing.
if git config --global --includes --get commit.gpgsign >/dev/null 2>&1 \
  || git config --global --includes --get gpg.format >/dev/null 2>&1 \
  || git config --global --includes --get user.signingkey >/dev/null 2>&1; then
  log "Detected existing Git signing configuration; leaving signing settings unchanged"
else
  log "No Git signing configuration detected; configuring SSH signing fallback"

  mkdir -p /home/vscode/.ssh

  if ! ssh-add -L >/tmp/ssh-agent-keys.out 2>/dev/null || ! grep -qE '^ssh-' /tmp/ssh-agent-keys.out; then
    log "No SSH keys found in agent; creating fallback signing key"
    if [ ! -f /home/vscode/.ssh/signing_key ]; then
      ssh-keygen -t ed25519 -f /home/vscode/.ssh/signing_key -N "" -C "${git_email}" >/dev/null
    fi
    cp /home/vscode/.ssh/signing_key.pub /tmp/ssh-agent-keys.out
  fi

  first_pubkey="$(head -n 1 /tmp/ssh-agent-keys.out)"
  printf "%s %s\n" "${git_email}" "${first_pubkey}" > /home/vscode/.ssh/allowed_signers
  printf "%s\n" "${first_pubkey}" > /home/vscode/.ssh/signing_key.pub

  git config --global gpg.format ssh
  git config --global commit.gpgsign true
  git config --global gpg.ssh.allowedSignersFile /home/vscode/.ssh/allowed_signers
  git config --global user.signingkey /home/vscode/.ssh/signing_key.pub

  rm -f /tmp/ssh-agent-keys.out
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
