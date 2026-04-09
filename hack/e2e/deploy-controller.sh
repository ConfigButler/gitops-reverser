#!/usr/bin/env bash
set -euo pipefail

# Update the controller deployment image and wait for rollout.
# Called after the install stamp is ready and the image is loaded.
#
# Inputs (env):
# - CTX (required): kube context
# - NAMESPACE (required): namespace containing the deployment
# - PROJECT_IMAGE (required): container image reference to set
# - CONTROLLER_CONTAINER (required): container name within the deployment
# - CONTROLLER_DEPLOY_SELECTOR (required): label selector to find the deployment

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${PROJECT_IMAGE:?PROJECT_IMAGE is required}"
: "${CONTROLLER_CONTAINER:?CONTROLLER_CONTAINER is required}"
: "${CONTROLLER_DEPLOY_SELECTOR:?CONTROLLER_DEPLOY_SELECTOR is required}"

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

current_image="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
	get deploy "${deploy}" \
	-o jsonpath="{.spec.template.spec.containers[?(@.name==\"${CONTROLLER_CONTAINER}\")].image}")"
desired_replicas="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
	get deploy "${deploy}" -o jsonpath='{.spec.replicas}')"
available_replicas="$(kubectl --context "${CTX}" -n "${NAMESPACE}" \
	get deploy "${deploy}" -o jsonpath='{.status.availableReplicas}')"

if [[ "${current_image}" == "${PROJECT_IMAGE}" ]] \
	&& [[ -n "${available_replicas}" ]] \
	&& [[ "${available_replicas}" == "${desired_replicas}" ]]; then
	echo "deployment/${deploy} already running ${PROJECT_IMAGE} (${available_replicas}/${desired_replicas} available); skipping"
	exit 0
fi

echo "Setting deployment/${deploy} container '${CONTROLLER_CONTAINER}' to image '${PROJECT_IMAGE}'"
kubectl --context "${CTX}" -n "${NAMESPACE}" \
	set image "deployment/${deploy}" "${CONTROLLER_CONTAINER}=${PROJECT_IMAGE}"
kubectl --context "${CTX}" -n "${NAMESPACE}" \
	rollout status "deployment/${deploy}" --timeout=180s
