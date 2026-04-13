#!/usr/bin/env bash
set -euo pipefail

# Load a container image into the k3d cluster and pin it against containerd GC.
# If the image is missing locally and PROJECT_IMAGE_PROVIDED is set, fails fast.
# If the image is missing locally and it's the local build, triggers a rebuild task.
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
# run the orchestration layer skips the import and the rollout fails with ImagePullBackOff.
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
# - IMAGE_DELIVERY_MODE (optional): load|pull; defaults to "load"
# - CONTROLLER_ID_TASK (required): Task target used to rebuild when the local image is missing
# - TASK_BIN (optional): task binary; defaults to "task"
# - CONTAINER_TOOL (optional): container tool binary; defaults to "docker"
# - K3D (optional): k3d binary; defaults to "k3d"
# - STAMP_FILE (required): path to write the loaded image reference and ID

: "${CTX:?CTX is required}"
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${PROJECT_IMAGE:?PROJECT_IMAGE is required}"
: "${STAMP_FILE:?STAMP_FILE is required}"

CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
K3D="${K3D:-k3d}"
TASK_BIN="${TASK_BIN:-task}"
IMAGE_DELIVERY_MODE="${IMAGE_DELIVERY_MODE:-load}"
IMAGE_REPO="${PROJECT_IMAGE%:*}"
IMAGE_TAG="${PROJECT_IMAGE##*:}"

if [[ -z "${CONTROLLER_ID_TASK:-}" ]]; then
	echo "ERROR: CONTROLLER_ID_TASK is required" >&2
	exit 2
fi

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

node_image_refs() {
	local node_name="$1"
	"${CONTAINER_TOOL}" exec "${node_name}" ctr -n k8s.io images ls -q 2>/dev/null || true
}

node_images_table() {
	local node_name="$1"
	"${CONTAINER_TOOL}" exec "${node_name}" ctr -n k8s.io images ls 2>/dev/null || true
}

find_pin_refs() {
	local node_name="$1"
	local normalized_ref="$2"
	local raw_ref="$3"
	local repo="$4"
	local tag="$5"

	node_image_refs "${node_name}" | awk \
		-v normalized_ref="${normalized_ref}" \
		-v raw_ref="${raw_ref}" \
		-v repo="${repo}" \
		-v tag="${tag}" '
			$0 == normalized_ref || $0 == raw_ref { print; next }
			index($0, repo ":") == 1 && $0 ~ (":" tag "$") { print; next }
			index($0, "/" repo ":") > 0 && $0 ~ (":" tag "$") { print; next }
		' | sort -u
}

pin_imported_image() {
	local node_name="$1"
	local normalized_ref="$2"
	local raw_ref="$3"
	local repo="$4"
	local tag="$5"
	local attempt refs ref

	for attempt in $(seq 1 10); do
		refs="$(find_pin_refs "${node_name}" "${normalized_ref}" "${raw_ref}" "${repo}" "${tag}")"
		if [[ -n "${refs}" ]]; then
			while IFS= read -r ref; do
				[[ -n "${ref}" ]] || continue
				"${CONTAINER_TOOL}" exec "${node_name}" \
					ctr -n k8s.io images label "${ref}" io.cri-containerd.pinned=pinned \
					>/dev/null
			done <<<"${refs}"
			return 0
		fi
		sleep 1
	done

	echo "ERROR: imported image ref for ${raw_ref} not found in ${node_name} after import" >&2
	echo "Known refs in ${node_name}:" >&2
	node_image_refs "${node_name}" >&2
	return 1
}

image_manifest_digest_in_node() {
	local node_name="$1"
	local normalized_ref="$2"
	local raw_ref="$3"

	node_images_table "${node_name}" | awk \
		-v normalized_ref="${normalized_ref}" \
		-v raw_ref="${raw_ref}" '
			NR > 1 && ($1 == normalized_ref || $1 == raw_ref) {
				print $3
				exit
			}
		'
}

runtime_image_id_in_node() {
	local node_name="$1"
	local manifest_digest="$2"

	node_images_table "${node_name}" | awk \
		-v manifest_digest="${manifest_digest}" '
			NR > 1 && $1 ~ /^sha256:/ && $3 == manifest_digest {
				print $1
				exit
			}
		'
}

import_image() {
	local ref="$1"
	echo "Importing ${PROJECT_IMAGE} into k3d cluster ${CLUSTER_NAME}"
	"${K3D}" image import -c "${CLUSTER_NAME}" "${PROJECT_IMAGE}"
	local node_name
	while IFS= read -r node_name; do
		pin_imported_image "${node_name}" "${ref}" "${PROJECT_IMAGE}" "${IMAGE_REPO}" "${IMAGE_TAG}"
	done < <(cluster_node_names)
}

if [[ "${IMAGE_DELIVERY_MODE}" == "pull" ]]; then
	echo "Skipping k3d image import for ${PROJECT_IMAGE}; cluster will pull from registry"
	echo "pull:${PROJECT_IMAGE}" >"${STAMP_FILE}"
	exit 0
fi

if ! "${CONTAINER_TOOL}" image inspect "${PROJECT_IMAGE}" >/dev/null 2>&1; then
	if [[ -z "${PROJECT_IMAGE_PROVIDED:-}" ]]; then
		echo "Local image ${PROJECT_IMAGE} missing; rebuilding..."
		"${TASK_BIN}" --force "${CONTROLLER_ID_TASK}"
	else
		echo "ERROR: PROJECT_IMAGE=${PROJECT_IMAGE} not found locally" >&2
		exit 2
	fi
fi

if ! "${CONTAINER_TOOL}" image inspect "${PROJECT_IMAGE}" >/dev/null 2>&1; then
	echo "ERROR: local image ${PROJECT_IMAGE} is still missing after rebuild" >&2
	exit 2
fi

img_id="$("${CONTAINER_TOOL}" inspect --format='{{.Id}}' "${PROJECT_IMAGE}")"
ref="$(containerd_ref "${IMAGE_REPO}" "${IMAGE_TAG}")"
first_node="$(cluster_node_names | head -n1 || true)"
cluster_manifest_digest=""
runtime_image_id=""

if [[ -n "${first_node}" ]]; then
	cluster_manifest_digest="$(image_manifest_digest_in_node "${first_node}" "${ref}" "${PROJECT_IMAGE}")"
	if [[ -n "${cluster_manifest_digest}" ]]; then
		runtime_image_id="$(runtime_image_id_in_node "${first_node}" "${cluster_manifest_digest}")"
	fi
fi

stamp_value="${PROJECT_IMAGE}@${runtime_image_id:-${img_id}}"

if [[ -n "${cluster_manifest_digest}" ]] \
	&& [[ "${cluster_manifest_digest}" == "${img_id}" ]] \
	&& [[ -n "${runtime_image_id}" ]] \
	&& [[ -f "${STAMP_FILE}" ]] \
	&& [[ "$(<"${STAMP_FILE}")" == "${stamp_value}" ]]; then
	echo "${PROJECT_IMAGE} (${img_id}) is already loaded into ${CTX} (stamp matches)"
	exit 0
fi

echo "Loading ${PROJECT_IMAGE} (${img_id}) into ${CTX}"
import_image "${ref}"

first_node="$(cluster_node_names | head -n1 || true)"
if [[ -n "${first_node}" ]]; then
	cluster_manifest_digest="$(image_manifest_digest_in_node "${first_node}" "${ref}" "${PROJECT_IMAGE}")"
	if [[ -n "${cluster_manifest_digest}" ]]; then
		runtime_image_id="$(runtime_image_id_in_node "${first_node}" "${cluster_manifest_digest}")"
	fi
fi

stamp_value="${PROJECT_IMAGE}@${runtime_image_id:-${img_id}}"
echo "${stamp_value}" >"${STAMP_FILE}"
