# Separating the config plane from the watched cluster

> **finished** — shipped or closed. Kept for context only; **nothing here binds**. For current behaviour see [`../spec/`](../spec/). Index: [`../INDEX.md`](../INDEX.md)

> Shipped 2026-07-17 as [#249](https://github.com/ConfigButler/gitops-reverser/pull/249). Supersedes
> the now-retired `SourceCluster` CRD proposal.
> Redesign of feature #1 from the closed multi-tenant PR (#220), shipped on its own.

## One sentence

A `GitTarget` may name the cluster it mirrors *from* — an immutable, optional
`spec.kubeConfig`, Flux's `meta.KubeConfigReference` verbatim (the same field
`Kustomization.spec.kubeConfig` uses) — so the operator can read its own config and
Git credentials from the cluster it runs in while watching resources on another,
and one operator can mirror many clusters.

## Problem

GitOps Reverser builds exactly one client:

```go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{ … })
```

That single `rest.Config` serves two jobs that are conceptually unrelated:

- **the config plane** — where `GitProvider`, `GitTarget`, `WatchRule` and the
  `secretRef` Git credentials are read;
- **the watched cluster** — where the resources it mirrors to Git actually live.

Because `GitProvider.spec.secretRef` is a local reference and
`WatchRule.spec.targetRef` must name a same-namespace `GitTarget`, all four
objects have to sit in one namespace **on the cluster being watched**. Nothing
chose this; it fell out of having one kubeconfig.

For an operator mirroring a cluster they also hand to a tenant, that means a Git
write credential — often scoped far more broadly than the one repository a
`GitTarget` names — lives one RBAC rule away from whoever can read Secrets in
that namespace. The isolation rests on a policy decision rather than on a
boundary. Splitting the two planes turns the policy into a boundary: the
credential for a cluster never has to live on that cluster.

## Decision

Add an **inline, optional, immutable** `spec.kubeConfig` to `GitTarget` — Flux's
`meta.KubeConfigReference`, verbatim and inline, exactly like
`Kustomization.spec.kubeConfig`. Omitted means "the cluster I run in" — the
single-cluster default, which needs no configuration and behaves exactly as today.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  providerRef: { name: acme }
  branch: main
  path: clusters/acme
  kubeConfig:                    # NEW — omit for "the cluster I run in"
    secretRef:
      name: acme-kubeconfig
      # key optional; when empty the operator reads "value" then
      # "value.yaml" — Flux's resolution order.
```

The Secret is read from the `GitTarget`'s **own** namespace, on the cluster the
operator runs in. Its value is an ordinary kubeconfig, which determines both the
source cluster and the credentials to reach it. The watched cluster then holds
nothing but the watched resources — no Secret, no `configbutler.ai` CRDs at all.

Three decisions were taken deliberately and are argued below:

- **Inline on `GitTarget`, not a dedicated CRD, and not on `WatchRule`.**
- **A bare `spec.kubeConfig`, not a `spec.sourceCluster` wrapper.**
- **Immutable, and therefore fully decoupled from retarget** — unlike the closed
  PR, which coupled it into a mutable-destination lifecycle and paid for it.

### Why `GitTarget`, not `WatchRule`

A `GitTarget` already owns exactly one materialization: one
(provider, branch, folder). Adding the source cluster makes it one
(**cluster**, provider, branch, folder) — still one owner, one folder, one
desired state. The watch data plane is *already* keyed by `GitTarget`
([`materialization.go`](../../internal/watch/materialization.go): `DeclareForGitTarget`
takes a `gitDest`), so the cluster comes along for free.

Putting it on `WatchRule` would let two rules point at different clusters and
feed one folder, and the mark-and-sweep would then alternately delete each
cluster's objects. That is not a configuration anyone should be able to write.

`WatchRule` keeps its meaning: it watches the namespace **it lives in**, resolved
on the `GitTarget`'s source cluster — a `WatchRule` in config-plane namespace
`team-a` watches namespace `team-a` on the remote. A `ClusterWatchRule` watches
the whole source cluster.

### Why `spec.kubeConfig`, not a `spec.sourceCluster` wrapper

An earlier draft nested the reference as `spec.sourceCluster.kubeConfig`. It is
collapsed to a bare `spec.kubeConfig`, because in v1 the wrapper would hold a
**single** field and the kubeconfig **already determines** the remote cluster and
its credentials — the nesting adds no contract value, only depth. Collapsing it
also makes the field *genuinely* Flux-shaped: `GitTarget.spec.kubeConfig`, the same
path as `Kustomization.spec.kubeConfig`, not a one-deeper cousin.

"Source cluster" stays the **domain / internal** term — the derived cluster-context
id (`SourceClusterID`), the `SourceClusterResolver`, the `SourceClusterUnreachable`
condition — but it is not an API object.

A wrapper would only earn its place by gaining a **real second property now**, and
none exists: per-target qps/burst live on flags, a friendly cluster name is not
needed for correctness, and a future platform-owned reference does **not** justify
it — that arrives as a *mutually-exclusive sibling*, `spec.sourceClusterRef`
alongside `spec.kubeConfig` (see *Future shape*), not as a field nested under a
wrapper today.

### Why inline, not a dedicated `SourceCluster` CRD

An earlier design proposed a dedicated `SourceCluster` CRD fusing **audit-ingress identity** and
**kube-API connectivity** into one onboarding object. Its load-bearing rationale
is gone: `main` **removed** the `/audit-webhook/<cluster-id>` path — the handler
now rejects any cluster-id segment
([`audit_handler.go`](../../internal/webhook/audit_handler.go): `validateAuditWebhookPath`,
*"audit webhook path must not include a cluster ID"*). Multi-cluster is now
purely a **kube-API** story (discovery + snapshot + live watches over a
kubeconfig), and that is exactly the shape Flux uses inline. A dedicated CRD's
remaining benefits — reuse across many `GitTarget`s, platform-admin RBAC
ownership, a home for connectivity status — are real but not needed for the first
version, and are not what Flux reached for. The inline field leaves a clean
migration path (see *Future shape*).

## Scope: what the first PR ships (and what it deliberately does not)

This is intentionally the **smallest useful cut**. The first PR does **one** thing:
let a `GitTarget` mirror a remote cluster reached by a **kubeconfig Secret**. Two
related capabilities are **designed in this document but explicitly NOT built in
this PR** — the plan is written down so the shape is settled, not so it ships now.

| | Mechanism | First PR |
|---|---|---|
| ✅ **In** | `kubeConfig.secretRef` — literal kubeconfig Secret | **built** |
| ⛔ **Out** | `kubeConfig.configMapRef` — cloud / OIDC **workload identity** | **CEL-rejected as reserved**; planned — see *Remote auth mechanisms* |
| ⛔ **Out** | `serviceAccountName` — remote **ServiceAccount impersonation** | not in the API at all; planned — see *Remote auth mechanisms* |

Concretely, in this first PR:

- The **only** Flux import taken is `github.com/fluxcd/pkg/apis/meta` (the type + its
  CEL), pinned to **v1.31.0** (matches this repo's Go 1.26 / Kubernetes 0.36
  dependencies). There is **no** `fluxcd/pkg/runtime` and **no** `fluxcd/pkg/auth`
  import — those arrive only with workload identity, as a separate feature.
- `configMapRef` is **rejected at admission** by a CEL guard, so setting it is a
  legible "not yet supported", never a silent no-op or a half-working path.
- There is **no** remote-impersonation surface at all: a remote `GitTarget` connects
  as the kubeconfig's own identity, which the operator should be given narrow
  read-only RBAC on the remote (see *Security*).

**Why both are deferred, not just unbuilt.** Workload identity turns the operator's
cloud/OIDC identity into a powerful access broker, and remote impersonation lets the
spec author choose the identity requests run as — both raise authorization questions
that the **credential-reference boundary** (*Security*, below) has to answer first.
`secretRef` plus a narrowly-scoped read-only remote identity is the simpler, safer
v1, and nothing about it forecloses either follow-up.

## What Flux does, and what we copy

Flux is the reference implementation of "optional remote cluster via kubeconfig",
so we read it as ground truth. The findings that shaped this design:

- **The type is shared, not per-controller.** `Kustomization.spec.kubeConfig`
  and `HelmRelease.spec.kubeConfig` both embed one type,
  `meta.KubeConfigReference` (`fluxcd/pkg/apis/meta/reference_types.go`). There
  is one canonical shape to copy.
- **It is an all-optional object with two mutually-exclusive refs:** `secretRef`
  (a literal kubeconfig, a `SecretKeyReference` = name + optional key) **or**
  `configMapRef` (a `LocalObjectReference` naming a ConfigMap that drives
  cloud **workload identity**: `provider` ∈ {aws,azure,gcp,generic}, `cluster`,
  `address`, `ca.crt`, `audiences`, `serviceAccountName`). The exactly-one-of
  constraint is enforced by **paired CEL `XValidation`**, not a webhook.
- **The default key is resolved in code, not the schema.** There is no
  `+kubebuilder:default`. `getRESTConfigFromSecret`
  (`fluxcd/pkg/runtime/client/impersonator.go`) resolves: explicit `key` wins →
  else `value` → else `value.yaml` → else error.
- **Kubeconfigs are sanitized — by *stripping*, silently.** Flux's client builder
  **drops** `exec` auth providers and `insecure-skip-tls-verify` from the kubeconfig
  unless `--insecure-kubeconfig-exec` / `--insecure-kubeconfig-tls` are set
  (`fluxcd/pkg/runtime/client/kubeconfig.go`). It neutralizes to a safe subset; it
  does not reject. (We diverge here — see below.)
- **`kubeConfig` composes with `spec.serviceAccountName`** to *impersonate* a
  ServiceAccount on the remote (`system:serviceaccount:<ns>:<name>`), with a
  controller-level `--default-service-account` fallback.
- **Flux does not make `kubeConfig` immutable**, and has **no dedicated
  remote-connectivity condition** — failures surface as generic `Ready=False`.
- **The Flux Operator itself carries no kubeconfig field at all.** It reconciles
  locally and *delegates* remote targeting to the Kustomization/HelmRelease
  objects it installs. This confirms the placement: the kubeconfig belongs on the
  object that owns one materialization, not on an install-wide object.

The one thing we **reuse as code** is the *type*: `spec.kubeConfig`
is Flux's `meta.KubeConfigReference` verbatim (see *Reuse*, next), which brings the
`secretRef`/`configMapRef` CEL with it. Everything else is our own — the
`value`→`value.yaml` key resolution, the kubeconfig safety check, the immutability,
and a legibility condition Flux does without. On the safety check we **diverge on
purpose**: where Flux *silently strips* `exec`/insecure-TLS to a safe subset, we
**reject** a kubeconfig that carries them, so the failure is legible (a
`Validated=False` reason) rather than a quietly-neutered credential that then fails
to connect for reasons the operator cannot see.

## Reuse — one type module in; everything else our own

Both Flux packages are public Go modules under Apache-2.0, and this repo is
Apache-2.0, so reuse is clean (keep upstream attribution / `NOTICE`). We depend on
no `fluxcd` module today. For the first PR **exactly one** Flux import is taken —
`apis/meta` — and nothing else.

**Import the API types — `github.com/fluxcd/pkg/apis/meta`.** This is the exact
`KubeConfigReference` / `SecretKeyReference` / `LocalObjectReference` we would
otherwise hand-write. Reusing it is strictly better:

- **Near-zero weight, and version-aligned.** Its only real dependency is
  `k8s.io/apimachinery`, which we already have. **Pin `v1.31.0`** — it targets Go
  1.26 and Kubernetes 0.36, matching this repo's own dependencies, and its CEL is
  the same generated markers proven in Flux's shipping CRDs.
- **It embeds in our CRD.** It ships `DeepCopy`, and the types are import-free pure
  structs, so controller-gen embeds them and **emits Flux's CEL markers into our
  CRD** — the exactly-one-of `secretRef`/`configMapRef` rule and the key contract
  come from upstream, and a Flux kubeconfig Secret works unchanged.
- **It realizes "shaped for later" by construction** — `configMapRef` is already in
  the type, so provider auth becomes a code-only change, never a schema break.

**Do *not* import `github.com/fluxcd/pkg/runtime` — not even for the sanitizer.**
The `client` package holds the real "connect to an external kube-api" logic (the
`Impersonator`/`GetClient` builder and the `KubeConfig()` sanitizer), but:

- **It is heavy** — it drags in `fluxcd/cli-utils`, `fluxcd/pkg/apis/{acl,event,
  kustomize}`, `cel-go`, `prometheus/client_golang`, and couples our build to
  Flux's release cadence.
- **Its shape is not ours.** It builds a *controller-runtime client* around
  impersonation and provider fetchers; we build a **dynamic client + discovery**
  per cluster, cache/rotate by `resourceVersion`, and deliberately don't dial.
- **Its sanitizer strips; we reject.** The one piece we might have lifted, the
  `KubeConfig()` sanitizer, *silently neutralizes* `exec`/insecure-TLS. We want a
  legible **rejection** instead — a different action, so there is nothing to reuse.
  Our check is a few lines over the parsed kubeconfig in `internal/watch`, takes no
  Flux import, and is documented as a deliberate divergence.

The **one** thing that later justifies importing `pkg/runtime` is **cloud workload
identity** — the `configMapRef` provider path — and it is *two* modules, not one:
`pkg/runtime/client` supplies only the injection **plumbing** (a
`ProviderRESTConfigFetcher` function type; it mints nothing and does not even
depend on the auth module), while `fluxcd/pkg/auth` supplies the actual token
**minting**. What that buys — and the cloud-SDK cost — is spelled out in
*[Remote auth mechanisms](#remote-auth-mechanisms--impersonation-vs-workload-identity)*
below. The other two auth mechanisms are **not** triggers for either import and
should not be conflated with them:

- **`secretRef`** (v1) needs only `clientcmd` + our own reject check — no Flux
  import beyond the `apis/meta` type.
- **`spec.serviceAccountName` impersonation** is ~three lines of client-go —
  `restConfig.Impersonate = rest.ImpersonationConfig{UserName:
  "system:serviceaccount:<ns>:<name>"}`. Flux's `Impersonator` wraps it, but the
  mechanism is trivial; port it, don't import for it.

So: import `pkg/runtime` only for the *provider/workload-identity* path, not for
impersonation.

## What this redesign changes vs the closed PR (#220)

The closed PR shipped a working version of this feature. It was folded into a
seven-feature branch and coupled to feature #6 (a *mutable, retargetable*
destination). This redesign keeps the good structure and removes the coupling.

| Closed PR (#220) | This redesign | Why |
|---|---|---|
| `sourceCluster` **mutable**, part of the retarget lifecycle (`observedDestination`, `retargetingTo`, teardown-before-validation). | `spec.kubeConfig` **immutable**, exactly like `providerRef`/`branch`/`path` are on `main` today. | The source of a folder's content is destination identity. On `main` the destination is already immutable; extending it is the minimal, coherent change. Mutability is retarget's problem (#6), and can subsume it later. |
| Source cluster **stamped onto every `CompiledRule`**; a `spec` change raced rule recompilation, needing `CompiledSourceClusters`, `sourceClusterRulesCaughtUp`, and a "rules disagree → watch nothing" state. | Source cluster is a **GitTarget property captured on `Declare`**, the same way `gitTargetUIDs` already is. | It *is* a GitTarget property; normalize it as one. Because it is immutable, there is no "spec changed, rules haven't caught up" window — the entire race apparatus disappears. |
| `key` **schema-defaulted to `value.yaml`**. | `key` optional, **no schema default**; resolver reads `value` then `value.yaml`. | `value.yaml` alone would **fail** on a standard Flux kubeconfig Secret, whose key is `value`. Matching Flux's fallback order is simpler *and* more compatible. |
| Resolver did a bare `RESTConfigFromKubeConfig(raw)`. | Resolver **rejects** a kubeconfig carrying `exec`/insecure-TLS (legible `Validated=False`), unless flag-opted-in. | An operator-supplied kubeconfig is attacker-adjacent input; an `exec` stanza runs a binary in the operator Pod. Flux *strips* these silently; we reject for legibility. |
| `spec.sourceCluster.kubeConfigSecretRef` (a bespoke wrapper + bare secret ref). | `spec.kubeConfig` — Flux's `meta.KubeConfigReference`, imported inline. | Exact Flux parity (`Kustomization.spec.kubeConfig`), and `kubeConfig.configMapRef` (provider/workload-identity) is already in the imported schema — **no wrapper, no schema break** later. |

Most of the PR's internals are sound and carried forward: the per-cluster
`clusterContext` (now with refcounted teardown), credential rotation on the
refresh cadence, and the parse-don't-dial legibility gate. Two things are **not**
carried over: its bare unsanitized resolver (we reject unsafe kubeconfigs, above)
and its **union GVK→GVR lookup**, which is replaced by target-scoped resolution
because a union is a correctness bug across clusters (see *Architecture*).

## API shape

No wrapper: `spec.kubeConfig` is Flux's own type, **imported, not re-declared**
(see *Reuse*), inline on `GitTargetSpec` — `GitTarget.spec.kubeConfig`, exactly like
`Kustomization.spec.kubeConfig`. Embedding `meta.KubeConfigReference` means the
schema, the `secretRef`/`configMapRef` CEL, and the `value`→`value.yaml` key
contract all come from upstream, and a Secret produced for a Flux `Kustomization`
works here unchanged.

```go
import meta "github.com/fluxcd/pkg/apis/meta"

// spec.kubeConfig is immutable — the source of a folder's content is destination identity.
// +kubebuilder:validation:XValidation:rule="has(self.kubeConfig) == has(oldSelf.kubeConfig) && (!has(self.kubeConfig) || self.kubeConfig == oldSelf.kubeConfig)",message="spec.kubeConfig is immutable; delete and recreate the GitTarget to change the cluster it mirrors"
//
// configMapRef (provider / workload-identity auth) is in meta's schema but not yet
// implemented; reject it at admission so the v1alpha3 contract is "secretRef only". Deleting
// this one rule, plus wiring the provider path, is the whole future enablement.
// +kubebuilder:validation:XValidation:rule="!has(self.kubeConfig) || !has(self.kubeConfig.configMapRef)",message="spec.kubeConfig.configMapRef (provider auth) is not yet supported; use secretRef"
type GitTargetSpec struct {
	// … providerRef, branch, path (unchanged, still immutable) …

	// KubeConfig names the SOURCE CLUSTER this GitTarget mirrors FROM: the kubeconfig
	// determines both the cluster and the credentials to reach it. Omitted means the cluster
	// the operator runs in, the single-cluster default. Its Secret is read from the
	// GitTarget's OWN namespace, on the cluster the operator runs in — the credential for a
	// cluster never has to live on that cluster. When SecretRef.Key is empty the resolver
	// reads "value" then "value.yaml", Flux's order (see Resolver, below). Immutable: the
	// source of a folder's content is part of what the folder means; delete and recreate to
	// change it. Only kubeConfig.secretRef is honored in v1alpha3.
	// +optional
	KubeConfig *meta.KubeConfigReference `json:"kubeConfig,omitempty"`
}
```

The two CEL rules live at *our* `GitTargetSpec` level — Flux's type carries neither
(no immutability, no provider guard), so nothing upstream is forked — and both use
`has()` guards because `kubeConfig` is optional. `meta.KubeConfigReference` already
carries the paired "exactly one of `configMapRef`/`secretRef`" CEL, so with our
guard blocking `configMapRef`, `secretRef` is effectively required: an empty
`kubeConfig` is rejected at admission, not discovered at watch time.

## Architecture

The changes are confined to the watch manager and the `GitTarget` controller. The
git write path is unchanged except for making GVK→GVR resolution source-cluster
scoped.

### The cluster context

The watch manager grows a **cluster context**: the set of things that were
Manager-wide singletons and are in fact properties of *one* cluster — its API
surface, the followability decisions derived from it, the clients that reach it,
and the informers that report its surface moved.

```go
type clusterContext struct {
	id             string          // "" for the local cluster, else "<ns>/<name>/<key>"
	catalog        *APIResourceCatalog
	registry       *typeset.Registry
	restConfig     *rest.Config
	configVersion  string          // the kubeconfig Secret's resourceVersion
	dynamicClient  dynamic.Interface
	discovery      apiResourceDiscovery
	triggerFactory dynamicinformer.DynamicSharedInformerFactory
	// … per-cluster edge-triggered logging state …
}
```

- `Manager.clusters map[string]*clusterContext`. A zero-value Manager (unit
  tests, and **every** single-cluster install) creates exactly one, keyed
  `LocalClusterID = ""`. Nothing that does not know about source clusters changes
  behavior — it all lands on the local context.
- The catalog refresh runs for every **active** cluster — the local one plus every
  cluster some `GitTarget` currently points at. It returns the *local* cluster's
  error only: a remote that cannot be reached fails **its own** `GitTarget`s
  (through their unready registries), never the local cluster's.
- Watch and list opens take the cluster id, so each `(GitTarget, GVR, namespace)`
  watch runs against the right cluster.
- A `GitTarget`'s rules resolve against **its own cluster's** type registry. A CRD
  installed only on the remote is followable only there — and, more importantly, a
  type served only *locally* never resolves for a remote target. Mirroring the
  wrong cluster into a folder is worse than mirroring none.
- **Contexts are reference-counted and torn down.** A `clusterContext` is created on
  first use by a `GitTarget` that names it, and **torn down when the last such
  `GitTarget` is gone** — its trigger informers stopped, its discovery/dynamic
  clients closed, its catalog and registry dropped. The local context is never torn
  down. Without refcounted teardown a deleted remote `GitTarget` would leak a
  discovery stream, an informer factory, and a client set for the life of the
  process; this is called out because PR #220 created contexts lazily but is worth
  auditing for the symmetric teardown. It needs a test (see *Build order*).

Because `spec.kubeConfig` is immutable, the manager learns a `GitTarget`'s cluster
once, on `DeclareForGitTarget`, and stores it keyed by `GitTarget` — the existing
`gitTargetUIDs` pattern. No per-rule propagation, no cross-rule disagreement
window.

### Cluster identity and keying

The cluster id the data plane keys on is `<namespace>/<name>/<key>` — the
config-plane namespace, the Secret name, and the Secret key **as written in spec**
(empty is its own identity). The key is part of the identity because two
`GitTarget`s naming one Secret under different keys are pointed at different
kubeconfigs, and so at different clusters. Two `GitTarget`s naming the *same*
Secret+key share one `clusterContext` — one catalog, one client set, one discovery
stream — which is the efficient outcome.

Identity is the *reference*, not the kubeconfig *contents*. Consequence: two
different Secrets that happen to point at the same physical cluster get two
contexts (duplicate discovery). This is accepted — each context is independent and
correct; deriving identity from contents (server URL, cluster UID) is fragile
under HA and rotation. Rotating a Secret's **contents** is transparent (same id,
fresh credential); renaming the Secret is a new cluster and requires recreating
the `GitTarget`.

### Credential resolution, rotation, and sanitization

A `SourceClusterResolver` turns a cluster id into a `rest.Config` by reading the
named Secret from the config plane:

1. Parse the id back to `{namespace, name, key}`.
2. `Get` the Secret (cache-bypassing read, so a rotation is seen without a Secret
   informer — same reasoning as the existing SOPS-key reads).
3. Select the kubeconfig bytes: explicit `key` → else `value` → else `value.yaml`
   → else error (Flux's order).
4. **Reject** an unsafe kubeconfig before building the config. Flux's
   `pkg/runtime/client` *silently strips* `exec` auth providers and
   `insecure-skip-tls-verify`; we instead **fail** a kubeconfig that carries them,
   with a legible `Validated=False` reason, unless the operator opts in with
   `--insecure-kubeconfig-exec` / `--insecure-kubeconfig-tls`. Explicit rejection
   over silent stripping is a **deliberate divergence** — a stripped-but-accepted
   kubeconfig that then fails to connect is exactly the illegible failure the
   legibility gate exists to prevent. This is our own check (not a port), so it
   takes no Flux import.
5. Build the `rest.Config`, apply `--source-cluster-qps` / `--source-cluster-burst`
   (a remote is reached over a network the local one is not), **drop the bytes**,
   and remember only the Secret's `resourceVersion` as the version token.

Rotation is picked up on the catalog-refresh cadence (every 30s and on every rule
change), not on the hot watch-reconnect path: one Secret read per cluster per
refresh, not one per watch. When the token changes, the cached clients are
dropped and the next use rebuilds them; a watch already streaming on the old
credential keeps working until that credential stops being accepted, and the
reconnect that follows picks up the rebuilt client.

### GVK→GVR resolution is scoped to the source cluster (not a union)

When a branch worker scans the manifests already in a Git folder to answer "what
resource is this document?", it must resolve GVK→GVR against **the source cluster
that folder mirrors** — not a union of all clusters.

PR #220 used a single **union** lookup (local first, then remotes; first answer
wins), reasoning that GVK→GVR is stable across clusters. **That is not safe.** Two
clusters can validly serve the same GVK under **different plural resources or
scopes** — two independently-authored CRDs both defining `example.io/v1 Widget`
with different `.spec.names.plural`, or one namespaced and one cluster-scoped.
"First answer wins" would then index or sweep a folder's documents against the
wrong cluster's mapping, **mis-filing or deleting** manifests. This is a
correctness bug, not an efficiency one, so the union is dropped.

The mapping is **target-scoped**. A folder is owned by exactly one `GitTarget` (one
materialization), so folder → `GitTarget` → source cluster is determined, and the
worker resolves each document against **that** cluster's `typeset.Registry`. This
costs the plumbing the union was avoiding: a branch worker is keyed by
(provider, branch) and may serve several `GitTarget`s with different source
clusters, so the resolver cannot be one worker-wide field. Instead each pending
write and each folder scan carries its originating `GitTarget`'s source-cluster id,
and the worker looks up the mapping for that id — the extra threading is the price
of correctness, and is paid deliberately. There is **no** union and **no**
first-wins fallback; a document whose owning cluster's registry does not know its
type is refused by the acceptance gate, exactly as in the single-cluster case. In a
single-cluster install every target resolves against the one local registry,
unchanged.

### The legibility gate

The `GitTarget` controller reads and parses the kubeconfig before any watch opens
against it, and reports **`Validated=False`** with a specific `KubeConfig*` reason
(*Status and conditions*) when the Secret is missing, empty at its key, unparseable,
or fails the exec/TLS safety policy. This is a **legibility** gate, not a security
one, and it diverges from Flux (which surfaces this only as a generic `Ready=False`):
without it, a typo'd Secret name surfaces only as a stalled data plane and a
repeating log line; with it, the `GitTarget` says exactly which input was wrong.

It deliberately does **not** dial the cluster — so it only ever sets `Validated`,
never reachability. Whether the API server can actually be *contacted* is a runtime
observation the data plane records on `SourceClusterReachable` after real discovery;
an unreachable-right-now cluster is a transient it retries, and a controller that
blocked on a network round trip would stall every other `GitTarget` behind it. This
split — inputs on `Validated`, reachability on `SourceClusterReachable` — is the one
the earlier draft got wrong by folding both into `Validated=False /
SourceClusterUnreachable`.

## Security and RBAC

- The operator needs `get` on Secrets in the namespaces where `GitTarget`s live —
  which it already has, for `GitProvider.spec.secretRef` and the SOPS age keys.
- The credential for a remote cluster lives **only** in the config plane. The
  watched cluster holds nothing but the watched resources.
- On the remote cluster, the kubeconfig's identity needs only **read** access to
  the mirrored types (`get`/`list`/`watch`), plus `apiextensions`/`apiregistration`
  read for discovery. Least privilege is the operator's to grant on the remote. The
  minimal ClusterRole the kubeconfig's identity must be bound to on the **source**
  cluster (bind it to whichever ServiceAccount/user the kubeconfig authenticates as):

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRole
  metadata:
    name: gitops-reverser-source-read
  rules:
    # Discovery: what types does this cluster serve, and are they followable?
    - apiGroups: ["apiextensions.k8s.io"]
      resources: ["customresourcedefinitions"]
      verbs: ["get", "list", "watch"]
    - apiGroups: ["apiregistration.k8s.io"]
      resources: ["apiservices"]
      verbs: ["get", "list", "watch"]
    # The mirrored types. `["*"]` grants read on every type; narrow it to exactly the
    # groups/resources the WatchRules select for a true least-privilege mirror.
    - apiGroups: ["*"]
      resources: ["*"]
      verbs: ["get", "list", "watch"]
  ```

  The operator never writes to the source cluster, so no write verb is ever needed;
  narrowing the wildcard rule to the selected types is the recommended hardening.
- Kubeconfig **rejection** (above) closes the `exec`-provider and insecure-TLS holes
  an operator-supplied kubeconfig would otherwise open.
- Remote clients carry client-side throttling the in-cluster config does not by
  default.

### Credential-reference authorization — the namespace is the boundary

The operator reads the kubeconfig Secret with **its own** credentials, not the
spec author's. Under ordinary namespace RBAC that is harmless: `spec.kubeConfig.secretRef`
is resolved **only from the GitTarget's own namespace** (there is no cross-namespace
Secret reference), and whoever can create a `GitTarget` in namespace *N* can already
read Secrets in *N* — so naming one grants nothing new. **The namespace is the trust
boundary**, exactly as it already is for `GitProvider.spec.secretRef` (Git credentials),
which has lived in one trust zone since day one. This is the model the feature ships with,
and it is picked up entirely on the reconcile loop — no admission webhook.

The one residual case this does *not* close is a **fine-grained intra-namespace RBAC
split**: a subject granted create/update on `GitTarget` in namespace *N* but **denied**
Secret-read in that same namespace could name a privileged kubeconfig Secret they cannot
themselves read, and the operator — which can — would mirror that remote cluster's state
into a Git destination the subject controls (a confused-deputy read escalation). This
requires an unusual RBAC posture (write-GitTarget-yes, read-Secret-no, same namespace); a
namespace is normally a single tenant's compartment, so the split rarely exists in
practice. **It is deliberately not closed by an admission webhook.** An earlier revision of
this design guarded it with a fail-closed `SubjectAccessReview` at admission on the
requesting user's `get` of the named Secret; that was removed because the product model is
**all configuration is picked up on the reconcile loop, like every other resource** — and
a reconcile runs as the operator ServiceAccount, with no requesting-user identity to review.

If self-service multi-tenant remote clusters with per-subject Secret isolation *inside* one
namespace ever becomes a real requirement, the right shape is **not** a per-field admission
webhook but a platform-owned `ClusterConnection`/`SourceCluster` CRD that only platform
admins may create (RBAC on the *object*, which the reconcile loop can honor natively),
referenced by name from the `GitTarget`. That keeps the "everything reconciles" model intact
while moving the credential out of the tenant's write reach. Deferring workload identity and
impersonation (see *Remote auth mechanisms*) is partly downstream of this: both widen what a
chosen credential/identity can reach, so neither should land before that CRD, if it is ever
needed.

## Explicitly out of scope (and why it's safe to defer)

- **Author attribution across clusters.** Live object *state* comes from watches,
  which work against a remote cluster. But *who* made a change is joined from
  apiserver **audit** events, and the audit webhook is now local-only (the
  cluster-id path was removed). So a remote-cluster mirror commits as the
  configured committer, not the human author — correct, just less rich.
  Per-cluster audit ingestion is a separate feature; this design does not block
  it, and notes the boundary rather than pretending to solve it.
- **Provider / workload-identity auth (`kubeConfig.configMapRef`).** Deferred, and
  the schema already carries it (rejected by CEL for now). It is genuinely useful
  later — no long-lived kubeconfigs — but it turns the operator's cloud/OIDC
  identity into a powerful access broker, so it is **gated on the
  credential-reference authorization model** (*Security*) landing first.
- **ServiceAccount impersonation (`spec.serviceAccountName`).** Flux's remote
  impersonation, and **not in the v1 API at all**. A read-only mirror does not need
  it — a kubeconfig identity with narrow remote RBAC is simpler and safer — and
  letting the spec author choose the impersonated ServiceAccount can itself be an
  **escalation path** (the connecting identity must hold remote `impersonate`, and
  the author picks who requests run as). Deferred until there is a concrete need and
  the authorization model above.
- **A mutable / retargetable source cluster.** Owned by feature #6 (retarget).
  When that lands and makes the destination mutable, it can generalize to cover
  `spec.kubeConfig` too. Until then, immutable is the honest, simple contract.

## Status and conditions

`GitTarget` already carries a kstatus-shaped set — `Ready` (aggregate),
`Reconciling`/`Stalled`, `Validated`, `EncryptionConfigured`, `GitPathAccepted`,
`RenderMatchesLive`, `StreamsRunning`
([`constants.go`](../../internal/controller/constants.go),
[`gittarget_controller.go`](../../internal/controller/gittarget_controller.go)). The
source-cluster split **slots into it**, and two principles keep it legible:

1. **Input validation and network reachability are different conditions.** A missing
   or malformed kubeconfig is a *spec* problem the controller sees without touching
   the network; an unreachable API server is a *runtime* observation. The earlier
   draft folded both into `Validated=False / SourceClusterUnreachable` — that
   conflation is corrected here.
2. **`SourceClusterUnreachable` is a *Reason*, not a condition *Type*.** The type is
   **`SourceClusterReachable`**, which names the current state without implying a
   held-open connection.

### The condition set

| Condition | Meaning | Change |
|---|---|---|
| `Validated` | Spec + **directly readable** inputs valid: provider/branch/path policy, and now — kubeconfig Secret & key exist, the kubeconfig parses, and it passes the exec/TLS safety policy. **No network dial.** | extended |
| `SourceClusterReachable` | The controller can actually reach the configured source API. `True` (reason `LocalCluster`) when `kubeConfig` is omitted; `Unknown` before first discovery; `False` after a real failed attempt. | **new** |
| `StreamsRunning` | Every selected source type has completed initial replay and its watch is healthy — strictly stronger than mere reachability. | existing |
| `GitProviderReady` | The referenced `GitProvider` is currently ready (its own credential/repository check), **projected** onto the target so one `kubectl get gittarget` separates source-side from destination-side failure. | **new (projection)** |
| `GitPathAccepted` | Git-tree / worktree write-safety state. | existing |
| `EncryptionConfigured` / `RenderMatchesLive` | Encryption-setup and render-fidelity, where enabled. | existing |
| `Ready` | Aggregate of the required conditions above, plus encryption/render where enabled. | extended |
| `Reconciling` / `Stalled` | Generic kstatus progress / needs-action, as today. | existing |

### Reasons, grouped by the condition they set

- **`Validated=False`** (input, no dial): `KubeConfigSecretNotFound`,
  `KubeConfigKeyNotFound`, `KubeConfigInvalid`, `KubeConfigExecNotAllowed`,
  `KubeConfigInsecureTLSNotAllowed` — alongside the existing `ProviderNotFound`,
  `BranchNotAllowed`, `TargetConflict`, …
- **`SourceClusterReachable=False`** (after a real API attempt):
  `SourceClusterUnreachable` (DNS/TCP/TLS/timeout), `SourceClusterAuthenticationFailed`
  (401), `SourceClusterAccessDenied` (403 during discovery). `True` reason
  `LocalCluster` for the omitted-`kubeConfig` case.
- **`StreamsRunning=False`**: `InitialReplay`, `WatchError`, `WatchNotPermitted`.
- **`GitProviderReady=False`**: `GitProviderNotReady`.

The load-bearing distinction: a missing/malformed kubeconfig is **`Validated=False`**;
an otherwise-valid kubeconfig whose API server cannot be contacted is
**`SourceClusterReachable=False` / `SourceClusterUnreachable`**.

### Where each is set

- `Validated` (+ the `KubeConfig*` reasons) is set by the **GitTarget controller**
  during reconcile — it reads/parses the Secret and applies the exec/TLS policy but
  **does not dial** (this is the legibility gate in *Architecture*, now correctly
  scoped to inputs only).
- `SourceClusterReachable` is set by the **watch data plane** on the catalog-refresh
  path — discovery is the first thing that actually talks to the source API. It
  starts `Unknown`, goes `True` on first successful discovery, and `False` with the
  reason matching the failure class. The controller never blocks on a dial to
  compute it.
- `StreamsRunning` is set as today, from per-type watch replay, computed against the
  **source cluster's** registry.
- `GitProviderReady` reuses the projection pattern the codebase already has for
  `WatchRule` → `GitTargetReady`
  ([`ConditionTypeGitTargetReady`](../../internal/controller/constants.go)): the
  GitTarget reads the referenced GitProvider's `Ready` and mirrors it. It requires
  **reconciling the GitTarget when the GitProvider's status changes** — a
  `Watches(&GitProvider{}, …)` mapping (as `WatchRule` already watches `GitTarget`),
  with the 5-minute periodic reconcile as the fallback.

### Git side: owned by GitProvider, projected here

Repository connectivity stays **owned by `GitProvider`** — its `Ready` already means
"the periodic repository-connectivity check passes". The GitTarget does not re-check
the repo; it **projects** that readiness as `GitProviderReady` and folds it into
`Ready`, so one `kubectl get gittarget` says whether a stall is source-side
(`SourceClusterReachable=False`) or destination-side (`GitProviderReady=False`).

**Caveat, and a deliberate deferral.** `GitProviderReady=True` means the provider's
periodic check succeeds; it does **not** prove every individual push lands. If
`Ready` should later mean "writes are actively succeeding", that is a separate
`GitDeliveryHealthy` condition driven by the branch worker — explicitly **out of
scope** here, so `Ready`'s meaning stays honest ("inputs valid, source reachable,
streams replaying, provider healthy") rather than overclaimed.

No `observedDestination` / `retargetingTo` — those belong to #6.

## Migration and compatibility

- `spec.kubeConfig` is a new optional field. Existing installs are unaffected:
  absent → local cluster → today's behavior, byte for byte.
- This design intentionally closed the older `SourceCluster` CRD proposal. The later
  [`ClusterProvider` attribution design record](multi-cluster-author-attribution.md) explains why
  the source connection was subsequently given a cluster-scoped home.

## Future shape (designed-in, not built)

Because we embed `meta.KubeConfigReference` whole, the `configMapRef` arm is
**already in the shipped schema**; enabling provider / workload-identity access is
not a type change at all. It is:

1. **delete the one `configMapRef`-unsupported CEL guard** on `GitTargetSpec`; and
2. **wire the provider path in the resolver, `generic` first** — import
   `pkg/auth/generic` **directly** (no cloud SDKs — see *Remote auth mechanisms*),
   mint the token, and build the `rest.Config` in our own resolver from the
   ConfigMap's `address`/`ca.crt`. `pkg/runtime/client`'s `ProviderRESTConfigFetcher`
   wiring is *optional* here: it fits flux's Impersonator, but a from-scratch
   resolver can call the auth provider directly and skip that import. Add each cloud
   (`pkg/auth/aws`, …) à la carte afterwards. A v1alpha3 object using `secretRef`
   stays valid across the change.

`spec.serviceAccountName` (Flux's remote impersonation) composes on top
independently — it is a few lines of client-go (`rest.ImpersonationConfig`), needs
no `pkg/runtime` import, and can be added whether or not the provider path ever is.

And if reuse across many `GitTarget`s or platform-admin ownership ever justifies a
platform-owned connection object, it arrives as a **sibling** of `spec.kubeConfig`,
not by reintroducing a wrapper: a `spec.sourceClusterRef` naming a
`ClusterConnection`/`SourceCluster` CRD, **mutually exclusive** with `spec.kubeConfig`
via one CEL rule. The inline `kubeConfig` path stays; the ref is additive. This is
the concrete reason the v1 wrapper was not worth keeping — the anticipated second
option is a peer of `kubeConfig`, never something nested beside it.

```go
type GitTargetSpec struct {
	// … one of kubeConfig or sourceClusterRef (future) …
	// +optional
	KubeConfig       *meta.KubeConfigReference `json:"kubeConfig,omitempty"`
	// +optional
	SourceClusterRef *ClusterConnectionReference `json:"sourceClusterRef,omitempty"` // future
}
// +kubebuilder:validation:XValidation:rule="!(has(self.kubeConfig) && has(self.sourceClusterRef))",message="set at most one of spec.kubeConfig or spec.sourceClusterRef"
```

## Remote auth mechanisms — impersonation vs workload identity

Two mechanisms sit beyond the v1 `secretRef`. They are **different axes**, not two
flavors of one thing, and keeping them straight is what makes deferring both safe.

- **Workload identity** (`kubeConfig.configMapRef`) answers *"how does the operator
  **authenticate** to the remote apiserver without a stored kubeconfig?"* The
  operator's own pod cloud identity (EKS/GKE/AKS, or any OIDC issuer) is exchanged
  for a short-lived token to the named cluster. It **replaces the Secret** as the
  credential source.
- **ServiceAccount impersonation** (`spec.serviceAccountName`) answers *"once
  connected, whose **RBAC** do my requests run under?"* It adds an `Impersonate-User`
  header so the apiserver evaluates permissions as `system:serviceaccount:<ns>:<name>`
  instead of the connecting identity. It **narrows** whatever credential you used; it
  is not a credential itself.

They compose: `kubeConfig` (secret or provider) sets *who you are*, optional
`serviceAccountName` sets *who you act as*.

**Why they matter differently to us.** We only ever `list`/`watch`/`get` the source
cluster — we never write to it.

- **Workload identity is a real win**: it lets an operator mirror managed clusters
  with zero stored kubeconfigs. It is the path worth an import when we build it —
  but it makes the operator's cloud/OIDC identity a **powerful access broker**, so
  it is gated on the credential-reference authorization model (*Security*) first.
- **Impersonation is mostly redundant for us, and carries its own escalation risk**:
  Flux needs it to scope *writes* on behalf of tenants; a read-only mirror gets the
  same containment by simply giving the kubeconfig's own identity a read-only
  ClusterRole. Worse, allowing the spec author to choose the impersonated
  ServiceAccount is itself an escalation surface (the connecting identity must hold
  remote `impersonate`, and the author picks whom requests run as). It stays a
  defence-in-depth nicety at best, never a requirement.

### What reusing Flux buys for the provider path (and what it doesn't)

The provider path is **two** modules — worth being exact about before signing up for
either:

- **`pkg/runtime/client` is only plumbing.** It defines the
  `ProviderRESTConfigFetcher` function *type* and an injection point on its
  impersonator; it mints no tokens and does not depend on the auth module. Importing
  it alone gives you a socket, not a provider.
- **`fluxcd/pkg/auth` is where the minting lives**, and its `utils.ProviderByName`
  switch implements exactly **four** cluster-auth providers: **`aws`, `azure`, `gcp`,
  and `generic`**. So it is *not only the big three* — but it is a fixed set of four
  in a `switch`, not an open plugin ecosystem.
- **`generic` is the open one.** It is cloud-agnostic OIDC: it exchanges a standard
  Kubernetes ServiceAccount projected token (via `coreos/go-oidc`) for access to any
  apiserver that trusts a conformant OIDC issuer — self-hosted, on-prem, or a managed
  cluster outside the big three. That is the "more open providers" answer: the escape
  hatch from the three clouds is `generic`, not a fourth vendor.
- **`pkg/auth`'s other providers are a different axis.** It also ships `githubapp`,
  `actionsoidc`, and `jwt` — but those authenticate to **git hosts / CI**, not to a
  remote kube-apiserver, so we would get no remote-cluster auth "for free" from them
  (they could only ever matter to our *GitProvider* surface, a separate feature).
- **The cost is a cloud-SDK dependency surface — but it is opt-in per provider.**
  `pkg/auth`'s `go.mod` requires the AWS SDK v2, the Azure SDK, and the Google Cloud
  SDK, but those are reachable **only** through the `aws`/`azure`/`gcp` subpackages
  and the `utils` registry that switches over all four. Verified: the production
  `pkg/auth/generic` package (and the `pkg/auth` root it imports) pull **no cloud
  SDK at all** — only `golang-jwt/jwt/v5` and `k8s.io/*` / `controller-runtime`
  packages we already have.

**This makes a `generic`-first rollout the recommended entry point**, and clean:
import `pkg/auth/generic` **directly** and write a one-case
`ProviderRESTConfigFetcher` (for `provider: generic`) — do **not** import
`pkg/auth/utils`, which is the `switch` that references all four providers and so
drags in every cloud SDK. Because the providers are independent subpackages, each
cloud is then addable **à la carte** later: importing `pkg/auth/aws` accepts exactly
the AWS SDK and nothing else. So the staged path is `secretRef` → `generic` OIDC
(near-zero added weight) → individual clouds as demand appears, and the `provider`
value lives in the referenced ConfigMap (not a CRD field), so an unsupported
provider is a clear resolver error, not a schema change.

## Build order

Each step leaves the system correct and is independently reviewable.

1. **API types + CRD** — add **only** `github.com/fluxcd/pkg/apis/meta` (pin
   **v1.31.0**) to `go.mod`; `spec.kubeConfig *meta.KubeConfigReference` **inline on
   `GitTargetSpec`** (no wrapper); the immutability + `configMapRef`-guard CEL;
   `task manifests`. (Confirm controller-gen emits meta's embedded CEL into our CRD;
   it should, since meta ships DeepCopy and the markers travel with the type.)
2. **`clusterContext`** — extract the Manager-wide catalog/registry/clients/triggers
   into a per-cluster context keyed `LocalClusterID`; pure refactor, no behavior
   change, fully unit-testable. Include **refcounted teardown** — the last
   `GitTarget` leaving a cluster stops its informers and closes its clients.
3. **Resolver** — `SourceClusterResolver` (parse id → read Secret → key fallback →
   **reject unsafe** → build config → drop bytes → version token). The reject check
   is our own code in `internal/watch` (no Flux import); add the
   `--source-cluster-qps/-burst` and `--insecure-kubeconfig-exec/-tls` flags.
4. **Wire the cluster into declare** — capture a `GitTarget`'s source cluster on
   `DeclareForGitTarget` (the `gitTargetUIDs` pattern); resolve rules and open
   watches against that cluster's context; **target-scoped GVK→GVR resolution** for
   the writer (each write carries its cluster id — *not* a union).
5. **Credential-reference authorization** — the namespace is the boundary
   (*Security*): `spec.kubeConfig.secretRef` resolves only from the GitTarget's own
   namespace, so the model is the same one `GitProvider.spec.secretRef` already uses.
   No admission webhook — all validation is on the reconcile loop.
6. **Status conditions** (*Status and conditions*) — `Validated` extended with the
   `KubeConfig*` reasons in the controller (inputs, no dial); the new
   `SourceClusterReachable` set from the data plane's discovery (`Unknown` →
   `True`/`False`); and `GitProviderReady` projected from the referenced provider,
   with a `Watches(&GitProvider{})` trigger, folded into `Ready`.
7. **Docs + tests** — the minimal remote-read ClusterRole; the e2e/integration
   scenarios below; retire the stale CRD proposal.

## E2E and integration test plan

The e2e harness ([`test/e2e/`](../../test/e2e/)) is kubectl-driven — CRs are rendered
YAML applied to the cluster. The source-cluster corner is its own gated leg
(`task test-e2e-source-cluster`, label `source-cluster`), the same idiom as
`E2E_ENABLE_BI_DIRECTIONAL`. Two consequences shape this plan:

- **The reachability/validation scenarios need no separate cluster.** Input validation
  and the `Validated`/`SourceClusterReachable` split exercise the source path against a
  kubeconfig Secret whose server is unreachable — no genuinely remote cluster required.
- **The mirror and GVK→GVR scenarios use real REMOTE clusters — kcp workspaces.**
  Rather than provision a second k3d cluster, the corner installs kcp
  ([`test/e2e/setup/kcp`](../../test/e2e/setup/kcp)) **by Flux**, like every other e2e
  dependency, and mirrors kcp *workspaces*: cheap logical clusters, each a real
  Kubernetes API reached at
  `frontproxy-front-proxy.kcp.svc.cluster.local:6443/clusters/<hash>` over verifiable
  TLS. Because kcp allows isolated copies of the same Kind across workspaces, three
  workspaces holding the *same* namespace + resource is the cheapest possible proof that
  state is keyed by source cluster — no second k3d, no two disagreeing CRDs to hand-wire.
  The harness lives in
  [`test/e2e/kcp_workspace_test.go`](../../test/e2e/kcp_workspace_test.go); the specs
  Skip when kcp is absent, so a default `task test-e2e` never runs them.

### The scenarios

1. **Input validation is legible, and does not dial** (single cluster). For each bad
   input, apply a `GitTarget` whose `spec.kubeConfig.secretRef` names it and assert
   `Validated=False` with the exact reason: Secret absent → `KubeConfigSecretNotFound`;
   key absent → `KubeConfigKeyNotFound`; not a kubeconfig → `KubeConfigInvalid`; an
   `exec` auth provider → `KubeConfigExecNotAllowed`; `insecure-skip-tls-verify` →
   `KubeConfigInsecureTLSNotAllowed`. Guards: the reject-not-strip posture, and that
   the controller never blocks on the network to reach these verdicts.

2. **The `Validated` vs `SourceClusterReachable` split** (single cluster). Give a
   Secret a **valid** kubeconfig whose server is unroutable (`https://192.0.2.1:6443`,
   RFC 5737 TEST-NET). Assert `Validated=True` (inputs are fine) **and**
   `SourceClusterReachable=False` / `SourceClusterUnreachable`. This is the exact
   conflation the earlier draft had, pinned as a test so it cannot regress.

3. **Omitted `kubeConfig` is unchanged local behavior** (single cluster). A
   `GitTarget` with no `kubeConfig` mirrors as today, and reports
   `SourceClusterReachable=True` reason `LocalCluster`. The single-cluster
   compatibility guard.

4. **Remote round-trip through a kcp workspace** (real remote). Create a kcp workspace,
   put a ConfigMap in it, point a `GitTarget`'s `kubeConfig` at that workspace, wire a
   `WatchRule` for ConfigMaps, and assert it mirrors to Git with
   `SourceClusterReachable=True` and `StreamsRunning=True`. Exercises the full resolver →
   `clusterContext` → per-cluster discovery → watch → target-scoped writer path against an
   actually-remote API (a workspace is a distinct logical cluster), not a self-referencing
   in-cluster fake — so it can tell a mistakenly-local watch from a remote one.

5. **Credential rotation is transparent** (deferred). Rotate the Secret's contents (new
   token, same cluster). Assert mirroring continues, the clients rebuild once, and the
   cluster identity is **unchanged** — no retarget, same folder. Not yet implemented as a
   spec; the rotation-on-refresh-cadence path is unit-tested
   (`refreshClusterCredentials`).

6. **Credential-reference authorization is namespace-scoped** (single cluster).
   `spec.kubeConfig.secretRef` resolves only from the GitTarget's own namespace — a
   Secret named in another namespace is never read. Assert a same-namespace reference
   validates and a cross-namespace name does not resolve. The boundary is namespace RBAC
   (whoever can write GitTargets in *N* can already read Secrets in *N*), not an
   admission check (*Security*).

7. **`GitProviderReady` projection** (single cluster). Make the referenced
   `GitProvider` un-ready (bad repo URL). Assert the `GitTarget` reflects
   `GitProviderReady=False` and `Ready=False`, and recovers when the provider does —
   which also exercises the `Watches(&GitProvider{})` reconcile trigger.

8. **Source-cluster identity is load-bearing — the centerpiece** (three kcp workspaces).
   Three workspaces each hold the **same namespace + the same resource name**
   (`<ns>`/`ConfigMap`/`shared`) with **different content**; three `GitTarget`s mirror
   them into three folders. **Assert three distinct files, each carrying its own
   workspace's value.** If the operator keyed state by `(namespace, GVR)` alone — a union
   / first-wins lookup (PR #220) — the three identical identities would collapse into one;
   that they land as three distinct, correctly-valued files is the proof that everything is
   keyed by source cluster. This is the same invariant the original two-disagreeing-CRDs
   plan targeted, proven more directly and far more cheaply with kcp's isolated-per-workspace
   API surfaces. Verified sound at the code level too:
   [`typeset`](../../internal/typeset/observe.go) derives the GVR from the served resource
   name and the scope from `Namespaced`, and refuses a *within-registry* GVK→two-GVR clash
   ([`funnel.go`](../../internal/typeset/funnel.go) `ReasonGVKNotUnique`) — so a clash can
   only arise from a cross-cluster union, which the scoped resolver rules out.

### Unit / integration coverage (faster, no cluster)

- **Target-scoped resolver** — the load-bearing correctness fix deserves a unit test
  too: build two `typeset.Registry` instances that map one GVK to different GVRs/scopes,
  and assert the writer's GVK→GVR resolution returns each target's *own* cluster's
  answer, and that there is no union/first-wins path. This reproduces #8's core in
  milliseconds and pins the invariant independent of the two-cluster e2e.
- **Resolver** — key fallback (`value` → `value.yaml`), exec/insecure-TLS rejection,
  bytes-dropped-after-build, `resourceVersion` version token.
- **`clusterContext` refcount** — creation on first use, teardown (informers stopped,
  clients closed) when the last referencing `GitTarget` leaves.

## Open questions

1. **Key in the identity vs. resolved key.** Identity uses the spec key verbatim
   (empty is its own id), while the resolver falls back `value`→`value.yaml`. Two
   `GitTarget`s — one with `key: value`, one with the key omitted — reach the same
   kubeconfig but get two contexts. Accept the duplicate, or canonicalize the id
   after the first successful read? (Proposal: accept it; canonicalizing couples
   identity to a network read.)
2. **Credential-reference authorization shape** (*Security*). **Decided:** the
   namespace is the boundary — `secretRef` resolves only from the GitTarget's own
   namespace, same as `GitProvider.spec.secretRef`, and everything is picked up on the
   reconcile loop (no admission webhook). The fine-grained intra-namespace RBAC split is
   left open by design; if per-subject Secret isolation inside one namespace is ever a
   real requirement, the answer is a platform-owned `ClusterConnection` CRD (RBAC on the
   object) referenced by name — never a per-field admission webhook.
3. **Unsafe-kubeconfig default.** Ship `exec`/insecure-TLS **rejected by default**
   (proposed, diverging from Flux's silent strip) with opt-in flags, or warn-and-
   allow? Rejecting is the safe default; confirm it will not surprise operators who
   rely on an `exec` auth plugin in a kubeconfig.
4. **Least-privilege ClusterRole shape on the remote.** Ship a documented broad
   read ClusterRole, or require the operator to grant per-type read and degrade
   unobservable cells gracefully (ties into
   [`watch-and-catalog-architecture.md`](../design/watch-and-catalog-architecture.md) §1.7)?
5. **`GitProviderReady` projection trigger and `Ready` gating.** Wire a
   `Watches(&GitProvider{})` so a provider going un-ready promptly re-reconciles its
   GitTargets (best, but adds an edge to the set that
   [`reconcile-triggering.md`](../design/reconcile-triggering.md) tracks), or lean on the
   5-minute periodic reconcile (simpler, laggier)? And does
   `SourceClusterReachable=Unknown` (pre-first-discovery) hold `Ready` at `Unknown`,
   or is `Ready` allowed to settle on the other axes first? (Proposal: `Watches`, and
   `Unknown` holds `Ready=Unknown` — an unconfirmed source is not yet Ready.)
