#!/usr/bin/env bash
# Install the kcp control plane into the e2e cluster for the source-cluster corner.
#
# Two phases, because the instance CRs (RootShard/FrontProxy/Kubeconfig) are typed by CRDs
# the kcp-operator HelmRelease installs:
#   1. apply the base (namespace, etcd, PKI Issuer, kcp-operator HelmRepository+HelmRelease),
#      then wait for the operator + its CRDs + etcd;
#   2. apply the instance CRs, then wait for the root shard, the front-proxy, and the admin
#      kubeconfig Secret the operator mints.
#
# kcp-operator is installed BY Flux (a HelmRelease), like every other e2e dependency; this
# script only applies the manifests and waits, it does not imperatively install kcp itself.
#
# Env: CTX (kube context, required). Idempotent — safe to re-run against a warm cluster.
set -euo pipefail

CTX="${CTX:?CTX (kube context) must be set}"
KCP_DIR="${KCP_DIR:-test/e2e/setup/kcp}"
KCP_WAIT_TIMEOUT="${KCP_WAIT_TIMEOUT:-300s}"

kc() { kubectl --context "${CTX}" "$@"; }

echo "⬇️  Applying kcp base (etcd, PKI issuer, kcp-operator HelmRelease)…"
kc apply -k "${KCP_DIR}/base"

echo "⏳ Waiting for the kcp-operator HelmRelease to become Ready…"
if ! kc -n flux-system wait helmrelease/kcp-operator --for=condition=Ready --timeout="${KCP_WAIT_TIMEOUT}"; then
  echo "ERROR: kcp-operator HelmRelease did not become Ready." >&2
  kc -n flux-system get helmrelease kcp-operator -o yaml | sed -n '/status:/,$p' >&2 || true
  kc -n kcp-operator get pods >&2 || true
  exit 1
fi

echo "⏳ Waiting for the kcp-operator CRDs to be Established…"
kc wait --for=condition=Established --timeout=120s \
  crd/rootshards.operator.kcp.io \
  crd/frontproxies.operator.kcp.io \
  crd/kubeconfigs.operator.kcp.io

echo "⏳ Waiting for etcd…"
kc -n kcp rollout status statefulset/etcd --timeout=120s

echo "⬇️  Applying the kcp instance (RootShard / FrontProxy / admin Kubeconfig)…"
kc apply -f "${KCP_DIR}/instance.yaml"

echo "⏳ Waiting for the root shard and front-proxy Deployments…"
# The operator creates the Deployments a few seconds after the CRs are accepted; poll until
# they exist, then let rollout status block on availability.
for dep in root-kcp frontproxy-front-proxy; do
  for _ in $(seq 1 40); do
    kc -n kcp get deployment "${dep}" >/dev/null 2>&1 && break
    sleep 3
  done
  kc -n kcp rollout status "deployment/${dep}" --timeout="${KCP_WAIT_TIMEOUT}"
done

echo "⏳ Waiting for the admin kubeconfig Secret (kcp/kcp-admin-kubeconfig)…"
for _ in $(seq 1 40); do
  kc -n kcp get secret kcp-admin-kubeconfig >/dev/null 2>&1 && break
  sleep 3
done
kc -n kcp get secret kcp-admin-kubeconfig >/dev/null 2>&1 || {
  echo "ERROR: kcp admin kubeconfig Secret was never minted." >&2
  kc -n kcp get kubeconfig admin -o yaml | sed -n '/status:/,$p' >&2 || true
  exit 1
}

echo "✅ kcp control plane is ready (front-proxy: frontproxy-front-proxy.kcp.svc.cluster.local:6443)."
