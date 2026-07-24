# Attribution Setup Guide

By default GitOps Reverser runs **configured-author**: every mirrored commit is authored by the single
configured committer identity. **Attribution** turns on a second identity — the actual Kubernetes
user or service account that made the change becomes the commit *author*, while the committer stays
constant. Git carries both.

Once attribution is on, a change whose actor cannot be resolved is authored
`unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>` rather than falling
back to the committer. Seeing that identity in your history means attribution ran and did not find a
matching audit fact — check the audit webhook path before assuming it is normal. Its rate is
`commits_total{author_kind="unresolved"}`.

Attribution is the only optional capability, and it is a real operational step up: it requires
kube-apiserver audit delivery and a Redis/Valkey backing store. This guide covers what that costs
and how the pieces fit. It assumes GitOps Reverser is already installed and mirroring state — see
the [root README quick start](../README.md#quick-start) for that first run.

## The two modes

Every install shares one base — Kubernetes watch/RBAC access, Git credentials, and cert-manager. The
only thing that varies is who shows up as the commit author:

| Mode | Git author | Git committer | Also needs |
|---|---|---|---|
| **`configured-author`** *(default)* | the configured identity | the configured identity | nothing |
| **`attributed-author`** | the authenticated Kubernetes actor | the configured identity | audit delivery + Valkey/Redis |

With `attribution.enabled`, only the **author** column moves:

| Who made the change (Kubernetes actor) | Git author | Git committer |
|---|---|---|
| Human user (`simon@example.com`) | Simon | `ConfigButler Bot` |
| Service account (`system:serviceaccount:team-a:deployer`) | the `team-a/deployer` service account | `ConfigButler Bot` |
| CI / GitHub App identity | that CI / App identity | `ConfigButler Bot` |
| No usable audit fact for a live change | `unknown (attribution unresolved)` | `ConfigButler Bot` |

The committer column never moves. That is the point.

## When it fits

Attribution works by correlating kube-apiserver **audit events** with the objects the operator sees
on **watch**. That means the apiserver has to deliver audit events to the controller over a webhook,
which only self-managed control planes expose:

- **Supported:** clusters where you control apiserver flags — k3s, k3d, Talos, Kamaji, kubeadm.
- **Not supported:** managed control planes (EKS, GKE, AKS) that hide apiserver configuration.

On a managed platform, either front it with a self-managed control plane or stay in configured-author
mode. The [audit webhook connectivity design](facts/audit-webhook-api-server-connectivity.md) has
the full reasoning on hosting.

## Source clusters — the `ClusterProvider`

Each source cluster a `GitTarget` mirrors FROM is named by a cluster-scoped **`ClusterProvider`**,
the read-side peer of `GitProvider`. `GitTarget.spec.clusterProviderRef` **defaults to
`{name: "default"}`** — a provider you create by that conventional name, which the chart can render
for you with `clusterProvider.createDefault: true`. `default` is only the name an omitted reference
points at: that provider may omit `spec.kubeConfig` (the operator's own cluster) or set it to mirror
a remote one.

The provider's **name is the cluster's identity** for the watch data plane, and the default for its
**audit route**. Attribution facts are partitioned by `spec.attribution.auditRoute`, which falls back
to the provider's name, so a fact from one cluster can never name the author of an object watched on
another. Set it explicitly when several providers name one cluster: an API server has a single audit
webhook backend and posts under one route, so the others must declare that route to see its facts.
A provider also carries a deny-by-default
`spec.allowedNamespaces` policy: a `GitTarget` may reference it only from an admitted namespace
(enforced on every reconcile, before that target's watches start — so tightening the policy also
stops a `GitTarget` that already exists).

**No provider, no streaming.** A `GitTarget` may mirror a source cluster only through an *existing*
`ClusterProvider` — `default` included — and the operator **never creates one**. If you set
`clusterProvider.createDefault: false` without committing your own `default`, a `GitTarget` that
references it is held `NotReady` (`ClusterProviderNotFound`) and never falls back to an implicit
in-cluster identity. Commit the object yourself, or point such targets at another `ClusterProvider`.

**Remote source clusters.** Create a `ClusterProvider` with a `spec.kubeConfig.secretRef` (the
kubeconfig Secret lives in the operator namespace) for the outbound *watch* connection, and — for
attribution — configure that cluster's apiserver to POST audit events to `/audit-webhook/<name>`
(the provider's name). The audit server already requires a CA-signed client certificate
(`RequireAndVerifyClientCert`); the remote apiserver presents the **cert-manager-issued audit client
certificate** the chart mints, exactly as the local apiserver does, and the operator accepts the
named route only for a `ClusterProvider` that exists. (Binding a distinct client certificate to each
provider is a future hardening; today the trust boundary is CA-level.) Remote attribution needs a
self-managed control plane — EKS/GKE/AKS are not supported (see *When it fits*). The current model is
in [the architecture guide](architecture.md#optional-attribution); the remaining multi-source hardening
work is in [multi-source audit-ingress hardening](design/multi-source-audit-ingress-hardening.md). See
[SECURITY.md](../SECURITY.md#shared-audit-ingress-trust-model) for the accepted shared-credential trust
assumption and its limits.

## Prerequisites

- GitOps Reverser installed and producing configured-author commits.
- A control plane whose apiserver flags you can set and reload.
- **Redis/Valkey, required.** In configured-author mode it is optional; with attribution on it holds the
  audit attribution facts (in addition to watch resume cursors), so `queue.redis.addr` must be
  non-empty. See Redis is required for HA for sizing notes.

## The two sides of the setup

Enabling attribution splits into a **chart side** (in-cluster, managed by Helm) and a
**control-plane side** (node-local files and apiserver flags the chart cannot touch). Do the chart
side first — it generates the exact values the control-plane side needs.

### 1. Chart side — enable and read the notes

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --set attribution.enabled=true \
  --set queue.redis.addr=valkey.example.internal:6379

helm get notes gitops-reverser -n gitops-reverser
```

Use the address of the Redis/Valkey service you prepared in the prerequisites. For an authenticated
instance, also set `queue.redis.auth.existingSecret` (and, if needed,
`queue.redis.auth.existingSecretKey`). The chart rejects attribution without a Redis address rather
than starting an audit receiver that cannot retain facts.

With `attribution.enabled=true` the chart additionally deploys the audit receiver, its Service, and
(via cert-manager) the audit TLS materials — a root CA Secret and a kube-apiserver client-cert
Secret.

`helm get notes` is the authoritative, install-specific output: it renders the audit webhook URL
reachable from your control-plane node, the exact Secret names, and a copy-paste block that assembles
the `audit-webhook.kubeconfig` kube-apiserver expects. Treat that rendered block as the source of
truth rather than transcribing values by hand — it reflects your Service type, ports, and TLS
choices.

### 2. Control-plane side — what the chart cannot do

The chart deliberately does **not** touch your nodes. Working from the rendered notes, you:

1. Generate `audit-webhook.kubeconfig` from the audit Secrets.
2. Copy it to **every** control-plane node (it must be a static file present before the apiserver
   reads it).
3. Provide an audit **policy** file — start from the tuned example at
   [`test/e2e/cluster/audit/policy.yaml`](../test/e2e/cluster/audit/policy.yaml), which already drops
   runtime noise and heartbeats.
4. Set the apiserver audit flags and point them at those files:
   `--audit-policy-file`, `--audit-webhook-config-file`, and `--audit-webhook-mode=batch`.
5. Restart or reload kube-apiserver once, one node at a time.

**Use `batch` mode. Never start with `blocking` or `blocking-strict`** — those couple normal API
request latency to the health of the audit receiver and can reject valid requests when the receiver
is slow. The [connectivity design doc](facts/audit-webhook-api-server-connectivity.md#audit-webhook-backend-best-practices)
covers the recommended flags and what to tune first; the
[TLS design doc](finished/audit-webhook-tls-design.md) covers trusting the receiver certificate versus
the local-only `insecure-skip-tls-verify` shortcut. For a k3s-specific file-placement walkthrough,
see [`audit-setup/cluster/readme.md`](audit-setup/cluster/readme.md).

## How matching works

Audit facts and watch events arrive on independent paths, so the controller joins them with a bounded
wait rather than blocking:

- **`attribution.grace`** (default `3s`) — how long a watch event waits for a matching audit fact
  before it ships with the explicit unresolved author. Larger raises the attribution hit-rate at the cost
  of commit latency.
- **`attribution.ttl`** (default `10m`) — how long an unmatched audit fact is retained waiting for
  its watch event.

Attribution is opportunistic: on a strong match the named user or service account is the author; with
no match in the grace window, the commit still lands, authored as
`unknown (attribution unresolved)`. No change is dropped for lack of a match. For a live mutation that
should be attributable, treat this author as an audit-attribution configuration or delivery problem until
the audit policy, webhook route, source identity, and Redis connectivity are verified.

## Verifying and reverting

The controller reports `NotReady` on `/readyz` until the audit listener is accepting connections
(with a loaded TLS cert) and it has reached Redis, so a rollout or cert rotation buffers events in the
apiserver instead of dropping them. Once ready, make a change as a distinct user and confirm the
resulting commit's **author** is that identity while the committer is unchanged.

To return to configured-author, upgrade with `--set attribution.enabled=false`. The audit receiver and
Service are removed; you can then also remove the apiserver audit flags on the nodes.

## Related docs

- [`design/audit-webhook-api-server-connectivity.md`](facts/audit-webhook-api-server-connectivity.md):
  networking, DNS, and TLS tradeoffs for audit delivery
- [`design/audit-webhook-tls-design.md`](finished/audit-webhook-tls-design.md): trusting the audit receiver certificate
- [`configuration.md`](configuration.md): core configuration objects
- [`../README.md`](../README.md): product overview and configured-author quick start
