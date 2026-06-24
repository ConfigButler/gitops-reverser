#!/usr/bin/env bash
set -euo pipefail

# Swap the deployed gitops-reverser controller to the mutation-capture lab image.
#
# The lab deliberately reuses the product's audit + admission webhook wiring (same
# URLs, same TLS cert mounts), so making the cluster capture with the lab instead
# of the controller is just this: import the lab image and patch the existing
# Deployment's image + entrypoint + args. No new audit policy, webhook config, or
# certificates are introduced — that is the whole point of the swap.
#
# Inputs (env):
# - CTX (required): kube context (k3d-<cluster>)
# - CLUSTER_NAME (required): k3d cluster name (without the k3d- prefix)
# - NAMESPACE (required): controller namespace
# - LAB_IMAGE (required): lab image reference to import and run
# - K3D / KUBECTL (optional): tool overrides
# - CONTROLLER_DEPLOY_SELECTOR (optional): label selector for the controller Deployment
# - CONTROLLER_CONTAINER (optional): container name to patch (default: manager)

: "${CTX:?CTX is required}"
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${LAB_IMAGE:?LAB_IMAGE is required}"

K3D="${K3D:-k3d}"
KUBECTL="${KUBECTL:-kubectl}"
SELECTOR="${CONTROLLER_DEPLOY_SELECTOR:-app.kubernetes.io/part-of=gitops-reverser}"
CONTAINER="${CONTROLLER_CONTAINER:-manager}"

echo "Importing ${LAB_IMAGE} into k3d cluster ${CLUSTER_NAME}…"
"${K3D}" image import "${LAB_IMAGE}" -c "${CLUSTER_NAME}"

deploy="$("${KUBECTL}" --context "${CTX}" -n "${NAMESPACE}" get deploy -l "${SELECTOR}" -o name | head -n1)"
[ -n "${deploy}" ] || { echo "no controller Deployment matching ${SELECTOR} in ${NAMESPACE}" >&2; exit 1; }
echo "Patching ${deploy} container '${CONTAINER}' to the lab image…"

# Strategic-merge patch: container list is keyed by name, command/args replace
# wholesale. The cert mounts and the /readyz:8081 readiness probe are inherited
# from the product Deployment unchanged.
read -r -d '' patch <<YAML || true
spec:
  template:
    spec:
      containers:
      - name: ${CONTAINER}
        image: ${LAB_IMAGE}
        imagePullPolicy: IfNotPresent
        command: ["/mutation-capture-lab"]
        args:
        - --admission-addr=:9443
        - --admission-cert-dir=/tmp/k8s-webhook-server/serving-certs
        - --audit-addr=:9444
        - --audit-cert-dir=/tmp/k8s-audit-server/audit-server-certs
        - --api-addr=:8081
        # M1 configmaps + M2 workload types: Deployments for the status/scale
        # subresource rows (5, 6) and Pods for the graceful-delete row (7).
        - --watch-resources=v1/configmaps,apps/v1/deployments,v1/pods
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
YAML

"${KUBECTL}" --context "${CTX}" -n "${NAMESPACE}" patch "${deploy}" \
  --type=strategic --patch "${patch}"

# The image tag is a content hash (see the Taskfile), so a real code change moves
# the tag, the patch above changes the deployment's image ref, and the rollout
# happens on its own. An unchanged source keeps the same tag and the running pod,
# so this is a clean no-op rather than a needless restart.
echo "Waiting for the lab rollout…"
"${KUBECTL}" --context "${CTX}" -n "${NAMESPACE}" rollout status "${deploy}" --timeout=180s
echo "Lab image is serving the audit + admission webhooks."
