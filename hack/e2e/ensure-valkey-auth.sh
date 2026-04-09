#!/usr/bin/env bash
set -euo pipefail

# Ensure the gitops-reverser namespace and valkey-auth Secret exist before
# helm install runs. Both operations are idempotent (dry-run + apply).
#
# Inputs (env):
# - CTX (required): kube context
# - NAMESPACE (required): namespace to create and populate
# - E2E_VALKEY_PASSWORD (required): password to put in the Secret

: "${CTX:?CTX is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${E2E_VALKEY_PASSWORD:?E2E_VALKEY_PASSWORD is required}"

kubectl --context "${CTX}" create namespace "${NAMESPACE}" \
	--dry-run=client -o yaml \
	| kubectl --context "${CTX}" apply -f -

kubectl --context "${CTX}" -n "${NAMESPACE}" \
	create secret generic valkey-auth \
	--from-literal=password="${E2E_VALKEY_PASSWORD}" \
	--dry-run=client -o yaml \
	| kubectl --context "${CTX}" apply -f -
