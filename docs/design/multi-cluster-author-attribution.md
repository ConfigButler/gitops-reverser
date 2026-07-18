# Author attribution across source clusters (via a `ClusterProvider`)

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)
>
> Closes the attribution gap [`config-plane-split.md`](../finished/config-plane-split.md) shipped
> without (its §"Explicitly out of scope", first bullet), via a dedicated **`ClusterProvider`** —
> the `ClusterConnection`/`SourceCluster` CRD config-plane-split named as its migration path and
> deferred only because the audit story was out of scope. config-plane-split is **merged but not
> released**, so there is no released API to keep compatible — inline `spec.kubeConfig` is removed
> outright. Advances [`multi-cluster-audit-ingestion-implications.md`](./multi-cluster-audit-ingestion-implications.md)
> §5.
>
> **v2 — revised after review.** Five things changed from the first draft, all argued below: the
> fact key is the provider **name, not a UID** (the UID has no value for an implicit-local cluster,
> and does not solve incarnation safety anyway); a cluster-scoped shared provider needs **namespace
> authorization** (Secret-pinning does not close the confused-deputy — it is a *data-export*
> escalation, not a credential-read one); **authenticated ingress is a prerequisite**, moved ahead
> of routing; **admission attribution is deferred entirely** (it does not unlock managed clusters —
> its callback auth needs the same apiserver flags audit does); and provider `kubeConfig` is
> **immutable in v1** (mutable endpoint repointing is a misattribution footgun).
>
> **v3 — local is the reserved `default` provider.** Local is a **shipped `ClusterProvider` named
> `default`** (kubeConfig omitted = in-cluster). `GitTarget.spec.clusterProviderRef` **defaults to
> `{name: "default"}`** rather than being an implicit `nil` — so every reference is concrete and
> jumpable (for a future "follow reference"/F12 traversal), and `default` == in-cluster is enforced by
> **per-object CEL** (name-uniqueness gives the singleton; no validating webhook for it). No `isDefault`
> (`default` is a fixed reserved name on an immutable object, not a movable pointer). `watchLocal` on by
> default.
>
> **v4 — `default` is not reserved, and audit routes are named. Supersedes the v3 bullet above and
> every "reserved"/`watchLocal` claim below.** `default` is only *the name an omitted
> `spec.clusterProviderRef` points at*. The per-object CEL tying that name to an absent `kubeConfig`
> is **removed**: `kubeConfig` is optional for any provider (omitted = the operator's in-cluster
> config), so a provider named `default` may mirror a **remote** cluster. Audit routes are all
> **named** — `/audit-webhook/default` included, existence-gated like every other name — and the bare
> `/audit-webhook` is instead the **shared-stream** endpoint that resolves each event's provider from
> `attribution.clusterAnnotationKey`, rejecting the request with 400 while that key is unset and
> rejecting an unroutable *event* (no annotation, or an unknown provider) without failing its batch.
> The chart value is now `clusterProvider.createDefault` (+ `clusterProvider.default.*`), not
> `attribution.watchLocal`/`localAllowedNamespaces`: the provider is required for **all** mirroring,
> not just attribution, and it may be remote — so both halves of the old name misled. The operator
> still **never** creates a `ClusterProvider`. See
> [`../configuration.md`](../configuration.md) for the shipped model.
>
> The **data plane matches**: `LocalClusterID` is gone. The watch engine now separates the **config
> plane** (the operator's own cluster — the API-surface trigger informers, the singleton catalog
> metrics; never a source, never torn down) from **source clusters**, one context per
> `ClusterProvider` name. Whether a source is in-cluster is *resolved* from its provider — an absent
> `spec.kubeConfig` — and never inferred from its name, so a `default` that carries a kubeConfig is
> genuinely remote. `SourceClusterResolver` is the single authority: it answers "in-cluster" with a
> nil `rest.Config`, and it is required for any GitTarget to mirror, single-cluster installs included.

## One sentence

Introduce a cluster-scoped **`ClusterProvider`** — the read-side peer of `GitProvider` — that a
`GitTarget` references by a **stable, admin-chosen name**; make that **name** the cluster's identity
everywhere (the `/audit-webhook/<name>` route *and* the fact-index key), authenticate each source
with a **per-provider client certificate** bound to the provider, authorize **which namespaces** may
reference it, and ship a reserved **`default` `ClusterProvider`** for the operator's own cluster that
`clusterProviderRef` *defaults to* (a concrete ref, never `nil`) — retiring the `SourceClusterID` string
entirely.

## Problem

Author attribution is a **join keyed by `(group/resource, object-uid, resourceVersion)`** with no
cluster dimension:

- **Write side** — the local apiserver POSTs audit events to `/audit-webhook`; the handler
  ([`audit_handler.go`](../../internal/webhook/audit_handler.go)) records a minimal fact per
  accepted mutation via `RecordFact`
  ([`attribution_index.go`](../../internal/queue/attribution_index.go)), keyed
  `…:author:v1:audit:<group/resource>:object:<uid>:<rv>` (plus a `:last` pointer and an rv-only
  `:rv:<rv>` hatch).
- **Read side** — a live watch event calls `AuthorResolver.ResolveAuthor(ctx, gvr, uid, rv,
  exactCapable)` ([`author_resolver.go`](../../internal/watch/author_resolver.go),
  [`target_watch.go`](../../internal/watch/target_watch.go) `attachAuthor`), reading the fact back
  by `(group/resource, uid, rv)`.

Config-plane-split broke the symmetry: a remote `GitTarget` now **watches a remote cluster** (remote
UIDs/RVs), but the audit webhook is **local-only** (`validateAuditWebhookPath` rejects any cluster-id
segment). A remote watch event looks up `(gr, uid, rv)`, finds no fact, and ships as the committer.

Two things are missing, and both need a **name for the cluster**: an *ingress* a remote apiserver can
reach tagged with which cluster it is, and a *cluster dimension in the keyspace* so a fact from
cluster A never joins a watch event from cluster B.

## Identity model — the name is the key; the UID only authenticates

`GitTarget.SourceClusterID()` (`<namespace>/<name>/<key>`) and every use of that string as a
data-plane key are **removed** (see *What we remove*). It fused two identities and leaked a Secret
reference. The replacement is **one identity, the provider name**, with the UID confined to the auth
layer:

| Concern | Value | Why |
|---|---|---|
| **External** — the audit route | `ClusterProvider.metadata.name` | admin-chosen, DNS-safe, known before install (bakeable into apiserver params at cluster-creation time), immutable |
| **Internal** — the fact-index cluster key, GVR scoping, the `clusters` map key | **the same name** | unique because cluster-scoped; on the audit path already; known to the watch side with no lookup; **local keys by `default`** — no `""` special case |
| **Authentication** — which incarnation is talking | `ClusterProvider.metadata.uid` | binds the per-provider client cert to one incarnation; revoked on delete (see *Authenticated ingress*) |

**Why the name is the key, not the UID (a reversal from v1, and a pushback on the review's
"UID-keyed facts").** Two reasons:

1. **UID does not deliver the incarnation safety it seems to.** The audit *route* is name-based, so a
   delayed/retried batch from a *deleted* cluster still hits `/audit-webhook/prod-eu-1`, resolves to
   the **current** provider, and lands under whatever key we choose. UID-in-the-key does not stop
   that; only **rejecting the stale sender** does (auth revocation) — plus a **fact purge on
   delete**. Those close it regardless of key shape.
2. **The name needs no translation.** The audit path already carries it; the watch side already knows
   its provider. UID-keying would add a `name → uid` lookup on *both* the write and read paths for no
   gain, and make Redis keys opaque.

(The concern that first surfaced this — an implicit-local cluster has no object and therefore no UID —
is now moot, since local is a shipped object with a name; but the two reasons above stand on their own,
and are why keying by name survives even now that every source has a UID.)

So the UID is used where it is actually load-bearing — **binding the authenticated sender to a
physical incarnation** — and nowhere else. `git.Event.SourceClusterID` /
`ResolvedTargetMetadata.SourceClusterID` become **`SourceCluster`** carrying the provider *name*
(the shipped local provider's name for local); the target-scoped GVK→GVR resolution
config-plane-split added is unchanged in shape.

## Decision

1. **Add a cluster-scoped `ClusterProvider`** — the read-side peer of `GitProvider`; a `GitTarget`
   references one by name (`spec.clusterProviderRef`), as it already references a `GitProvider`.
2. **Identity = the provider name**, everywhere; UID only authenticates (above). Retire
   `SourceClusterID`.
3. **The provider carries a namespace-access policy** (`spec.allowedNamespaces`, **deny-by-default**);
   a `GitTarget` may reference it only from an allowed namespace — enforced at admission *and* before
   any watch starts.
4. **Authenticated ingress is a prerequisite**, not a late add: a per-provider client cert bound to
   the provider, `cert-provider == path-provider`, revoked on delete. Remote paths are refused until
   this exists.
5. **The fact index gains a cluster dimension** keyed by provider **name** (the local provider
   included); a finalizer purges a provider's facts on delete.
6. **Local is the reserved `default` `ClusterProvider`** (kubeConfig omitted = in-cluster);
   `clusterProviderRef` **defaults to `{name: "default"}`** (concrete, jumpable — never `nil`). CEL
   enforces *named `default` iff no kubeConfig*, so name-uniqueness makes it a singleton (no webhook).
   The chart ships it (`watchLocal: true`); `watchLocal: false` → not-found → `NotReady`. No `isDefault`.
7. **v1 = audit attribution on self-managed clusters only.** Remote `attribution.mode` defaults to
   **`None`**; **`Admission` is deferred entirely** (it does not unlock managed clusters); provider
   `kubeConfig` is **immutable**; workload identity and mutable repointing are deferred.

```
GitTarget  ─ spec.clusterProviderRef ─▶ ClusterProvider   (source: READ + attribution, authorized per namespace)
           ─ spec.providerRef ────────▶ GitProvider       (destination: WRITE)
```

## Why this is config-plane-split's planned step

Config-plane-split argued against a dedicated CRD, but its reasoning was conditional: *"A dedicated
CRD's remaining benefits — reuse across many `GitTarget`s, platform-admin RBAC ownership, a home for
connectivity status — are real but not needed for the first version … its load-bearing rationale is
gone: `main` removed the `/audit-webhook/<cluster-id>` path … Multi-cluster is now purely a kube-API
story."* Re-adding attribution restores the audit story and makes all three deferred benefits needed.
It pre-drew the shape: a **sibling** `sourceClusterRef` naming a `ClusterConnection`/`SourceCluster`
CRD "that only platform admins may create … referenced by name from the `GitTarget`." This is that
object.

## Does an object keep the Flux vision? Mostly — one item deferred

Moving the kubeconfig from inline into a `ClusterProvider` embeds the **same** `meta.KubeConfigReference`
type, so the Flux-shaped roadmap is preserved:

| config-plane-split capability | On a `ClusterProvider` |
|---|---|
| Embed `meta.KubeConfigReference` verbatim; a Flux kubeconfig Secret works unchanged | **Preserved** — same embedded type |
| `value`→`value.yaml` key order; reject-not-strip `exec`/insecure-TLS | **Preserved** — same resolver, run by the provider reconciler; verdict on the provider |
| Workload identity (`configMapRef`, `provider: generic`→cloud) | **Preserved, deferred** — in the type; a v1 CEL guard rejects it (config-plane-split already deferred it) |
| ServiceAccount impersonation (`serviceAccountName`) | **Preserved, more Flux-faithful** — a sibling of `kubeConfig` on the provider, as in a Flux Kustomization |
| Target-scoped GVK→GVR (no union) | **Preserved** — keyed by provider name |
| Split `Validated`/`SourceClusterReachable` conditions | **Preserved, re-homed** onto the provider |

The object **unlocks** what config-plane-split parked for lack of a home — per-provider `qps`/`burst`
(off the global `--source-cluster-qps/-burst` flags), per-provider attribution mode, and the status
surface — which is exactly the *"real second property"* it said a wrapper lacked. **One thing is now
deferred, not unlocked:** mutable rotation. v1 promised it; the review is right that a mutable
*endpoint* silently retargets and misattributes (below), so **`kubeConfig` is immutable in v1**; only
Secret *contents* rotation stays transparent (the resolver re-reads — as config-plane-split already
did). Immutability sits on both `GitTarget.spec.clusterProviderRef` (which cluster a folder sources
from = folder identity) and `ClusterProvider.spec.kubeConfig` (which physical cluster a name means).

## Local — the reserved `default` provider, and a defaulted ref

Local is a **first-class `ClusterProvider`** named **`default`**: `kubeConfig` is optional, and
**omitted means in-cluster** (the operator's own cluster via in-cluster config — Flux's "the cluster I
run in"). The chart **ships it** by default (`attribution.watchLocal: true`, named `default`).

**The ref is *defaulted*, never `nil`.** `GitTarget.spec.clusterProviderRef` carries a schema default of
`{name: "default"}`, so a target that omits it persists with a concrete ref to the `default` provider —
there is no implicit-`nil` sentinel. This is deliberately chosen over "`nil` means local" for one
forward-looking reason: **every reference is always populated and jumpable**, so a "follow reference"
(F12) traversal over the object graph never hits an implicit hop with nowhere to land. It also makes
`kubectl get gittarget -o yaml` self-describing — the source cluster is always shown, even for local.

**`default` is reserved for the in-cluster cluster, enforced by per-object CEL** — no validating webhook
needed for this (a simplification over the v3 draft, which used one). The rule is *named `default` **iff**
`kubeConfig` is absent*:

```go
// +kubebuilder:validation:XValidation:rule="(self.metadata.name == 'default') == !has(self.spec.kubeConfig)",message="the ClusterProvider named 'default' is the in-cluster provider and must omit kubeConfig; every other provider must set kubeConfig"
```

Kubernetes name-uniqueness makes `default` a singleton for free, so "exactly one in-cluster provider"
falls out — the cross-object count the v3 webhook did is gone. The bare `/audit-webhook` path, the ref
default, and "the operator's own cluster" all coincide on `default`.

**Turning local watching off** stays explicit: `watchLocal: false` ships no `default` provider, and a
`GitTarget` (defaulted or explicit) referencing `default` is then a clear `NotReady` — the *same*
"referenced ClusterProvider not found" path as any missing remote, not a special case.

**We still reject an `isDefault` flag.** `default` is a **fixed reserved name on an immutable object**,
not a movable pointer: because `clusterProviderRef` is immutable (a folder's source *is* its identity)
and `kubeConfig` is immutable, nothing about `default` can be re-pointed to silently retarget the
folders bound to it. A mutable/movable default (`isDefault`) would reintroduce exactly that
silent-retarget hazard, so it stays rejected.

Because local is a named object it keys facts by its **name** (`default`) like every other provider (no `""`
special case) and gets the same status surface; the bare `/audit-webhook` path resolves to it (the
single in-cluster provider), so the local apiserver's config stays simple.

## Authorization — the namespace is a *policy on the provider*, not the Secret's location

**The confused-deputy this must close is data export, not credential read.** A cluster-scoped provider
holds a credential that may read a lot of a remote cluster. Any `GitTarget` that references it causes
the operator — using *its* credential — to mirror that cluster's state into the `GitTarget`'s
destination. So a tenant who can create a `GitTarget` **and their own `GitProvider`** could reference a
platform provider and **export whatever the operator can read on the remote into a repo they control**.
Pinning the kubeconfig Secret to the operator namespace does **not** help: the tenant never touches the
Secret. (The v1 draft's claim that Secret-pinning "closes the confused-deputy by construction" was
wrong and is retracted.)

**Fix: a namespace-access policy on the provider, deny-by-default.**

```yaml
spec:
  allowedNamespaces:            # deny-by-default: empty = no namespace may reference this provider
    names: [team-a, team-b]     # explicit list, and/or
    selector: {matchLabels: {tier: trusted}}   # a label selector on namespaces
```

Enforced in **two** places: a validating admission webhook rejects a `GitTarget` whose namespace is
not allowed by its referenced provider, and the watch manager **refuses to start watches** for a
target that fails the check at reconcile (defense in depth against a policy that tightened after the
`GitTarget` was created). Denial is tested explicitly.

This is config-plane-split's "RBAC on the object" model made real: platform admins create providers
and decide who may reference them; tenants reference by name and are bounded by the policy.

## Authenticated ingress — per-provider client identity, before any remote path

Path is **routing, not authentication.** A name is guessable; on its own, anyone who can reach
`:9444/audit-webhook/<name>` could inject false author facts. Today the server already does
`RequireAndVerifyClientCert` against a CA ([`main.go`](../../cmd/main.go): `buildAuditServerTLSConfig`),
but the chart issues **one** client cert with `commonName: kube-apiserver`
([`audit-certificates.yaml`](../../charts/gitops-reverser/templates/audit-certificates.yaml)) and the
handler **never reads the peer certificate** — so every source is indistinguishable and the path is the
only (unauthenticated) discriminator. That is fine for one local apiserver; it is not enough the moment
a second cluster can POST.

**One audit server, not one per cluster** (a listener per cluster is unscalable and pointless). The
per-cluster distinction is the **client identity**:

- Each `ClusterProvider` has its **own client credential** — a cert whose subject/SAN (or a pinned
  SPKI fingerprint) maps to that provider. The apiserver on the source cluster presents it.
- The handler **reads `r.TLS.PeerCertificates`**, maps it to a provider, and **requires
  `cert-provider == path-provider`** — a forged path without the matching cert is rejected.
- The credential is **bound to the provider incarnation (UID)** and **revoked on delete**, so a
  delayed batch from a deleted/recreated cluster fails auth instead of misattributing (the P0.3 fix;
  the fact-purge finalizer is the belt to this suspenders).

This is a **prerequisite**, sequenced *before* remote path routing — the operator must never accept a
remote fact it cannot attribute to an authenticated provider. The cert issuance/rotation/revocation
flow is genuinely new design (the chart's cert-manager path mints the *local* client cert but cannot
reach a remote apiserver's filesystem), and is an explicit build step, not an afterthought.

**Answer to "separate TLS per webhook or just path?"** — one server, path for routing, a per-provider
**client cert** for authentication, required to agree. Never path-only.

## API shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider          # cluster-scoped
metadata:
  name: prod-eu-1              # identity: /audit-webhook/prod-eu-1 AND the fact-index key
spec:
  kubeConfig:                  # OPTIONAL: omit for the in-cluster (local) provider. IMMUTABLE in v1.
    secretRef:                 # Flux's meta.KubeConfigReference; secretRef pinned to the operator ns
      name: prod-eu-1-kubeconfig
    # configMapRef: {...}      # RESERVED — Flux workload identity; v1 CEL guard rejects it
  # serviceAccountName: ...    # RESERVED — Flux remote impersonation; future sibling
  allowedNamespaces:           # authorization, DENY-BY-DEFAULT (empty = none)
    names: [team-a]
  attribution:
    mode: None                 # None (default) | Audit.  Admission is deferred (not in the enum)
  qps: 20                      # outgoing kube client throttle (was --source-cluster-qps)
  burst: 40
  ingressLimits:               # INCOMING audit protection — distinct from qps/burst
    maxEventsPerSecond: 500    # per-provider ceiling; excess is shed, counted, not queued unbounded
status:
  observedGeneration: 1
  conditions:                  # Validated, Reachable, DiscoveryHealthy (per cluster, not per GitTarget)
    - type: Ready
  lastAuditEventTime: "..."    # NOT "AuditIngestionActive": a quiet cluster is not unready
```

```go
// GitTarget names its source by a ref. Inline spec.kubeConfig is removed.
type GitTargetSpec struct {
    // … providerRef (GitProvider, write side), branch, path (unchanged, immutable) …
    // ClusterProviderRef names the SOURCE cluster. DEFAULTS to {name: "default"} (the in-cluster
    // provider) — a concrete, jumpable ref, never nil. IMMUTABLE.
    // +kubebuilder:default={name: "default"}
    // +optional
    ClusterProviderRef *ClusterProviderReference `json:"clusterProviderRef,omitempty"`
}
// +kubebuilder:validation:XValidation:rule="self.clusterProviderRef == oldSelf.clusterProviderRef",message="spec.clusterProviderRef is immutable"
```

The shipped local provider is the same kind, reserved-named `default`, with `kubeConfig` omitted:

```yaml
kind: ClusterProvider
metadata: {name: default}      # chart-shipped when attribution.watchLocal=true; RESERVED name
spec:
  # no kubeConfig => in-cluster. CEL: (name == "default") == !has(kubeConfig)
  allowedNamespaces: {...}      # who may bind local (chart sets a sensible default)
  attribution: {mode: None}     # or Audit, fed by the local apiserver on the bare /audit-webhook path
```

The cluster-scoped `secretRef` needs a namespace Flux's type lacks (pinned to the operator ns) — the
*only* departure from verbatim-Flux reuse; the embedded type is otherwise unchanged.

## Ingress, keyspace, and the recorder API

- **Routing.** `ServeHTTP` switches on the path: bare `/audit-webhook` → the **`default`** (in-cluster)
  provider; `/audit-webhook/<name>` → the named provider **iff** the peer cert authenticates it;
  unknown/unauthorized → 404/403. `validateAuditWebhookPath` relaxes to "accept a segment naming an
  *authenticated* provider."
- **Keyspace.** A `cluster:<name>` infix on `factKeyExact/Last/RV`, using the resolved provider name —
  the local provider included, so there is no `""` special case. The rv-only hatch becomes
  `(cluster, group/resource, rv)` — **the correctness fix** (RV is not globally unique, so the no-UID
  hatch was cross-cluster-ambiguous without this). Facts are ephemeral (15-min TTL,
  [`DefaultAttributionFactTTL`](../../internal/queue/attribution_index.go)); a **delete finalizer purges
  `cluster:<name>:*`** so a recreated name starts clean.
- **Recorder API (explicit).** `RecordFact(ctx, providerName string, event auditv1.Event)` — the
  current interface has **no** cluster argument ([`audit_handler.go`](../../internal/webhook/audit_handler.go)
  `AuditFactRecorder`); the handler resolves the authenticated provider and threads its **name**.
  `ResolveAuthor`/`LookupAuthorResolution` gain the same `providerName` (the local provider's name for
  a `nil`-ref target); `attachAuthor` passes it.

## Timing — batch-max-wait vs the grace window is a stated prerequisite

Exact attribution needs the audit fact to arrive within the resolver's grace window
([`DefaultAttributionGraceWindow`](../../internal/watch/author_resolver.go) = 3s). The apiserver's
`--audit-webhook-batch-max-wait` **defaults to 30s**, so at low mutation volume a fact can arrive after
the commit and the event ships as committer. This is **not new** — the local path relies on the same
relationship, and the repo already handles it: e2e sets `--audit-webhook-batch-max-wait=1s`
([`start-cluster.sh`](../../test/e2e/cluster/start-cluster.sh)) and the chart NOTES / setup guide make
low max-wait the freshness knob. The multi-cluster design makes it **explicit and per-provider**: a low
`batch-max-wait` on the source apiserver is a documented prerequisite, and the grace is
**per-`ClusterProvider` configurable** for links that are laggier than local. (A missed fact degrades
to a committer commit — never wrong, just less rich — so this is a freshness SLO, not a correctness
bug.)

## Status — a quiet cluster is not unready

Owned by the provider: `Validated` (inputs), `Reachable` (discovery reached the API),
`DiscoveryHealthy` (types + followability, per cluster), aggregated `Ready`, with `observedGeneration`.
Attribution health is a **`lastAuditEventTime` timestamp**, *not* an `AuditIngestionActive` condition —
a normally-quiet cluster with no recent mutations must not read as unready. If a readiness signal is
wanted, it is `Unknown` until the first event, never `False` for silence. `GitTarget` **projects** a
one-line `ClusterProviderReady` (the `GitProviderReady` pattern, with a `Watches(&ClusterProvider{})`
trigger), so per-cluster detail lives once on the shared object, not copied onto every target.

## Admission attribution — deferred entirely from v1

The v1 draft sold a remote `ValidatingWebhookConfiguration` as the *"easy, identical-identity"* fallback
for managed clusters. That is **wrong on both counts**, and it is removed from the v1 enum:

- **Not identical.** A `ValidatingWebhookConfiguration` configures the webhook **server URL + CA**, not
  a client identity for the *apiserver*. For the apiserver to authenticate *to* the webhook (so we can
  trust and attribute the caller) needs an `AdmissionConfiguration` via `--admission-control-config-file`
  — an **apiserver flag**.
- **Not a managed-cluster unlock.** That flag is the *same class* of thing managed control planes hide.
  The product's own [attribution-setup-guide.md](../../docs/attribution-setup-guide.md) already scopes
  attribution to self-managed control planes and lists EKS/GKE/AKS as **not supported**. Admission does
  not change that.

So admission attribution buys us an *unauthenticated* callback on exactly the clusters where audit also
fails, plus a fail-open webhook that risks blocking tenant writes, plus a weaker (`:last`-only, no
post-write RV) join. The honest managed-cluster answer is a **source-side agent** (runs in the cluster,
authenticates outward) — a separate, larger future feature. Admission stays out of v1; if it ever
returns it needs its own authenticated-source design, and command authorship (`/validate-operator-types`,
config-plane-only) is untouched regardless.

## CRD invariants (to specify before coding)

- `clusterProviderRef` / `providerRef`: typed local references; `clusterProviderRef` **defaults to
  `{name: "default"}`** and is immutable.
- `attribution.mode`: enum `{None, Audit}`, default `None`; `configMapRef` rejected by CEL.
- `kubeConfig`: **optional (omitted = in-cluster), immutable**; `secretRef` namespace pinned (operator
  ns); **CEL `(name == "default") == !has(kubeConfig)`** — the reserved `default` is the sole in-cluster
  provider (name-uniqueness gives the singleton; **no validating webhook** for it); no `isDefault` field.
- `qps`/`burst`: positive, bounded defaults; `ingressLimits.maxEventsPerSecond` required with a bounded
  default.
- `allowedNamespaces`: deny-by-default; names and/or selector; semantics enforced at admission + reconcile.
- Status: `observedGeneration`, kstatus-style `Ready` aggregation, `lastAuditEventTime`, no
  quiet-cluster-unready condition.

## What we remove (and it must not be left in place)

The `SourceClusterID` string is **not** something to keep — it is deleted, not adapted:

- **`GitTarget.SourceClusterID()`** ([`gittarget_types.go`](../../api/v1alpha3/gittarget_types.go)) —
  deleted. Resolution is `spec.clusterProviderRef.name` → the provider (→ its uid only for auth).
- **The `<ns>/<name>/<key>` string as any data-plane key** — gone. `clusters
  map[string]*clusterContext` and `clusterIDForGitTarget` key by **provider name** (`default` for the
  in-cluster one; no `""` sentinel and no implicit-`nil` case — the ref is always populated).
- **`git.Event.SourceClusterID` / `ResolvedTargetMetadata.SourceClusterID`** → renamed **`SourceCluster`**,
  carrying the provider *name*.
- **Inline `GitTarget.spec.kubeConfig`** — removed (unreleased; no migration).
- **The v1-draft `cluster:<uid>` fact infix and any `name→uid` fact-key lookup** — never built; the
  infix is the name.
- **The "mutable kubeConfig rotation" idea** — not built in v1 (immutable); Secret-contents rotation
  stays.

## Minimal safe v1, and what is deferred

**v1 (build):** `ClusterProvider` (kubeConfig optional = in-cluster, immutable) + a **shipped local
provider** (`clusterProviderRef` defaults to `{name: "default"}`) + immutable source identity + **namespace authorization** +
**name-keyed facts** + **authenticated audit ingress** + per-provider ingress limits + name-based status
with `lastAuditEventTime`.

**Deferred (explicitly not v1):** admission attribution; workload identity (`configMapRef`);
ServiceAccount impersonation; mutable kubeConfig / endpoint repointing; an `isDefault`/movable default
(the ref defaults to the reserved `default` provider instead); managed-control-plane support (needs a
source-agent).

## Build order

1. **`ClusterProvider` CRD + reconciler** — cluster-scoped; kubeConfig **optional (omitted =
   in-cluster)** + immutable; the **`(name == "default") == !has(kubeConfig)`** CEL; `attribution.mode`
   (`None` default); `allowedNamespaces`; validate kubeconfig (exec/TLS reject) → `Validated`;
   `observedGeneration`.
2. **Retire `SourceClusterID`; re-home the engine onto the provider name** — delete
   `SourceClusterID()`; key `clusters` by name; rename the `git` carriers to `SourceCluster` (name);
   delete inline `kubeConfig`; **default `clusterProviderRef` to `{name: "default"}`** so a ref-less
   target resolves to the reserved in-cluster provider. Behavior-preserving.
3. **Namespace authorization** — a validating webhook rejecting a `GitTarget` in a non-allowed
   namespace; reconcile-time refusal to start watches; denial tests. (Before any remote data flows.)
   *(The in-cluster singleton is CEL from step 1 — no webhook needed for it.)*
4. **Authenticated ingress** — per-provider client credential contract (subject/SAN or SPKI), handler
   reads the peer cert, `cert-provider == path-provider`, incarnation-binding + revoke-on-delete.
   (Before routing accepts remote paths.)
5. **Ingress routing + name-keyed facts** — relax `validateAuditWebhookPath` to authenticated
   providers; `RecordFact(ctx, providerName, event)`; `cluster:<name>` infix; thread `providerName`
   through the read path; delete-finalizer fact purge; per-provider `ingressLimits`. Unit-prove a
   single-provider install's keyspace matches a bare install.
6. **Status + projection** — `Reachable`/`DiscoveryHealthy`/`lastAuditEventTime` on the provider;
   `ClusterProviderReady` projected onto `GitTarget` with a `Watches(&ClusterProvider{})` trigger;
   per-provider grace override.
7. **Chart + docs** — **ship the local `ClusterProvider` by default** (`watchLocal: true`, sensible
   `allowedNamespaces`); issue/rotate per-provider client certs; document the remote apiserver
   audit-config recipe (name + low `batch-max-wait`); extend the attribution setup guide.

## Test plan

Reuse config-plane-split's kcp harness (`test/e2e/kcp_workspace_test.go`) where a real remote is needed.

- **Unit** — a single-provider install's keyspace matches a bare install; cross-cluster isolation incl.
  the rv-only hatch; recorder threads the provider name.
- **Local binding & defaulting** — a `GitTarget` omitting `clusterProviderRef` persists with
  `{name: "default"}` and binds to the `default` provider; with `watchLocal:false` (no `default`) it is
  `NotReady` via the ordinary "provider not found" path; CEL rejects a non-`default` provider without a
  kubeConfig **and** a `default` provider *with* one; the ref is always populated (jumpable), never nil.
- **Authorization** — a `GitTarget` in a non-allowed namespace is rejected at admission and never
  starts watches; allowed namespace works; tightening the policy stops an existing target.
- **Authentication** — cert/path mismatch is rejected; a provider's cert authenticates only its own
  path; cert rotation continues ingestion; revocation on delete stops it.
- **Recreation/repoint** — stale audit delivery to a recreated name fails auth (not misattributed);
  the fact purge ran; `kubeConfig` mutation is rejected (immutable).
- **Timing** — with `batch-max-wait` above the grace, a low-volume mutation degrades to committer (not
  wrong); with the e2e's 1s max-wait it attributes exactly.
- **Isolation** — a noisy provider hitting `ingressLimits` sheds its own excess without starving other
  providers' ingestion, Redis, or the commit queue.
- **e2e (kcp)** — remote author round-trip (`/audit-webhook/<name>`, authored by the real user); the
  three-workspace same-`(ns, ConfigMap, name)` non-leak centerpiece; `ClusterProviderReady` projection
  and recovery.

## Open questions

1. **Namespace-authorization surface.** `allowedNamespaces` (names + selector) on the provider vs a
   `ReferenceGrant`-like object each namespace opts in with. The former is one object for the admin;
   the latter is standard cross-namespace-consent shape. Recommendation: `allowedNamespaces`
   deny-by-default now; revisit ReferenceGrant if per-namespace self-service consent is wanted.
2. **Per-provider client-credential mechanism.** mTLS client cert (subject/SAN map, or SPKI pin) vs a
   per-provider bearer token in the audit webhook kubeconfig. Cert fits the existing mTLS setup; token
   is simpler for some operators. Pick one contract, incl. rotation/revocation.
3. **`:last`-as-weak** — only relevant if admission ever returns; leave the read policy exact-only for v1.
4. **Ingress backpressure policy** — shed vs buffer-with-bound when a provider exceeds `ingressLimits`,
   and how that surfaces (a condition? a metric only?).
