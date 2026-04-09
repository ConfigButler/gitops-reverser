#!/usr/bin/env bash
set -euo pipefail

# Load a container image into the k3d cluster.
# If the image is missing locally and PROJECT_IMAGE_PROVIDED is set, fails fast.
# If the image is missing locally and it's the local build, triggers a make rebuild.
#
# Inputs (env):
# - CTX (required): kube context (k3d-<cluster>)
# - CLUSTER_NAME (required): k3d cluster name (without k3d- prefix)
# - PROJECT_IMAGE (required): image reference to load
# - PROJECT_IMAGE_PROVIDED (optional): non-empty means image came from outside; empty = local build
# - CONTROLLER_ID_STAMP (required): make stamp target to rebuild if the local image is missing
# - CONTAINER_TOOL (optional): container tool binary; defaults to "docker"
# - K3D (optional): k3d binary; defaults to "k3d"
# - STAMP_FILE (required): path to write the loaded image ID

: "${CTX:?CTX is required}"
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${PROJECT_IMAGE:?PROJECT_IMAGE is required}"
: "${CONTROLLER_ID_STAMP:?CONTROLLER_ID_STAMP is required}"
: "${STAMP_FILE:?STAMP_FILE is required}"

CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
K3D="${K3D:-k3d}"

if ! "${CONTAINER_TOOL}" image inspect "${PROJECT_IMAGE}" >/dev/null 2>&1; then
	if [[ -z "${PROJECT_IMAGE_PROVIDED:-}" ]]; then
		echo "Local image ${PROJECT_IMAGE} missing; rebuilding..."
		make "${CONTROLLER_ID_STAMP}"
	else
		echo "ERROR: PROJECT_IMAGE=${PROJECT_IMAGE} not found locally" >&2
		exit 2
	fi
fi

img_id="$("${CONTAINER_TOOL}" inspect --format='{{.Id}}' "${PROJECT_IMAGE}")"
echo "Loading ${PROJECT_IMAGE} (${img_id}) into ${CTX}"
"${K3D}" image import "${PROJECT_IMAGE}" -c "${CLUSTER_NAME}"
echo "${img_id}" >"${STAMP_FILE}"
