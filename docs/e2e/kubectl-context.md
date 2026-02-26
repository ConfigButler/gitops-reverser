We will preserve the user chosen kubectl context: but inside the the tests we will everywhere switch to the right context.

Make it an environment contract: Make exports either KUBECONFIG and/or a “ready-to-append” CTX (or KUBE_CONTEXT) and every script uses that consistently.

Option A (my go-to): export KUBECONFIG + CTX from Make

Makefile:

KUBECONFIG ?=
KUBE_CONTEXT ?=

export KUBECONFIG
export CTX := $(if $(KUBE_CONTEXT),--context $(KUBE_CONTEXT),)

.PHONY: e2e
e2e:
	./hack/run-e2e.sh

Script(s):

#!/usr/bin/env bash
set -euo pipefail

# CTX is either empty or "--context name"
kubectl ${CTX:-} get ns
kubectl ${CTX:-} -n kube-system get pods

Usage:

make e2e
KUBE_CONTEXT=prod make e2e
KUBECONFIG=./kubeconfigs/prod.yaml make e2e
KUBECONFIG=./kubeconfigs/prod.yaml KUBE_CONTEXT=prod make e2e

This works because Make exports env vars to subprocesses, and your scripts just append ${CTX:-}.

Option B: export KUBE_CONTEXT and have scripts add --context

Makefile:

export KUBECONFIG ?=
export KUBE_CONTEXT ?=

Script:

kubectl_ctx=()
if [[ -n "${KUBE_CONTEXT:-}" ]]; then
  kubectl_ctx+=(--context "$KUBE_CONTEXT")
fi

kubectl "${kubectl_ctx[@]}" get ns

This is more robust for quoting (contexts with weird chars/spaces), and avoids word-splitting issues.

I’d pick B if you can touch scripts

Because CTX="--context foo" relies on correct splitting. It’s usually fine (context names almost never have spaces), but arrays are bulletproof.

Tiny helper to keep scripts clean

Drop this in hack/kubectl.sh and source it from scripts:

kubectl_cmd() {
  local args=()
  [[ -n "${KUBECONFIG:-}" ]] && export KUBECONFIG
  [[ -n "${KUBE_CONTEXT:-}" ]] && args+=(--context "$KUBE_CONTEXT")
  command kubectl "${args[@]}" "$@"
}

Then in scripts:

source "$(dirname "$0")/kubectl.sh"
kubectl_cmd get ns
kubectl_cmd -n default get pods

If you show one of your bash scripts (how it currently calls kubectl, and whether it already reads env), I’ll adapt this to your exact style (and keep it consistent with your existing $(CTX) usage).