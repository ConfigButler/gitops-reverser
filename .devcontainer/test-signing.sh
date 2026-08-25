#!/usr/bin/env bash

# Tests for sync-signing-key.sh, run against a throwaway HOME, a disposable
# ssh-agent and generated keys -- no developer key, identity or agent is
# touched. Run it by hand after changing the signing scripts:
#
#   bash .devcontainer/test-signing.sh

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sync_script="${script_dir}/sync-signing-key.sh"

readonly TEST_EMAIL="user@example.com"

sandbox="$(mktemp -d)"
agent_pid=""

cleanup() {
  [ -n "${agent_pid}" ] && kill "${agent_pid}" 2>/dev/null
  rm -rf "${sandbox}"
}
trap cleanup EXIT

failures=0

pass() { echo "  ok    - $*"; }
fail() { echo "  FAIL  - $*" >&2; failures=$((failures + 1)); }

check() {
  local description="$1" expected="$2" actual="$3"
  if [ "${expected}" = "${actual}" ]; then
    pass "${description}"
  else
    fail "${description} (expected '${expected}', got '${actual}')"
  fi
}

# Each case gets its own HOME so global Git config never leaks between them.
new_home() {
  local home="${sandbox}/home-$1"
  mkdir -p "${home}"
  if [ "${2:-}" = "with-identity" ]; then
    HOME="${home}" git config --global user.name "Test User"
    HOME="${home}" git config --global user.email "${TEST_EMAIL}"
  fi
  echo "${home}"
}

run_sync() {
  local home="$1"
  shift
  local status=0
  # cd somewhere without a repository so no repo-local config can supply the
  # identity the case under test is meant to control.
  ( cd "${sandbox}" && HOME="${home}" "$@" bash "${sync_script}" ) >"${sandbox}/out" 2>&1 || status=$?
  echo "${status}"
}

git_cfg() {
  HOME="$1" git config --global --get "$2" 2>/dev/null || true
}

# Commit in a scratch repo under the given HOME; echo the exit status.
try_commit() {
  local home="$1" repo="${sandbox}/repo-$2"
  shift 2
  local status=0
  mkdir -p "${repo}"
  (
    cd "${repo}"
    export HOME="${home}"
    "$@" git init -q -b main
    "$@" git commit -q --allow-empty -m "test commit"
  ) >"${sandbox}/commit-out" 2>&1 || status=$?
  echo "${status}"
}

echo "== disposable keys and ssh-agent =="
ssh-keygen -q -t ed25519 -N '' -C 'first-key' -f "${sandbox}/k1"
ssh-keygen -q -t ed25519 -N '' -C 'second-key' -f "${sandbox}/k2"
eval "$(ssh-agent -s)" >/dev/null
agent_pid="${SSH_AGENT_PID}"
ssh-add -q "${sandbox}/k1"
sock="${SSH_AUTH_SOCK}"

echo
echo "No Git identity: the environment still comes up"
home_n="$(new_home noidentity)"
check "exits 0" "0" "$(run_sync "${home_n}" env SSH_AUTH_SOCK="${sock}")"
check "signing is still mandatory" "true" "$(git_cfg "${home_n}" commit.gpgsign)"
check "no identity is invented" "" "$(git_cfg "${home_n}" user.email)"
[ -f "${home_n}/.config/git/allowed_signers" ] \
  && fail "wrote allowed_signers without an identity" \
  || pass "no allowed_signers without an identity"

echo
echo "Identity, no SSH agent: pending, not a failure"
home_c="$(new_home noagent with-identity)"
check "exits 0" "0" "$(run_sync "${home_c}" env -u SSH_AUTH_SOCK)"
check "signing is mandatory" "true" "$(git_cfg "${home_c}" commit.gpgsign)"
check "gpg.format is ssh" "ssh" "$(git_cfg "${home_c}" gpg.format)"
check "the key comes from the agent, not a pinned file" "ssh-add -L" \
  "$(git_cfg "${home_c}" gpg.ssh.defaultKeyCommand)"
check "no signing key is invented" "" "$(git_cfg "${home_c}" user.signingkey)"
grep -q "commits cannot be signed yet" "${sandbox}/out" \
  && pass "says signing credentials are unavailable" \
  || fail "no diagnostic about missing credentials"

# The load-bearing invariant: no credentials must mean a refused commit, not a
# silently unsigned one. Git enforces this by itself.
commit_status="$(try_commit "${home_c}" noagent env -u SSH_AUTH_SOCK)"
[ "${commit_status}" -ne 0 ] \
  && pass "git commit fails without a signing key (exit ${commit_status})" \
  || fail "git commit succeeded without signing credentials"
grep -qi "sign" "${sandbox}/commit-out" \
  && pass "the commit failure names signing" \
  || { fail "commit failure is not about signing"; sed 's/^/        /' "${sandbox}/commit-out" >&2; }

echo
echo "Identity and agent: signing configured"
home_e="$(new_home agent with-identity)"
check "exits 0" "0" "$(run_sync "${home_e}" env SSH_AUTH_SOCK="${sock}")"
allowed_signers="${home_e}/.config/git/allowed_signers"
check "allowed_signers is wired up" "${allowed_signers}" \
  "$(git_cfg "${home_e}" gpg.ssh.allowedSignersFile)"
check "principal is the bare Git email" "${TEST_EMAIL}" "$(awk 'NR==1 {print $1}' "${allowed_signers}")"
check "the key comment is preserved" "first-key" "$(awk 'NR==1 {print $4}' "${allowed_signers}")"
grep -q '<' "${allowed_signers}" \
  && fail "allowed_signers still contains a 'Name <email>' principal" \
  || pass "no 'Name <email>' principal"
check "no key file is left on disk" "absent" \
  "$([ -e "${home_e}/.ssh/devcontainer_signing_key.pub" ] && echo present || echo absent)"

check "a commit signs" "0" "$(try_commit "${home_e}" agent env SSH_AUTH_SOCK="${sock}")"
(
  cd "${sandbox}/repo-agent"
  HOME="${home_e}" SSH_AUTH_SOCK="${sock}" git log -1 --show-signature
) >"${sandbox}/verify-out" 2>&1 || true
grep -q "Good \"git\" signature for ${TEST_EMAIL}" "${sandbox}/verify-out" \
  && pass "git log --show-signature verifies it for ${TEST_EMAIL}" \
  || { fail "signature did not verify"; sed 's/^/        /' "${sandbox}/verify-out" >&2; }

echo
echo "Idempotency and reconnects"
before="$(cat "${allowed_signers}")"
check "a second run exits 0" "0" "$(run_sync "${home_e}" env SSH_AUTH_SOCK="${sock}")"
check "allowed_signers is unchanged" "${before}" "$(cat "${allowed_signers}")"
check "no ephemeral socket path is persisted" "" \
  "$(grep -rl "${sock}" "${home_e}" 2>/dev/null | head -n 1)"
check "an agent-less run afterwards still exits 0" "0" "$(run_sync "${home_e}" env -u SSH_AUTH_SOCK)"

echo
echo "A second agent key is added, neither matching the Git email"
ssh-add -q "${sandbox}/k2"
check "exits 0" "0" "$(run_sync "${home_e}" env SSH_AUTH_SOCK="${sock}")"
check "no key is pinned when none matches" "" "$(git_cfg "${home_e}" user.signingkey)"
grep -q "2 keys are loaded" "${sandbox}/out" \
  && pass "warns which key will sign" \
  || fail "no diagnostic about the ambiguity"
check "both agent keys are trusted" "2" "$(wc -l <"${allowed_signers}" | tr -d ' ')"
check "every line uses the email principal" "2" "$(awk -v e="${TEST_EMAIL}" '$1 == e' "${allowed_signers}" | wc -l | tr -d ' ')"
check "a commit still verifies" "0" "$(try_commit "${home_e}" twokeys env SSH_AUTH_SOCK="${sock}")"
(
  cd "${sandbox}/repo-twokeys"
  HOME="${home_e}" SSH_AUTH_SOCK="${sock}" git log -1 --show-signature
) >"${sandbox}/verify2-out" 2>&1 || true
grep -q "Good \"git\" signature for ${TEST_EMAIL}" "${sandbox}/verify2-out" \
  && pass "whichever key was used verifies" \
  || { fail "signature did not verify with two keys loaded"; sed 's/^/        /' "${sandbox}/verify2-out" >&2; }

echo
echo "A key whose comment matches the Git email is loaded"
ssh-keygen -q -t ed25519 -N '' -C "${TEST_EMAIL}" -f "${sandbox}/k3"
ssh-add -q "${sandbox}/k3"
home_p="$(new_home pinned with-identity)"
check "exits 0" "0" "$(run_sync "${home_p}" env SSH_AUTH_SOCK="${sock}")"
matched_fingerprint="$(ssh-keygen -lf "${sandbox}/k3.pub" | awk '{print $2}')"
check "the matching key is pinned, not agent order" "key::$(cat "${sandbox}/k3.pub" | sed 's/ *$//')" \
  "$(git_cfg "${home_p}" user.signingkey)"
check "a commit signs" "0" "$(try_commit "${home_p}" matched env SSH_AUTH_SOCK="${sock}")"
(
  cd "${sandbox}/repo-matched"
  HOME="${home_p}" SSH_AUTH_SOCK="${sock}" git log -1 --show-signature
) >"${sandbox}/verify3-out" 2>&1 || true
grep -q "${matched_fingerprint}" "${sandbox}/verify3-out" \
  && pass "the matching key is the one that signed" \
  || { fail "a different key signed"; sed 's/^/        /' "${sandbox}/verify3-out" >&2; }
grep -q "Good \"git\" signature for ${TEST_EMAIL}" "${sandbox}/verify3-out" \
  && pass "and it verifies" \
  || fail "the matched-key signature did not verify"

echo
echo "The matching key is unloaded again"
ssh-add -qd "${sandbox}/k3" 2>/dev/null
check "exits 0" "0" "$(run_sync "${home_p}" env SSH_AUTH_SOCK="${sock}")"
check "our stale pin is dropped" "" "$(git_cfg "${home_p}" user.signingkey)"
check "commits still work via the key command" "0" \
  "$(try_commit "${home_p}" unpinned env SSH_AUTH_SOCK="${sock}")"

echo
echo "Migration off a previously pinned signing key"
home_l="$(new_home legacy with-identity)"
mkdir -p "${home_l}/.ssh"
ssh-add -L | head -n1 >"${home_l}/.ssh/devcontainer_signing_key.pub"
HOME="${home_l}" git config --global user.signingkey "${home_l}/.ssh/devcontainer_signing_key.pub"
check "exits 0" "0" "$(run_sync "${home_l}" env SSH_AUTH_SOCK="${sock}")"
check "the stale pin is removed" "" "$(git_cfg "${home_l}" user.signingkey)"
check "the file it pinned is gone" "absent" \
  "$([ -e "${home_l}/.ssh/devcontainer_signing_key.pub" ] && echo present || echo absent)"

echo
echo "Configuration this script does not own is left alone"
home_o="$(new_home owned with-identity)"
HOME="${home_o}" git config --global gpg.format openpgp
HOME="${home_o}" git config --global user.signingkey "SOMEONE-ELSES-KEY"
check "exits 0" "0" "$(run_sync "${home_o}" env SSH_AUTH_SOCK="${sock}")"
check "an existing gpg.format is kept" "openpgp" "$(git_cfg "${home_o}" gpg.format)"
check "a foreign signing key is kept" "SOMEONE-ELSES-KEY" "$(git_cfg "${home_o}" user.signingkey)"
grep -q "did not write; leaving it alone" "${sandbox}/out" \
  && pass "says why it left the key alone" \
  || fail "no diagnostic about the foreign key"

echo
if [ "${failures}" -eq 0 ]; then
  echo "All signing tests passed."
else
  echo "${failures} signing test(s) failed." >&2
  exit 1
fi
