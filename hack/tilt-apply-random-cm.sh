#!/usr/bin/env bash
set -euo pipefail

# Create a single random ConfigMap to smoke-test the watch/commit pipeline.
#
# Namespace: arg 1, else $TILT_CONFIGMAP_NAMESPACE, else "tilt-playground".
# The namespace is expected to already exist.

ns="${1:-${TILT_CONFIGMAP_NAMESPACE:-tilt-playground}}"

name="tilt-smoke-$(date +%s)-${RANDOM}"
value="tilt-$(date -u +%Y%m%dT%H%M%SZ)-${RANDOM}"

kubectl -n "${ns}" create configmap "${name}" \
	--from-literal=key="${value}" \
	--from-literal=createdBy=tilt \
	--from-literal=createdAt="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "Created ConfigMap: ${ns}/${name}"
echo "Value: ${value}"
