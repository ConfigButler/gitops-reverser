#!/usr/bin/env bash
set -euo pipefail

# Load a container image into the k3d cluster and pin it against containerd GC.
# If the image is missing locally and PROJECT_IMAGE_PROVIDED is set, fails fast.
# If the image is missing locally and it's the local build, triggers a make rebuild.
#
# Why pinning matters
# -------------------
# After k3d image import, the image lives in containerd's k8s.io namespace inside
# each node container. Containerd's CRI plugin runs image GC on behalf of the kubelet:
# when disk pressure occurs, or after an image goes unreferenced (e.g. because the
# deployment was deleted during a clean reinstall), containerd will evict it.
#
# Once evicted, the stamp file still says the image is loaded — because the stamp
# only tracks whether WE loaded it, not whether containerd still has it. On the next
# run Make skips the import and the rollout fails with ImagePullBackOff.
#
# The fix: after import we set the io.cri-containerd.pinned=pinned label on the image
# in every node's containerd. The CRI plugin checks this label before evicting any
# image and skips pinned ones unconditionally. This makes the stamp reliable: if it
# says loaded, containerd still has it.
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
IMAGE_REPO="${PROJECT_IMAGE%:*}"
IMAGE_TAG="${PROJECT_IMAGE##*:}"

if [[ "${IMAGE_REPO}" == "${IMAGE_TAG}" ]]; then
	IMAGE_REPO="${PROJECT_IMAGE}"
	IMAGE_TAG="latest"
fi

# Normalize to the fully-qualified reference containerd uses internally.
# Rules mirror Docker's normalization:
#   no slash            → docker.io/library/IMAGE
#   slash, no dot/colon in first component → docker.io/ORG/IMAGE
#   dot or colon in first component → registry is explicit
containerd_ref() {
	local repo="$1" tag="$2"
	local first="${repo%%/*}"
	if [[ "${repo}" != *"/"* ]]; then
		echo "docker.io/library/${repo}:${tag}"
	elif [[ "${first}" != *"."* && "${first}" != *":"* && "${first}" != "localhost" ]]; then
		echo "docker.io/${repo}:${tag}"
	else
		echo "${repo}:${tag}"
	fi
}

cluster_node_names() {
	"${CONTAINER_TOOL}" ps --format '{{.Names}}' \
		| grep -E "^k3d-${CLUSTER_NAME}-(server|agent)-[0-9]+$" \
		| sort
}

# Pin the image on every node so containerd's GC never evicts it.
# Uses the io.cri-containerd.pinned label, which containerd honours as a GC root.
pin_image() {
	local ref="$1"
	local node_name
	while IFS= read -r node_name; do
		"${CONTAINER_TOOL}" exec "${node_name}" \
			ctr -n k8s.io images label "${ref}" io.cri-containerd.pinned=pinned \
			>/dev/null
	done < <(cluster_node_names)
}

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

if [[ -f "${STAMP_FILE}" ]] && [[ "$(<"${STAMP_FILE}")" == "${img_id}" ]]; then
	echo "${PROJECT_IMAGE} (${img_id}) is already loaded into ${CTX} (stamp matches)"
	exit 0
fi

ref="$(containerd_ref "${IMAGE_REPO}" "${IMAGE_TAG}")"

echo "Loading ${PROJECT_IMAGE} (${img_id}) into ${CTX}"
"${K3D}" image import "${PROJECT_IMAGE}" -c "${CLUSTER_NAME}"

echo "Pinning ${ref} on ${CTX} nodes (prevents containerd GC)"
pin_image "${ref}"

echo "${img_id}" >"${STAMP_FILE}"
