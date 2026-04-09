#!/usr/bin/env bash
set -euo pipefail

# Render and apply the controller via the pre-built dist/install.yaml (plain-manifests-file mode).
# Patches the Redis address and injects the e2e ClusterIP, then applies via kustomize namespace overlay.
# Writes the rendered manifest to STAMP_FILE on success.
#
# Inputs (env):
# - CTX (required): kube context
# - NAMESPACE (required): target namespace
# - DEFAULT_AUDIT_REDIS_ADDR (required): Redis address baked into dist/install.yaml
# - E2E_AUDIT_REDIS_ADDR (required): Redis address to substitute in for e2e
# - E2E_CONTROLLER_SERVICE_CLUSTER_IP (required): ClusterIP to inject for the controller Service
# - KUSTOMIZE (optional): kustomize binary; defaults to "kustomize"
# - KUBECTL (optional): kubectl binary; defaults to "kubectl"
# - STAMP_FILE (required): path to write the rendered install.yaml

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${DEFAULT_AUDIT_REDIS_ADDR:?DEFAULT_AUDIT_REDIS_ADDR is required}"
: "${E2E_AUDIT_REDIS_ADDR:?E2E_AUDIT_REDIS_ADDR is required}"
: "${E2E_CONTROLLER_SERVICE_CLUSTER_IP:?E2E_CONTROLLER_SERVICE_CLUSTER_IP is required}"
: "${STAMP_FILE:?STAMP_FILE is required}"

KUSTOMIZE="${KUSTOMIZE:-kustomize}"
KUBECTL="${KUBECTL:-kubectl}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

cp dist/install.yaml "${tmpdir}/install.yaml"

# Patch Redis address
sed -i \
	"s|--audit-redis-addr=${DEFAULT_AUDIT_REDIS_ADDR}|--audit-redis-addr=${E2E_AUDIT_REDIS_ADDR}|" \
	"${tmpdir}/install.yaml"

# Inject fixed ClusterIP for the controller Service
perl -0pi -e \
	"s/type: ClusterIP\n/type: ClusterIP\n  clusterIP: ${E2E_CONTROLLER_SERVICE_CLUSTER_IP}\n/" \
	"${tmpdir}/install.yaml"

# Wrap in a kustomize overlay to set the target namespace
cat >"${tmpdir}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- install.yaml
EOF

(
	cd "${tmpdir}"
	"${KUSTOMIZE}" edit set namespace "${NAMESPACE}" >/dev/null
	"${KUSTOMIZE}" build .
) | tee "${tmpdir}/rendered-install.yaml" \
	| "${KUBECTL}" --context "${CTX}" apply -f -

# Replace stamp only when content changed (keeps mtime stable on no-op runs).
if [[ -f "${STAMP_FILE}" ]] && cmp -s "${tmpdir}/rendered-install.yaml" "${STAMP_FILE}"; then
	: # no change
else
	mv "${tmpdir}/rendered-install.yaml" "${STAMP_FILE}"
fi
