#!/usr/bin/env bash
set -euo pipefail

# Show recent logs from the voter GitOps auth-service and highlight the demo access code.
#
# Inputs:
# - CTX, E2E_KUBECONTEXT, or KUBECONTEXT: kube context; defaults to current context
# - VOTER_NAMESPACE: namespace to read from; default voter-production
# - VOTER_AUTH_DEPLOYMENT: auth deployment name; default voter-auth-service
# - TAIL: number of log lines; default 80
#
# Flags:
# - --code-only: print only the latest demo access code
# - --follow: follow logs after printing the recent tail

KUBE_CONTEXT="${E2E_KUBECONTEXT:-${CTX:-${KUBECONTEXT:-}}}"
VOTER_NAMESPACE="${VOTER_NAMESPACE:-voter-production}"
VOTER_AUTH_DEPLOYMENT="${VOTER_AUTH_DEPLOYMENT:-voter-auth-service}"
TAIL="${TAIL:-80}"
CODE_ONLY=false
FOLLOW=false

usage() {
	cat <<EOF
Usage: $0 [--code-only] [--follow]

Environment:
  CTX / E2E_KUBECONTEXT / KUBECONTEXT  Kubernetes context
  VOTER_NAMESPACE                      Namespace, default: voter-production
  VOTER_AUTH_DEPLOYMENT                Deployment, default: voter-auth-service
  TAIL                                 Log lines, default: 80
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--code-only)
			CODE_ONLY=true
			shift
			;;
		--follow|-f)
			FOLLOW=true
			shift
			;;
		--help|-h)
			usage
			exit 0
			;;
		*)
			echo "ERROR: unknown argument: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [[ -z "${KUBE_CONTEXT}" ]]; then
	KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
fi

if [[ -z "${KUBE_CONTEXT}" ]]; then
	echo "ERROR: Kubernetes context is required (set E2E_KUBECONTEXT, CTX, or KUBECONTEXT)" >&2
	exit 1
fi

logs="$(
	kubectl --context "${KUBE_CONTEXT}" \
		-n "${VOTER_NAMESPACE}" \
		logs "deploy/${VOTER_AUTH_DEPLOYMENT}" \
		--tail="${TAIL}"
)"

code="$(
	printf '%s\n' "${logs}" \
		| sed -n 's/.*demo-access-code: code=\([^ ]*\).*/\1/p' \
		| tail -n 1
)"

if [[ "${CODE_ONLY}" == "true" ]]; then
	if [[ -z "${code}" ]]; then
		echo "ERROR: no demo-access-code line found in the last ${TAIL} log lines" >&2
		exit 1
	fi
	printf '%s\n' "${code}"
	exit 0
fi

echo "Context: ${KUBE_CONTEXT}"
echo "Namespace: ${VOTER_NAMESPACE}"
echo "Deployment: ${VOTER_AUTH_DEPLOYMENT}"
if [[ -n "${code}" ]]; then
	echo "Latest demo access code: ${code}"
else
	echo "Latest demo access code: not found in last ${TAIL} lines"
fi
echo
printf '%s\n' "${logs}"

if [[ "${FOLLOW}" == "true" ]]; then
	echo
	echo "Following logs..."
	kubectl --context "${KUBE_CONTEXT}" \
		-n "${VOTER_NAMESPACE}" \
		logs "deploy/${VOTER_AUTH_DEPLOYMENT}" \
		--tail=0 \
		--follow
fi
