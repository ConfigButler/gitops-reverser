#!/usr/bin/env bash
set -euo pipefail

# Ensure the controller deployment references PROJECT_IMAGE and wait for rollout.
# Called after the install stamp is ready and the image is loaded.
#
# Inputs (env):
# - CTX (required): kube context
# - NAMESPACE (required): namespace containing the deployment
# - PROJECT_IMAGE (required): container image reference to set
# - IMAGE_DELIVERY_MODE (optional): load|pull; defaults to "load"
# - IMAGE_PULL_SECRET_NAME (optional): imagePullSecret to attach when using pull mode
# - IMAGE_PULL_SECRET_REGISTRY (optional): registry hostname for docker-registry secret creation
# - IMAGE_PULL_SECRET_USERNAME (optional): registry username for docker-registry secret creation
# - IMAGE_PULL_SECRET_PASSWORD (optional): registry password/token for docker-registry secret creation
# - CONTROLLER_CONTAINER (required): container name within the deployment
# - CONTROLLER_DEPLOY_SELECTOR (required): label selector to find the deployment

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${PROJECT_IMAGE:?PROJECT_IMAGE is required}"
: "${CONTROLLER_CONTAINER:?CONTROLLER_CONTAINER is required}"
: "${CONTROLLER_DEPLOY_SELECTOR:?CONTROLLER_DEPLOY_SELECTOR is required}"

IMAGE_DELIVERY_MODE="${IMAGE_DELIVERY_MODE:-load}"
IMAGE_PULL_SECRET_NAME="${IMAGE_PULL_SECRET_NAME:-}"
IMAGE_PULL_SECRET_REGISTRY="${IMAGE_PULL_SECRET_REGISTRY:-}"
IMAGE_PULL_SECRET_USERNAME="${IMAGE_PULL_SECRET_USERNAME:-}"
IMAGE_PULL_SECRET_PASSWORD="${IMAGE_PULL_SECRET_PASSWORD:-}"

ensure_image_pull_secret() {
	if [[ -z "${IMAGE_PULL_SECRET_NAME}" ]]; then
		return 0
	fi

	if [[ -n "${IMAGE_PULL_SECRET_REGISTRY}" && -n "${IMAGE_PULL_SECRET_USERNAME}" && -n "${IMAGE_PULL_SECRET_PASSWORD}" ]]; then
		kubectl --context "${CTX}" -n "${NAMESPACE}" \
			create secret docker-registry "${IMAGE_PULL_SECRET_NAME}" \
			--docker-server="${IMAGE_PULL_SECRET_REGISTRY}" \
			--docker-username="${IMAGE_PULL_SECRET_USERNAME}" \
			--docker-password="${IMAGE_PULL_SECRET_PASSWORD}" \
			--dry-run=client -o yaml \
			| kubectl --context "${CTX}" apply -f -
	fi

	kubectl --context "${CTX}" -n "${NAMESPACE}" patch deployment "${deploy}" --type=merge -p "$(cat <<EOF
{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"${IMAGE_PULL_SECRET_NAME}"}]}}}}
EOF
)"
}

count="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
	get deploy -l "${CONTROLLER_DEPLOY_SELECTOR}" --no-headers 2>/dev/null \
	| wc -l | tr -d ' ')"

if [[ "${count}" -ne 1 ]]; then
	echo "ERROR: Expected exactly 1 Deployment matching '${CONTROLLER_DEPLOY_SELECTOR}'" \
		"in namespace '${NAMESPACE}', found ${count}" >&2
	kubectl --context "${CTX}" -n "${NAMESPACE}" \
		get deploy -l "${CONTROLLER_DEPLOY_SELECTOR}" -o wide || true
	exit 1
fi

deploy="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
	get deploy -l "${CONTROLLER_DEPLOY_SELECTOR}" \
	-o jsonpath='{.items[0].metadata.name}')"

if [[ "${IMAGE_DELIVERY_MODE}" == "pull" ]]; then
	ensure_image_pull_secret
	echo "Setting deployment/${deploy} container '${CONTROLLER_CONTAINER}' to image '${PROJECT_IMAGE}'"
	kubectl --context "${CTX}" -n "${NAMESPACE}" \
		set image "deployment/${deploy}" "${CONTROLLER_CONTAINER}=${PROJECT_IMAGE}"
else
	echo "Restarting deployment/${deploy} to pick up loaded image '${PROJECT_IMAGE}'"
	kubectl --context "${CTX}" -n "${NAMESPACE}" \
		rollout restart "deployment/${deploy}"
fi
kubectl --context "${CTX}" -n "${NAMESPACE}" \
	rollout status "deployment/${deploy}" --timeout=180s
