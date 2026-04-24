#!/usr/bin/env bash

set -euo pipefail

workspace_dir="${1:-${containerWorkspaceFolder:-${WORKSPACE_FOLDER:-$(pwd)}}}"

bash "${workspace_dir}/.devcontainer/sync-signing-key.sh"

if [[ ! -f "${workspace_dir}/.env" ]]; then
  echo "hint: ${workspace_dir}/.env is absent; add GH_TOKEN there if you want read-only gh CLI access."
fi
