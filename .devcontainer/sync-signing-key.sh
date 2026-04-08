#!/usr/bin/env bash

set -euo pipefail

log() {
  echo "[sync-signing-key] $*"
}

fail() {
  echo "[sync-signing-key] ERROR: $*" >&2
  exit 1
}

git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"

if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  fail "Missing Git identity. Configure user.name and user.email before syncing SSH signing."
fi

if [ -z "${SSH_AUTH_SOCK:-}" ]; then
  fail "SSH agent not available in the devcontainer. Start ssh-agent on your machine, load your key with ssh-add, then reopen the devcontainer."
fi

agent_keys_file="$(mktemp)"
trap 'rm -f "${agent_keys_file}"' EXIT

if ! ssh-add -L >"${agent_keys_file}" 2>/dev/null; then
  fail "Could not read SSH keys from agent. Make sure your key is loaded on your machine with ssh-add, then reopen the devcontainer."
fi

if ! grep -qE '^ssh-' "${agent_keys_file}"; then
  fail "SSH agent is running but has no keys loaded. Run ssh-add ~/.ssh/id_ed25519 on your machine, then reopen the devcontainer."
fi

selected_pubkey="$(awk -v email="${git_email}" '$0 ~ email { print; exit }' "${agent_keys_file}")"
if [ -z "${selected_pubkey}" ]; then
  selected_pubkey="$(head -n 1 "${agent_keys_file}")"
  log "No SSH key comment matched ${git_email}; using the first agent key"
else
  log "Using SSH key whose comment matches ${git_email}"
fi

signing_key_path="${HOME}/.ssh/devcontainer_signing_key.pub"
allowed_signers_path="${HOME}/.config/git/allowed_signers"

git config --global gpg.format ssh
git config --global commit.gpgsign true

mkdir -p "${HOME}/.config/git" "${HOME}/.ssh"
printf '%s\n' "${selected_pubkey}" > "${signing_key_path}"
chmod 600 "${signing_key_path}"
git config --global user.signingkey "${signing_key_path}"

printf '%s <%s> %s\n' "${git_name}" "${git_email}" "${selected_pubkey}" > "${allowed_signers_path}"
chmod 600 "${allowed_signers_path}"
git config --global gpg.ssh.allowedSignersFile "${allowed_signers_path}"

log "Git SSH signing configuration refreshed"
