#!/usr/bin/env bash
set -euo pipefail

# Render and apply the controller via kustomize config-dir mode.
# Writes the rendered manifest to STAMP_FILE on success.
#
# Inputs (env):
# - CTX (required): kube context
# - NAMESPACE (required): target namespace
# - PROJECT_IMAGE (required): controller image to inject
# - KUSTOMIZE (optional): kustomize binary; defaults to "kustomize"
# - KUBECTL (optional): kubectl binary; defaults to "kubectl"
# - STAMP_FILE (required): path to write the rendered install.yaml

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${PROJECT_IMAGE:?PROJECT_IMAGE is required}"
: "${STAMP_FILE:?STAMP_FILE is required}"

KUSTOMIZE="${KUSTOMIZE:-kustomize}"
KUBECTL="${KUBECTL:-kubectl}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

cp -R config "${tmpdir}/config"
(cd "${tmpdir}/config" && "${KUSTOMIZE}" edit set namespace "${NAMESPACE}")
(cd "${tmpdir}/config" && "${KUSTOMIZE}" edit set image gitops-reverser="${PROJECT_IMAGE}")

(cd "${tmpdir}/config" && "${KUSTOMIZE}" build .) \
	| tee "${tmpdir}/install.yaml" \
	| "${KUBECTL}" --context "${CTX}" apply -f -

# Replace stamp only when content changed (keeps mtime stable on no-op runs).
if [[ -f "${STAMP_FILE}" ]] && cmp -s "${tmpdir}/install.yaml" "${STAMP_FILE}"; then
	: # no change
else
	mv "${tmpdir}/install.yaml" "${STAMP_FILE}"
fi
