# `ClusterProvider` multi-cluster author attribution: decision record

> **finished** — shipped or closed. Kept for context only; **nothing here binds**. For current
> behaviour see [`../architecture.md`](../architecture.md), [`../configuration.md`](../configuration.md),
> and [`../../SECURITY.md`](../../SECURITY.md). Index: [`../INDEX.md`](../INDEX.md)
>
> Produced the cluster-scoped `ClusterProvider`, immutable `GitTarget.spec.clusterProviderRef`,
> provider-name-partitioned attribution facts, reconcile-time namespace authorization, and named
> audit routes. Remaining ingress work:
> [`../design/multi-source-audit-ingress-hardening.md`](../design/multi-source-audit-ingress-hardening.md).
> Purge-on-delete was later answered separately:
> [`clusterprovider-fact-purge.md`](clusterprovider-fact-purge.md).

## The problem

Author attribution is a **join keyed by `(group/resource, object-uid, resourceVersion)`** with no
cluster dimension. The write side records a fact per accepted mutation from the local apiserver's
audit stream ([`audit_handler.go`](../../internal/webhook/audit_handler.go) → `RecordFact`); the
read side resolves it back on each live watch event
([`author_resolver.go`](../../internal/watch/author_resolver.go)).

Config-plane split broke the symmetry: a remote `GitTarget` watches a **remote** cluster (remote
UIDs and RVs) while the audit webhook was **local-only**. A remote watch event found no fact and
shipped as the committer.

Two things were missing, and both needed a **name for the cluster**: an ingress a remote apiserver
can reach, tagged with which cluster it is, and a cluster dimension in the keyspace so a fact from
cluster A never joins an event from cluster B.

## What shipped

`ClusterProvider` is the cluster-scoped read-side peer of `GitProvider`. A `GitTarget` references
one by immutable name; that name partitions the source client, discovery and watch state, and the
attribution facts. `spec.allowedNamespaces` is deny-by-default and is checked on every reconcile,
before any watch starts.

```text
GitTarget ─ spec.clusterProviderRef ─▶ ClusterProvider   (source: READ + attribution, authorized per namespace)
          ─ spec.providerRef ────────▶ GitProvider       (destination: WRITE)
```

Audit ingestion uses `/audit-webhook/<provider>` and stores facts in that provider's partition. The
server verifies the sender is signed by the audit CA and that the named provider exists; it does
**not** bind an individual certificate to a provider. That is an accepted shared-control-plane
trust boundary, not tenant isolation — see the hardening doc.

---

## The decisions, and what each one replaced

### The name is the identity; the UID only authenticates

`GitTarget.SourceClusterID()` (`<namespace>/<name>/<key>`) was **deleted, not adapted**. It fused
two identities and leaked a Secret reference. One identity replaced it — the provider **name** —
used for the audit route, the fact-index key, the GVK→GVR registry key, and the `clusters` map key.
`git.Event.SourceClusterID` became `SourceCluster`, carrying the name.

An earlier revision keyed facts by **UID**. Reversed, for two reasons:

1. **UID does not deliver the incarnation safety it appears to.** The audit *route* is name-based,
   so a delayed batch from a deleted cluster still hits `/audit-webhook/prod-eu-1` and resolves to
   the *current* provider. UID-in-the-key does not stop that; only rejecting the stale sender does.
2. **The name needs no translation.** It is already on the audit path and already known to the
   watch side. UID-keying would add a `name → uid` lookup on *both* paths for no gain, and make
   Redis keys opaque.

### `default` is a defaulting convention, **not** a reserved local name

This one was reversed late and matters most, because the rejected version is the intuitive one.

An earlier draft made `default` **reserved for the in-cluster cluster**, enforced by CEL
`(self.metadata.name == 'default') == !has(self.spec.kubeConfig)`, with the chart shipping the
object. **None of that shipped.**

What shipped: local-vs-remote follows from **`spec.kubeConfig` alone** — omitted means the
operator's own cluster, for *any* name
([`clusterprovider_types.go:174-178`](../../api/v1alpha3/clusterprovider_types.go#L174-L178)). A
provider named `default` may carry a `kubeConfig` and mirror a remote cluster; a provider named
anything else may omit it and be in-cluster. `default` is only the value that
`GitTarget.spec.clusterProviderRef` defaults to
([`gittarget_types.go:98`](../../api/v1alpha3/gittarget_types.go#L98)), and the operator never
creates that object — a user does.

Two things drove the reversal. Pinning a *name* to a physical cluster is the same
silent-retarget hazard that `kubeConfig` immutability exists to prevent — it just moves it into the
schema. And the reserved name bought nothing the existence check did not already give: a
`GitTarget` naming a provider that does not exist is held unready through the ordinary
"provider not found" path, so turning local watching off needs no special case.

The **ref is defaulted, never `nil`**, which did survive: every reference is populated and jumpable,
so a "follow reference" traversal never hits an implicit hop with nowhere to land, and
`kubectl get gittarget -o yaml` is self-describing. An `isDefault` **flag was rejected** — a movable
default would reintroduce the retarget hazard directly.

> Downstream consequence: locality is derived from `kubeConfig`, never from the name, and
> `GitTarget.IsLocalSource()` — despite the name — **is a name test** that only seeds the
> pre-discovery `SourceClusterReachable` default. It is not a locality predicate. A later design
> took the further step of not deriving *permissions* from locality at all: see
> [`../design/watchrule-source-namespace/`](../design/watchrule-source-namespace/README.md), whose
> PR 3 page restates this trap verbatim.

### Namespace authorization enforced once, on reconcile

The confused deputy to close is **data export, not credential read**. A cluster-scoped provider
holds a credential that can read a lot of a remote cluster; any `GitTarget` referencing it makes
the operator mirror that state into the target's destination. So a tenant who can create a
`GitTarget` *and their own `GitProvider`* could export whatever the operator can read into a repo
they control. Pinning the kubeconfig Secret to the operator namespace does **not** help — the
tenant never touches the Secret. (An earlier claim that Secret-pinning "closes the confused deputy
by construction" was wrong and is retracted.)

The fix is `spec.allowedNamespaces` on the provider, **deny-by-default** (empty admits nobody),
names and/or a label selector, ORed.

This draft specified enforcement **"in two places"**, one of them a validating admission webhook.
**One shipped**, on every reconcile, returning before `DeclareForGitTarget`
([`gittarget_controller.go:311`](../../internal/controller/gittarget_controller.go#L311)) — which
also covers a policy tightened *after* the `GitTarget` was created, something admission cannot see.
That reasoning was generalized into a repo-wide rule:
[`../spec/where-validation-lives.md`](../spec/where-validation-lives.md).

### Keyspace and the recorder API

A `cluster:<name>` infix on `factKeyExact/Last/RV`, the in-cluster provider included, so there is no
`""` special case. The rv-only hatch became `(cluster, group/resource, rv)` — **a correctness fix**,
since RV is not globally unique and the no-UID hatch was cross-cluster-ambiguous without it.
`RecordFact` gained an explicit `providerName` argument; `ResolveAuthor` / `LookupAuthorResolution`
the same.

### Admission attribution: cut entirely

An earlier draft sold a remote `ValidatingWebhookConfiguration` as the *"easy, identical-identity"*
fallback for managed clusters. Wrong on both counts, and removed from the enum:

- **Not identical.** A `ValidatingWebhookConfiguration` configures the webhook URL and CA, not a
  client identity for the *apiserver*. For the apiserver to authenticate *to* the webhook needs an
  `AdmissionConfiguration` via `--admission-control-config-file` — an apiserver flag.
- **Not a managed-cluster unlock.** That flag is the same class of thing managed control planes
  hide. [`attribution-setup-guide.md`](../attribution-setup-guide.md) already scopes attribution to
  self-managed control planes.

So it bought an *unauthenticated* callback on exactly the clusters where audit also fails, plus a
fail-open webhook that risks blocking tenant writes, plus a weaker (`:last`-only, no post-write RV)
join. The honest managed-cluster answer is a **source-side agent** — a separate, larger feature.
Command authorship (`/validate-operator-types`) is untouched by this and is unrelated.

### Timing is a prerequisite, not a bug

Exact attribution needs the fact inside the resolver's grace window
([`DefaultAttributionGraceWindow`](../../internal/watch/author_resolver.go) = 3s), while the
apiserver's `--audit-webhook-batch-max-wait` **defaults to 30s**. Not new — the local path always
relied on the same relationship, and e2e sets `1s`. Made explicit and per-provider here. A missed
fact degrades to a committer commit — never wrong, just less rich — so this is a freshness SLO.

### Status: a quiet cluster is not unready

`Validated` / `Reachable` / `DiscoveryHealthy` on the provider, aggregated into `Ready`, projected
onto `GitTarget` as a one-line `ClusterProviderReady` (the `GitProviderReady` pattern) so
per-cluster detail lives once on the shared object. Attribution health is a **`lastAuditEventTime`
timestamp**, deliberately *not* an `AuditIngestionActive` condition — a normally-quiet cluster with
no recent mutations must not read as unready.

---

## Deferred, and why

| Deferred | Why |
|---|---|
| Per-provider client-certificate binding | Real gap; the current CA-only check is a shared-control-plane boundary. Moved whole to [`../design/multi-source-audit-ingress-hardening.md`](../design/multi-source-audit-ingress-hardening.md). |
| Admission attribution | Cut entirely (above) — needs a source-side agent, not a webhook. |
| Mutable `kubeConfig` / endpoint repointing | v1 promised it; a mutable *endpoint* silently retargets and misattributes. `kubeConfig` is immutable; Secret **contents** rotation stays transparent. |
| Workload identity (`configMapRef`), ServiceAccount impersonation | Present in the embedded Flux type; rejected by CEL for now. |
| Fact purge on delete | Answered separately and **rejected**: [`clusterprovider-fact-purge.md`](clusterprovider-fact-purge.md). |
| Managed control planes (EKS/GKE/AKS) | Needs the source-side agent. |
| `ReferenceGrant`-style per-namespace consent | `allowedNamespaces` is one object for the admin; revisit if self-service consent is wanted. |

## Why this was config-plane-split's planned step

Config-plane split argued against a dedicated CRD, but conditionally: its remaining benefits —
reuse across many `GitTarget`s, platform-admin RBAC ownership, a home for connectivity status —
were *"real but not needed for the first version"*, and its load-bearing rationale was gone because
`main` had removed the `/audit-webhook/<cluster-id>` path. Re-adding attribution restored the audit
story and made all three benefits needed. It even pre-drew the shape: a sibling ref naming a CRD
*"that only platform admins may create … referenced by name from the `GitTarget`."*

Moving the kubeconfig from inline into the provider embeds the **same**
`meta.KubeConfigReference`, so the Flux-shaped roadmap survives intact (`value`→`value.yaml` key
order, reject-not-strip `exec`/insecure-TLS, target-scoped GVK→GVR, split
`Validated`/`SourceClusterReachable`). The object also **unlocked** what config-plane split parked
for lack of a home — per-provider `qps`/`burst`, per-provider attribution mode, and a real status
surface. The one departure from verbatim-Flux reuse: the cluster-scoped `secretRef` needs a
namespace Flux's type lacks, pinned to the operator namespace.
