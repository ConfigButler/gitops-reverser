#!/usr/bin/env bash

# Configure Git SSH commit signing against whatever the forwarded SSH agent
# currently offers.
#
# The mandatory half of the policy is plain Git configuration, not shell logic:
#
#   commit.gpgsign=true            every commit is signed
#   gpg.format=ssh                 signed with an SSH key
#   gpg.ssh.defaultKeyCommand      the key comes from the live agent
#
# With those set and no agent, `git commit` fails on its own ("fatal: either
# user.signingkey or gpg.ssh.defaultKeyCommand needs to be configured", or the
# key command returning nothing). So this script never has to police commits,
# and never has to fail a lifecycle hook to keep signing mandatory.
#
# What it adds on top is verification data: allowed_signers, which Git cannot
# derive on its own.
#
# Idempotent and platform-neutral: safe on create, on every start and by hand,
# and it leaves identity and credentials to whatever created the container.

set -euo pipefail

log() {
  echo "[sync-signing-key] $*"
}

warn() {
  echo "[sync-signing-key] WARNING: $*" >&2
}

# --- Generic baseline. Needs no Git identity and no SSH agent. ---------------

# The policy this repository asserts: commits are signed. Always re-applied.
git config --global commit.gpgsign true

# Only fill in what is unset, so a developer or platform that has already
# chosen a signing mechanism (an openpgp gpg.format, their own key command)
# keeps it.
if [ -z "$(git config --global --get gpg.format || true)" ]; then
  git config --global gpg.format ssh
fi

if [ -z "$(git config --global --get gpg.ssh.defaultKeyCommand || true)" ]; then
  # Resolves the signing key from the agent at commit time. Nothing is cached,
  # so a reconnect that changes SSH_AUTH_SOCK or swaps the loaded key needs no
  # re-sync, and there is no stale key to go wrong.
  git config --global gpg.ssh.defaultKeyCommand "ssh-add -L"
fi

# Earlier versions pinned user.signingkey to a public key file this script
# wrote. That pin is stale the moment the agent offers a different key, and it
# overrides the key command above. Retire it -- but only when it is still the
# exact path this script used to manage, never a key someone else configured.
legacy_key_file="${HOME}/.ssh/devcontainer_signing_key.pub"
if [ "$(git config --global --get user.signingkey || true)" = "${legacy_key_file}" ]; then
  log "Removing the pinned signing key; the agent now supplies it per commit"
  git config --global --unset user.signingkey
  rm -f "${legacy_key_file}"
fi

# --- Verification data. Needs both an identity and an agent. -----------------

git_email="$(git config --get user.email || true)"
if [ -z "${git_email}" ]; then
  log "No Git identity configured yet; signing is enabled but allowed_signers is not written."
  log "Configure user.email (or let the platform personalize Git), then re-run this script."
  exit 0
fi

agent_keys_file="$(mktemp)"
trap 'rm -f "${agent_keys_file}"' EXIT

if [ -n "${SSH_AUTH_SOCK:-}" ]; then
  # ssh-add -L exits 1 when the agent holds no keys and 2 when it cannot be
  # reached; neither is fatal here.
  ssh-add -L >"${agent_keys_file}" 2>/dev/null || true
fi

if ! grep -qE '^ssh-' "${agent_keys_file}"; then
  warn "No SSH key is available from an agent, so commits cannot be signed yet."
  warn "Signing stays mandatory: git commit will fail until a key is loaded."
  warn "Load one on your machine (ssh-add ~/.ssh/id_ed25519) and attach a session that forwards"
  warn "the agent; refresh sooner with: bash .devcontainer/sync-signing-key.sh"
  exit 0
fi

# Deterministic selection when the developer has expressed intent: a key whose
# comment contains the Git email wins over agent order. index() is a literal
# substring search -- an email used as a regex would treat its dots as
# wildcards. With no match, gpg.ssh.defaultKeyCommand picks, which is both
# simpler and immune to going stale.
#
# The pin is a literal key rather than a path, so there is no key file to
# manage, and it is only ever written over an empty setting or a previous pin
# of ours ("key::..."). A signing key configured by the developer or by the
# platform that created this container is left alone.
matched_key="$(awk -v email="${git_email}" '/^ssh-/ && index($0, email) { print; exit }' "${agent_keys_file}")"
configured_key="$(git config --global --get user.signingkey || true)"
own_pin=false
if [ -z "${configured_key}" ] || [ "${configured_key#key::}" != "${configured_key}" ]; then
  own_pin=true
fi

if [ "${own_pin}" = false ]; then
  log "user.signingkey is set to something this script did not write; leaving it alone"
elif [ -n "${matched_key}" ]; then
  git config --global user.signingkey "key::${matched_key}"
  log "Signing with the agent key whose comment matches ${git_email}"
else
  if [ -n "${configured_key}" ]; then
    # A previous run matched, this one does not. Drop our pin rather than keep
    # signing with a key the agent may no longer hold.
    git config --global --unset user.signingkey
  fi
  key_count="$(grep -cE '^ssh-' "${agent_keys_file}")"
  if [ "${key_count}" -gt 1 ]; then
    warn "No agent key comment matches ${git_email} and ${key_count} keys are loaded;"
    warn "the first one from ssh-add -L will sign. To choose deliberately, give the intended key a"
    warn "comment containing ${git_email}: ssh-keygen -c -C \"${git_email}\" -f ~/.ssh/id_ed25519"
  fi
fi

# allowed_signers principals are bare identities: "PRINCIPALS keytype base64
# [comment]". A "Name <email>" principal is not valid there -- ssh-keygen -Y
# rejects the line ("invalid key", "No principal matched"), so a correctly
# signed commit fails to verify. The trailing key comment is valid and useful,
# and is kept.
#
# Every key in the agent is listed, not just the signing one: they are all the
# same developer's keys, and this keeps verification working for commits signed
# before the pin above existed, or by whichever key the key command picks.
allowed_signers_path="${HOME}/.config/git/allowed_signers"
mkdir -p "${HOME}/.config/git"
awk -v email="${git_email}" '/^ssh-/ { print email, $0 }' "${agent_keys_file}" >"${allowed_signers_path}"
chmod 600 "${allowed_signers_path}"

configured_signers="$(git config --global --get gpg.ssh.allowedSignersFile || true)"
if [ -z "${configured_signers}" ] || [ "${configured_signers}" = "${allowed_signers_path}" ]; then
  git config --global gpg.ssh.allowedSignersFile "${allowed_signers_path}"
else
  warn "gpg.ssh.allowedSignersFile points at ${configured_signers}; leaving it alone."
  warn "Refreshed ${allowed_signers_path}, which is currently unused."
fi

log "Signing ready: $(grep -c '^ssh-' "${agent_keys_file}") agent key(s) trusted for ${git_email}"
