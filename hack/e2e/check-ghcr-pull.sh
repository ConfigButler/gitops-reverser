#!/usr/bin/env bash
set -euo pipefail

# check-ghcr-pull.sh — preflight that the registry can actually serve the OCI
# artifact the e2e Flux bring-up pulls (`flux-operator install` →
# flux-operator-manifests). Run it BEFORE creating the k3d cluster so an
# unreachable/denied registry fails fast with an actionable message instead of
# surfacing deep inside the Ginkgo SynchronizedBeforeSuite as an opaque
# "DENIED: denied" that aborts all specs (0 run).
#
# Two distinct causes hide behind that "DENIED" message; this script tells them
# apart by probing the registry both WITH and WITHOUT the locally-resolved
# Docker credentials:
#
#   1. Registry outage / throttling — ghcr.io refuses the pull for everyone
#      (anonymous AND authenticated). Usually transient; retry later.
#
#   2. A STALE ghcr.io credential in your Docker config. Dev-container images
#      ship a Docker `credsStore` credential helper (see ~/.docker/config.json).
#      It can hand the Flux/oras pull an EXPIRED GitHub token, so ghcr.io returns
#      401/403 for the *authenticated* request even though an *anonymous* pull of
#      this public artifact would succeed. A hand-rolled anonymous `curl` of the
#      token endpoint therefore looks healthy while the real pull dies. The pull
#      uses your Docker credentials, so this preflight must too — hence we
#      replicate the credential resolution below.
#      Fix: `docker logout ghcr.io` drops the stale entry; pulls fall back to
#      anonymous and succeed.
#
# Inputs (env):
# - GHCR_PREFLIGHT_REF (optional): the OCI reference to probe.
#     default: ghcr.io/controlplaneio-fluxcd/flux-operator-manifests:latest
# - DOCKER_CONFIG (optional): Docker config dir; default $HOME/.docker
# - GHCR_PREFLIGHT_AUTO_LOGOUT (optional): when "1" and a stale credential is
#     detected, run `docker logout <registry>` automatically instead of failing.

REF="${GHCR_PREFLIGHT_REF:-ghcr.io/controlplaneio-fluxcd/flux-operator-manifests:latest}"
DOCKER_CONFIG_DIR="${DOCKER_CONFIG:-${HOME}/.docker}"
CONFIG_JSON="${DOCKER_CONFIG_DIR}/config.json"

command -v curl >/dev/null 2>&1 || { echo "❌ curl is required for the ghcr preflight" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "❌ jq is required for the ghcr preflight" >&2; exit 1; }

# Split "<registry>/<repo>:<tag>" into its parts.
registry="${REF%%/*}"          # ghcr.io
remainder="${REF#*/}"          # controlplaneio-fluxcd/flux-operator-manifests:latest
if printf '%s' "${remainder}" | grep -q ':'; then
	repo="${remainder%:*}"     # controlplaneio-fluxcd/flux-operator-manifests
	tag="${remainder##*:}"     # latest
else
	repo="${remainder}"
	tag="latest"
fi

# Resolve the credentials Docker would use for this registry, echoing
# "user:secret" (empty when none). Mirrors Docker's own lookup order: an inline
# `auths` entry first, then a per-registry `credHelpers` entry, then the global
# `credsStore` helper. Helpers are invoked as `docker-credential-<helper> get`
# with the registry on stdin (the same contract the Docker CLI uses).
resolve_registry_credentials() {
	[ -f "${CONFIG_JSON}" ] || return 0

	local auth
	auth="$(jq -r --arg r "${registry}" '.auths[$r].auth // empty' "${CONFIG_JSON}" 2>/dev/null || true)"
	if [ -n "${auth}" ]; then
		printf '%s' "${auth}" | base64 -d 2>/dev/null || true
		return 0
	fi

	local helper
	helper="$(jq -r --arg r "${registry}" '.credHelpers[$r] // .credsStore // empty' "${CONFIG_JSON}" 2>/dev/null || true)"
	if [ -n "${helper}" ] && command -v "docker-credential-${helper}" >/dev/null 2>&1; then
		local out user secret
		out="$(printf '%s' "${registry}" | "docker-credential-${helper}" get 2>/dev/null || true)"
		if [ -n "${out}" ]; then
			user="$(printf '%s' "${out}" | jq -r '.Username // empty' 2>/dev/null || true)"
			secret="$(printf '%s' "${out}" | jq -r '.Secret // empty' 2>/dev/null || true)"
			if [ -n "${secret}" ]; then
				printf '%s:%s' "${user}" "${secret}"
			fi
		fi
	fi
	return 0
}

# Discover the Bearer token realm + service from the registry's /v2/ challenge,
# rather than hard-coding ghcr.io's endpoint, so this works for any registry.
discover_token_realm() {
	local hdrs
	hdrs="$(curl -sS -o /dev/null -D - "https://${registry}/v2/" 2>/dev/null | tr -d '\r' || true)"
	local realm service
	realm="$(printf '%s' "${hdrs}" | sed -nE 's/^[Ww]ww-[Aa]uthenticate:.*realm="([^"]+)".*/\1/p' | head -n1)"
	service="$(printf '%s' "${hdrs}" | sed -nE 's/^[Ww]ww-[Aa]uthenticate:.*service="([^"]+)".*/\1/p' | head -n1)"
	# Fall back to the conventional ghcr/Docker layout if the challenge is absent.
	[ -n "${realm}" ] || realm="https://${registry}/token"
	[ -n "${service}" ] || service="${registry}"
	printf '%s\t%s' "${realm}" "${service}"
}

# HTTP status of the token request. $1 = optional "user:secret" (basic auth).
token_status() {
	local creds="$1"
	local url="${REALM}?service=${SERVICE}&scope=repository:${repo}:pull"
	if [ -n "${creds}" ]; then
		curl -s -o /dev/null -w '%{http_code}' -u "${creds}" "${url}"
	else
		curl -s -o /dev/null -w '%{http_code}' "${url}"
	fi
}

# Confirm the tag actually resolves (catches a moved/renamed/typo'd artifact),
# using whichever credential path just succeeded. $1 = optional "user:secret".
manifest_status() {
	local creds="$1" token url
	url="${REALM}?service=${SERVICE}&scope=repository:${repo}:pull"
	if [ -n "${creds}" ]; then
		token="$(curl -s -u "${creds}" "${url}" | jq -r '.token // .access_token // empty' 2>/dev/null || true)"
	else
		token="$(curl -s "${url}" | jq -r '.token // .access_token // empty' 2>/dev/null || true)"
	fi
	[ -n "${token}" ] || { echo "000"; return 0; }
	curl -s -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer ${token}" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json' \
		"https://${registry}/v2/${repo}/manifests/${tag}"
}

echo "🔎 ghcr preflight: probing ${REF}"

IFS=$'\t' read -r REALM SERVICE <<EOF
$(discover_token_realm)
EOF

creds="$(resolve_registry_credentials || true)"
cred_user="${creds%%:*}"

anon_token_code="$(token_status "")"

if [ -n "${creds}" ]; then
	auth_token_code="$(token_status "${creds}")"

	# Stale-credential signature: the registry rejects our stored credential but
	# would serve the artifact anonymously. The real pull uses the credential, so
	# it WILL fail — fail the preflight now with the one-line fix.
	if printf '%s' "${auth_token_code}" | grep -qE '^(401|403)$' && [ "${anon_token_code}" = "200" ]; then
		echo "❌ ghcr preflight: the Docker credential stored for ${registry} is being REJECTED (HTTP ${auth_token_code})," >&2
		echo "   but an anonymous pull of ${repo} works (HTTP ${anon_token_code})." >&2
		echo "   This is almost always a STALE token handed out by your Docker credsStore" >&2
		echo "   (username '${cred_user:-<none>}'). The Flux/oras pull uses that credential and will fail." >&2
		echo >&2
		echo "   Fix: drop the stale entry so pulls fall back to anonymous —" >&2
		echo "       docker logout ${registry}" >&2
		if [ "${GHCR_PREFLIGHT_AUTO_LOGOUT:-}" = "1" ] && command -v docker >/dev/null 2>&1; then
			echo "   GHCR_PREFLIGHT_AUTO_LOGOUT=1 set — running 'docker logout ${registry}'…" >&2
			docker logout "${registry}" >&2 || true
			anon_token_code="$(token_status "")"
			[ "${anon_token_code}" = "200" ] || { echo "❌ still cannot reach ${registry} anonymously (HTTP ${anon_token_code})" >&2; exit 1; }
			creds=""
		else
			exit 1
		fi
	fi
fi

# Choose the working credential path for the final manifest check.
probe_creds=""
if [ -n "${creds}" ] && [ "${auth_token_code:-}" = "200" ]; then
	probe_creds="${creds}"
elif [ "${anon_token_code}" != "200" ]; then
	echo "❌ ghcr preflight: ${registry} is not serving tokens (anonymous HTTP ${anon_token_code})." >&2
	echo "   Likely a ghcr.io outage or throttling — the pull of ${REF} would fail. Retry later." >&2
	exit 1
fi

mf_code="$(manifest_status "${probe_creds}")"
if [ "${mf_code}" != "200" ]; then
	echo "❌ ghcr preflight: token OK but the manifest ${REF} did not resolve (HTTP ${mf_code})." >&2
	echo "   The artifact may have moved/renamed, or the registry is partially degraded." >&2
	exit 1
fi

echo "✅ ghcr preflight: ${REF} is pullable$( [ -n "${probe_creds}" ] && printf ' (authenticated as %s)' "${cred_user}" || printf ' (anonymous)' )."
