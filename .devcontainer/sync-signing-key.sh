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
# derive on its own -- for whichever signing key is actually in play. Two are
# supported, in this order:
#
#   1. a user.signingkey the environment configured (a platform that manages
#      its own key file, a developer who chose one). It is preserved as-is and
#      needs no SSH agent.
#   2. a key from a forwarded SSH agent, the usual VS Code flow.
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

# --- Verification data. Needs an identity and a key source. ------------------

git_email="$(git config --get user.email || true)"
if [ -z "${git_email}" ]; then
  log "No Git identity configured yet; signing is enabled but allowed_signers is not written."
  log "Configure user.email (or let the platform personalize Git), then re-run this script."
  exit 0
fi

# --- Resolve the public key(s) to trust ---------------------------------------

# public_key_for_signingkey prints the public key line for a configured
# user.signingkey, or nothing when it cannot be derived. Only ever reads public
# key material: for a private key path it reads the sibling .pub, never the key.
public_key_for_signingkey() {
  local value="$1"

  # A literal key, which is what this script pins for an agent key.
  if [ "${value#key::}" != "${value}" ]; then
    printf '%s\n' "${value#key::}"
    return 0
  fi

  # A path to a public key.
  case "${value}" in
    *.pub)
      if [ -r "${value}" ]; then
        head -n 1 "${value}"
        return 0
      fi
      return 1
      ;;
  esac

  # A path to a private key: the public half sits beside it by convention.
  if [ -r "${value}.pub" ]; then
    head -n 1 "${value}.pub"
    return 0
  fi

  return 1
}

# Ownership is recorded explicitly rather than inferred from the value. "key::"
# is the documented way for ANYONE to configure a literal signing key, so
# treating that prefix as a signature of this script would let it replace or
# unset a key a platform deliberately configured.
managed_key_marker="devcontainer.managedSigningKey"

configured_key="$(git config --global --get user.signingkey || true)"
managed_key="$(git config --global --get "${managed_key_marker}" || true)"
own_pin=false
if [ -z "${configured_key}" ] || [ "${configured_key}" = "${managed_key}" ]; then
  # Unset, or the exact value this script last wrote: ours to manage.
  own_pin=true
fi

trusted_keys_file="$(mktemp)"
trap 'rm -f "${trusted_keys_file}"' EXIT

# keep_public_keys filters a key listing down to well-formed public key lines.
# Matching on the base64 blob rather than on a "ssh-" prefix keeps ecdsa-*,
# sk-ssh-* and sk-ecdsa-* keys, which are valid for both signing and
# allowed_signers.
keep_public_keys() {
  local file="$1" tmp
  tmp="$(mktemp)"
  awk '$2 ~ /^AAAA/' "${file}" >"${tmp}"
  mv "${tmp}" "${file}"
}

if [ "${own_pin}" = false ]; then
  # An externally configured signing key wins over the agent. A platform may
  # deliberately configure a key before invoking this script, and the whole
  # point of that is for it to be used -- so it is never replaced here, and no
  # agent is required. Only verification is added.
  public_key_for_signingkey "${configured_key}" >"${trusted_keys_file}" || true
  keep_public_keys "${trusted_keys_file}"
  if [ -s "${trusted_keys_file}" ]; then
    log "Using the signing key already configured at ${configured_key}"
  else
    warn "user.signingkey is ${configured_key}, but no public key could be derived from it."
    warn "Expected either a readable *.pub, or a sibling <key>.pub next to a private key."
    warn "Signing is left exactly as configured; local verification is not set up."
    exit 0
  fi
else
  if [ -n "${SSH_AUTH_SOCK:-}" ]; then
    # ssh-add -L exits 1 when the agent holds no keys and 2 when it cannot be
    # reached; neither is fatal here.
    ssh-add -L >"${trusted_keys_file}" 2>/dev/null || true
  fi

  keep_public_keys "${trusted_keys_file}"
  if [ ! -s "${trusted_keys_file}" ]; then
    warn "No signing key is available: no user.signingkey is configured and no SSH agent key was found."
    warn "Signing stays mandatory: git commit will fail until a key is available."
    warn "Either load one on your machine (ssh-add ~/.ssh/id_ed25519) and attach a session that"
    warn "forwards the agent, or configure user.signingkey; then re-run:"
    warn "  bash .devcontainer/sync-signing-key.sh"
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
  matched_key="$(awk -v email="${git_email}" 'index($0, email) { print; exit }' "${trusted_keys_file}")"
  if [ -n "${matched_key}" ]; then
    git config --global user.signingkey "key::${matched_key}"
    git config --global "${managed_key_marker}" "key::${matched_key}"
    log "Signing with the agent key whose comment matches ${git_email}"
  else
    if [ -n "${configured_key}" ]; then
      # A previous run of OURS matched, this one does not. Drop the pin rather
      # than keep signing with a key the agent may no longer hold.
      git config --global --unset user.signingkey
      git config --global --unset "${managed_key_marker}" 2>/dev/null || true
    fi
    key_count="$(wc -l <"${trusted_keys_file}" | tr -d ' ')"
    if [ "${key_count}" -gt 1 ]; then
      warn "No agent key comment matches ${git_email} and ${key_count} keys are loaded;"
      warn "the first one from ssh-add -L will sign. To choose deliberately, give the intended key a"
      warn "comment containing ${git_email}: ssh-keygen -c -C \"${git_email}\" -f ~/.ssh/id_ed25519"
    fi
  fi
fi

# allowed_signers principals are bare identities: "PRINCIPALS keytype base64
# [comment]". A "Name <email>" principal is not valid there -- ssh-keygen -Y
# rejects the line ("invalid key", "No principal matched"), so a correctly
# signed commit fails to verify. The trailing key comment is valid and useful,
# and is kept.
#
# For the agent case every loaded key is listed, not just the signing one:
# they are all the same developer's keys, and it keeps verification working
# whichever one the key command picks. For an externally configured key it is
# that one key, which is the only thing that can sign.
allowed_signers_path="${HOME}/.config/git/allowed_signers"
mkdir -p "${HOME}/.config/git"
awk -v email="${git_email}" '{ print email, $0 }' "${trusted_keys_file}" >"${allowed_signers_path}"
chmod 600 "${allowed_signers_path}"

configured_signers="$(git config --global --get gpg.ssh.allowedSignersFile || true)"
if [ -z "${configured_signers}" ] || [ "${configured_signers}" = "${allowed_signers_path}" ]; then
  git config --global gpg.ssh.allowedSignersFile "${allowed_signers_path}"
else
  warn "gpg.ssh.allowedSignersFile points at ${configured_signers}; leaving it alone."
  warn "Refreshed ${allowed_signers_path}, which is currently unused."
fi

log "Signing ready: $(wc -l <"${allowed_signers_path}" | tr -d ' ') key(s) trusted for ${git_email}"
