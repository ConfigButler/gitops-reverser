#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <cluster-stamp-dir> <context> -- <command> [args...]" >&2
}

if [ "$#" -lt 4 ] || [ "$3" != "--" ]; then
  usage
  exit 2
fi

cluster_stamp_dir="$1"
ctx="$2"
shift 3

if ! command -v flock >/dev/null 2>&1; then
  echo "ERROR: flock is required to serialize e2e runs" >&2
  exit 127
fi

mkdir -p "${cluster_stamp_dir}"
lock_file="${cluster_stamp_dir}/e2e.lock"

if [ "${E2E_LOCK_FILE:-}" = "${lock_file}" ]; then
  exec "$@"
fi

exec 9>"${lock_file}"

case "${E2E_LOCK_WAIT:-false}" in
  true|1|yes)
    echo "Waiting for e2e lock ${lock_file} (CTX=${ctx})..."
    flock 9
    ;;
  *)
    if ! flock -n 9; then
      echo "Another e2e run is already active for CTX=${ctx}; lock: ${lock_file}" >&2
      echo "Set E2E_LOCK_WAIT=true to wait for the current run instead of failing fast." >&2
      exit 75
    fi
    ;;
esac

export E2E_LOCK_FILE="${lock_file}"
exec "$@"
