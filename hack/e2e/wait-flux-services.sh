#!/usr/bin/env bash
set -euo pipefail

# Wait for all Flux-managed HelmReleases and Kustomizations to reach Ready.
# Skips suspended Kustomizations and known demo-only namespaces.
#
# Inputs (env):
# - CTX (required): kube context
# - FLUX_SERVICES_WAIT_TIMEOUT (optional): per-resource wait timeout; default 120s

: "${CTX:?CTX is required}"
FLUX_SERVICES_WAIT_TIMEOUT="${FLUX_SERVICES_WAIT_TIMEOUT:-120s}"

# On a wait timeout we otherwise get a single opaque "timed out" line and have to
# guess. This dumps the resource's conditions, the source it pulls from
# (OCIRepository/HelmRepository), the workloads/events in its target namespace and
# recent Flux controller logs so the *cause* is visible in the CI log. Best-effort:
# every command is guarded so diagnostics never mask the original failure.
dump_diagnostics() {
	local kind="$1" namespace="$2" name="$3"

	echo "::group::diagnostics ${kind}/${name} (ns=${namespace})" >&2
	echo "----- ${kind}/${name} conditions -----" >&2
	kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" -o wide >&2 2>&1 || true
	kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" \
		-o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason} msg={.message}{"\n"}{end}' >&2 2>&1 || true

	if [[ "${kind}" == helmreleases.* ]]; then
		# Resolve the chart source (OCIRepository/HelmRepository) and its readiness.
		local src_kind src_name src_ns target_ns
		src_kind="$(kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" -o jsonpath='{.spec.chartRef.kind}' 2>/dev/null || true)"
		src_name="$(kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" -o jsonpath='{.spec.chartRef.name}' 2>/dev/null || true)"
		src_ns="$(kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" -o jsonpath='{.spec.chartRef.namespace}' 2>/dev/null || true)"
		src_ns="${src_ns:-${namespace}}"
		if [[ -n "${src_kind}" && -n "${src_name}" ]]; then
			echo "----- source ${src_kind}/${src_name} (ns=${src_ns}) -----" >&2
			kubectl --context "${CTX}" -n "${src_ns}" get "${src_kind}" "${src_name}" -o wide >&2 2>&1 || true
			kubectl --context "${CTX}" -n "${src_ns}" get "${src_kind}" "${src_name}" \
				-o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason} msg={.message}{"\n"}{end}' >&2 2>&1 || true
		fi

		target_ns="$(kubectl --context "${CTX}" -n "${namespace}" get "${kind}/${name}" -o jsonpath='{.spec.targetNamespace}' 2>/dev/null || true)"
		target_ns="${target_ns:-${namespace}}"
		echo "----- pods in target namespace ${target_ns} -----" >&2
		kubectl --context "${CTX}" -n "${target_ns}" get pods -o wide >&2 2>&1 || true
		echo "----- recent events in ${target_ns} -----" >&2
		{ kubectl --context "${CTX}" -n "${target_ns}" get events --sort-by=.lastTimestamp 2>&1 | tail -n 30; } >&2 || true
	fi

	echo "----- recent flux controller logs (source/helm/kustomize) -----" >&2
	for ctrl in source-controller helm-controller kustomize-controller; do
		echo "--- ${ctrl} ---" >&2
		kubectl --context "${CTX}" -n flux-system logs "deploy/${ctrl}" --tail=40 >&2 2>&1 || true
	done
	echo "::endgroup::" >&2
}

flux_ready_count=0
echo "⏳ Waiting for Flux-managed installations to become ready..."

for kind in \
	helmreleases.helm.toolkit.fluxcd.io \
	kustomizations.kustomize.toolkit.fluxcd.io
do
	if [[ "${kind}" == "kustomizations.kustomize.toolkit.fluxcd.io" ]]; then
		resources="$(kubectl --context "${CTX}" get "${kind}" --all-namespaces \
			-o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SUSPEND:.spec.suspend \
			--no-headers 2>/dev/null \
			| awk '$3 != "true" && $1 != "podinfos-preview" && $1 != "podinfos-production" && $1 != "podinfos-intent" {print $1 " " $2}')"
	else
		resources="$(kubectl --context "${CTX}" get "${kind}" --all-namespaces \
			-o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
			2>/dev/null)"
	fi

	[[ -z "${resources}" ]] && continue

	resource_count="$(printf '%s\n' "${resources}" | sed '/^$/d' | wc -l | tr -d ' ')"
	flux_ready_count="$((flux_ready_count + resource_count))"

	while IFS=' ' read -r namespace name; do
		[[ -n "${namespace}" ]] || continue
		if ! kubectl --context "${CTX}" -n "${namespace}" \
			wait "${kind}/${name}" \
			--for=condition=Ready \
			--timeout="${FLUX_SERVICES_WAIT_TIMEOUT}"; then
			echo "ERROR: ${kind}/${name} (ns=${namespace}) did not become Ready within ${FLUX_SERVICES_WAIT_TIMEOUT}" >&2
			dump_diagnostics "${kind}" "${namespace}" "${name}"
			exit 1
		fi
	done <<<"${resources}"
done

if [[ "${flux_ready_count}" -le 0 ]]; then
	echo "ERROR: no Flux-managed e2e ready-check resources found" >&2
	exit 1
fi

echo "✓ Flux-managed installations ready: ${flux_ready_count}"
