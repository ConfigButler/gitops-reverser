#!/usr/bin/env bash

set -euo pipefail

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"

# Refresh signing on every start: this is the first hook that can expect a
# forwarded SSH agent, and a reconnect can change SSH_AUTH_SOCK or the loaded
# key. The helper reports missing credentials rather than failing, and Git
# itself is what refuses an unsigned commit, so startup never depends on them.
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh" ||
  echo "[post-start] WARNING: sync-signing-key.sh failed; run it by hand to see why." >&2

if [[ ! -f "${workspace_dir}/.env" ]]; then
  echo "hint: ${workspace_dir}/.env is absent; add GH_TOKEN there if you want read-only gh CLI access."
fi
