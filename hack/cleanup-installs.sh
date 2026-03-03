#!/usr/bin/env bash
set -euo pipefail

ctx="${CTX:-${1:-}}"
kubectl_bin="${KUBECTL:-${2:-kubectl}}"
base=".stamps/cluster/${ctx}"

if [[ -z "${ctx}" ]]; then
	echo "ERROR: CTX is required (set CTX or pass as arg 1)" >&2
	exit 2
fi

echo "🧹 Cleanup installs in context '${ctx}' (base: ${base})"

cleanup_ns() {
	local ns="$1"

	echo "🧹 Cleaning namespace '${ns}'"

	if [[ -d "${base}/${ns}" ]]; then
		while IFS= read -r -d '' f; do
			echo "  kubectl delete -f ${f}"
			"${kubectl_bin}" --context "${ctx}" delete -f "${f}" --ignore-not-found=true || true
		done < <(find "${base}/${ns}" -type f -name "install.yaml" -print0 2>/dev/null || true)
	fi

	if "${kubectl_bin}" --context "${ctx}" get namespace "${ns}" >/dev/null 2>&1; then
		echo "  kubectl delete namespace ${ns}"
		"${kubectl_bin}" --context "${ctx}" delete namespace "${ns}" --ignore-not-found=true || true
	fi

	if [[ -d "${base}/${ns}" ]]; then
		echo "  rm -rf ${base}/${ns}"
		rm -rf "${base:?}/${ns}" || true
	fi
}

# Match: .stamps/cluster/<ctx>/<namespace>/<install_method>/install.yaml
declare -A seen_ns=()
base_regex="${base//./\\.}"
while IFS= read -r -d '' m; do
	if [[ "${m}" =~ ^${base_regex}/([^/]+)/([^/]+)/install\.yaml$ ]]; then
		ns="${BASH_REMATCH[1]}"
		seen_ns["${ns}"]=1
	fi
done < <(find "${base}" -type f -name "install.yaml" -print0 2>/dev/null || true)

if [[ "${#seen_ns[@]}" -eq 0 ]]; then
	echo "🧹 No installs found under ${base}"
	exit 0
fi

while IFS= read -r ns; do
	[[ -n "${ns}" ]] && cleanup_ns "${ns}"
done < <(printf '%s\n' "${!seen_ns[@]}" | sort)

"${kubectl_bin}" --context "${ctx}" delete crd \
	gitproviders.configbutler.ai \
	gittargets.configbutler.ai \
	watchrules.configbutler.ai \
	clusterwatchrules.configbutler.ai \
	--ignore-not-found=true || true
