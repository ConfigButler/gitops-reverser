# Addressing a differently-named namespace from a WatchRule

> **design** — nothing here is built. Companion to `upstream-wishlist.md` #14 and
> [`../finished/config-plane-split.md`](../finished/config-plane-split.md).
> Written 2026-07-19, revised 2026-07-20. Index: [`../INDEX.md`](../INDEX.md)

## The ask

The config-plane split (v0.38.0) lets one reverser watch many source clusters, but a `WatchRule`
watches the namespace named *identically to its own* on that cluster — verbatim, no remapping
([`rulestore/store.go:127`](../../internal/rulestore/store.go#L127),
[`watch/target_watch.go:876`](../../internal/watch/target_watch.go#L876)).

So to watch `repo-config` on a workspace, the config-plane `WatchRule` must live in a namespace
*called* `repo-config` — and in a shared management cluster those names collide across tenants.
#14 asks for a field to break the coupling.

**Shape of the answer:** an optional `WatchRule.spec.sourceNamespace`, permitted only for source
namespaces the platform admin has listed in `ClusterProvider.spec.allowedSourceNamespaces`
(deny-by-default). One mechanism, uniform across in-cluster and remote providers.

---

## 1. Locality is a fact; permission is a field

Two things get conflated here, and separating them is what makes the rest simple.

### The fact: a provider is in-cluster iff `kubeConfig` is empty

`ClusterProvider` has **no reserved name and no local-vs-remote flag**. Whether a provider means
the operator's own cluster or a remote one follows from **one thing only: is `spec.kubeConfig`
set?**

```go
// IsInCluster reports whether this provider represents the operator's own (in-cluster) cluster —
// i.e. it has no kubeConfig. The provider name is irrelevant: any name, including "default", may
// either omit kubeConfig for the in-cluster client or set it for a remote cluster.
func (p *ClusterProvider) IsInCluster() bool { return p.Spec.KubeConfig == nil }
```
— [`clusterprovider_types.go:174-178`](../../api/v1alpha3/clusterprovider_types.go#L174-L178)

The name `default` is **a defaulting convention and nothing else**: it is the value an omitted
`GitTarget.spec.clusterProviderRef` persists as
([`gittarget_types.go:98`](../../api/v1alpha3/gittarget_types.go#L98)). The operator never creates
that object; a user does. And a user is free to give it a `kubeConfig` and have `default` mean a
remote cluster — supported, not an abuse:

> That choice is free for EVERY name, `"default"` included — a provider named `"default"` may just
> as well carry a kubeConfig and mirror a remote cluster. The name is an identity, not a claim
> about which cluster it points at.
>
> — [`clusterprovider_types.go:62-66`](../../api/v1alpha3/clusterprovider_types.go#L62-L66)

An earlier draft of the `ClusterProvider` design proposed CEL enforcing *named `default` **iff** no
`kubeConfig`*. **That was rejected and never shipped** — pinning a name to a physical cluster is
the silent-retarget hazard the immutability rules exist to prevent, moved into the schema.

### The trap this leaves behind

`GitTarget.IsLocalSource()` **is** a name test
([`gittarget_types.go:238`](../../api/v1alpha3/gittarget_types.go#L238)). It exists solely to seed
the pre-discovery `SourceClusterReachable` default before the watch manager is wired
([`gittarget_controller.go:250`](../../internal/controller/gittarget_controller.go#L250)), and the
manager overwrites it immediately. **It is not a locality predicate and must never be used as
one.** Anything that needs to know "is this the operator's own cluster" tests `kubeConfig`.

### Why permission is not derived from that fact

A previous revision of this document gated the new field on `kubeConfig` presence: remote-only,
in-cluster forbidden. **Rejected** (§5). Deriving an *addressing capability* from a *connectivity*
setting means an admin who sets `kubeConfig` to reach a cluster silently hands WatchRule authors a
namespace-addressing power they were never asked about. Permission gets its own field, on the
platform-owned object, and locality below is used only to explain **what the admin is signing off
on** — not to decide it for them.

---

## 2. What the admin is actually granting

The mechanism is identical in both cases. The thing being agreed to is not.

**In-cluster source (`kubeConfig` empty) — the coupling *was* the boundary.** The config plane *is*
the watched cluster, so "who can cause namespace A to be mirrored" and "who has RBAC in A" are the
same question, and ordinary namespace RBAC does the work. Listing a namespace here **deliberately
bypasses live namespace RBAC**: a WatchRule author in `team-a` gains read access to `team-b`'s
objects, laundered through the operator, into a Git destination they may control. That is a real
cross-namespace read escalation — sharper than the intra-namespace RBAC-split edge config-plane
split already flags and consciously leaves open.

It is still a legitimate thing for a cluster-admin to grant deliberately. It must never happen
by default, or as a side effect of some other field.

**Remote source (`kubeConfig` set) — the coupling was never the boundary.** The config-plane
namespace and the source namespace are on *different clusters*; "who can create a WatchRule in
config-plane namespace `repo-config`" tells you nothing about who can read `repo-config` on the
source. The same-name coupling there is pure mechanism, bypassing nothing. What governs a remote
mirror is a pair that already ships: `allowedNamespaces` (deny-by-default, config-plane side) and
the kubeconfig's own RBAC on the source cluster. Neither depends on the two namespaces sharing a
name, so naming one widens nothing — only the addressing improves.

**So: same field, same enforcement, materially different sign-off.** The docs must say that; the
API must not try to decide it.

## 3. Where the gate lives

Cross-object (`WatchRule.spec.targetRef` → `GitTarget` → `clusterProviderRef` →
`allowedSourceNamespaces`), so it is not expressible in CEL. Per
[`../spec/where-validation-lives.md`](../spec/where-validation-lives.md) that makes it a
**reconciler check, not a webhook** — same shape and ordering as `checkSourceAuthorization` /
`GitTargetReasonNamespaceNotAuthorized`. Settled repo-wide; no further argument needed here.

## 4. Names

### On `WatchRule`: `sourceNamespace`

A previous revision recommended `sourceClusterNamespace`, arguing the longer name "forces the
reader to ask *which cluster?* — exactly the question the remote-only rule turns on."
**That justification dies with the remote-only rule.** With an explicit admin-set allow-list, the
field is bounded by an object the tenant does not control, and "which cluster" is answered by
`targetRef` → `GitTarget` → `clusterProviderRef` exactly like everything else about the source.

`sourceNamespace` it is. `remoteNamespace` stays rejected — "source" is the established axis (~110
occurrences, plus `SourceClusterReachable`, `SourceClusterResolver`, `SourceClusterAccessDenied`);
`clusterProviderRef` "names the SOURCE cluster this GitTarget mirrors FROM"
([`gittarget_types.go:90`](../../api/v1alpha3/gittarget_types.go#L90)), while "remote" appears only
in informal prose and never as an API term. `sourceNamespaceOverride` is also rejected: it names
the mechanism relative to a default rather than the thing itself.

### On `ClusterProvider`: `allowedSourceNamespaces`, not `sourceNamespaces`

The `allowed` prefix is load-bearing, and dropping it would quietly undermine §5's whole argument.
The allow-list **is** the gate because *absent or empty means none* — so the name must carry
deny-by-default, or a reader meeting an absent field has no cue whether it means "none" or
"unrestricted". The fail-open reading is the catastrophic one, so the name must foreclose it.
`allowed*` is already this repo's idiom for exactly this (`allowedNamespaces` on this same object,
`allowedBranches` on `GitProvider`).

### The asymmetry this leaves, and why we live with it

Side by side, `allowedNamespaces` and `allowedSourceNamespaces` are symmetric on *permission* but
not on *whose namespaces* — the first is unqualified and means the config-plane/GitTarget side:

| Field | Which namespaces | Status |
|---|---|---|
| `allowedNamespaces` | config-plane: which may bind this provider from a `GitTarget` | shipped v0.38.0 |
| `allowedSourceNamespaces` | source cluster: which a `WatchRule` may address | proposed |

The fully symmetric pair is `allowedGitTargetNamespaces` + `allowedSourceNamespaces`. **Do not
rename in v1alpha3** — `allowedNamespaces` is shipped and is a security control. Worth recording
that the rename would fail *closed* (old field ignored → no policy → deny-by-default →
`Validated=False`), so it is churn rather than danger; **adopt it at the next API version**, with
conversion, and not before.

---

## 5. Directions tried

| Direction | Verdict |
|---|---|
| **`sourceNamespace` on `WatchRule`, authorized by `allowedSourceNamespaces` on the provider** | ✅ **Recommended.** One mechanism, uniform across in-cluster and remote; authorization sits on the platform-owned object next to `allowedNamespaces`; the tenant cannot widen it. |
| **Gate on `kubeConfig` presence (remote-only)** | ❌ **Superseded.** Derives an addressing capability from a connectivity setting — setting a kubeconfig to reach a cluster silently grants namespace addressing. It also welds the in-cluster case shut forever, conflating "unsafe by default" with "impossible", and needs a *second* field for the source-side bound anyway (below). Two mechanisms where one suffices. |
| **A boolean (`allowWatchRuleNamespaceOverride`) plus a namespace allow-list** | ❌ **Merged into the allow-list.** Right instinct — explicit beats derived, and the in-cluster case should be grantable — but two fields gate one capability: the boolean says *whether*, the list says *which*. A deny-by-default list already **is** a gate (`allowedNamespaces` is exactly this shape on this object). The only thing the boolean uniquely adds is *"any namespace"*, which is the capability §6 argues must not ship. The "cannot enumerate namespaces" objection is answered by the selector. |
| **Namespace selector on `ClusterWatchRule` instead** | ⚠️ **Viable, with prerequisites — raise it, do not lead with it.** Mechanically confirmed (§8): ClusterWatchRule already watches the GitTarget's *source* cluster across all namespaces, through the same resolution. **But** it needs two pre-existing defects fixed first, and costs per-tenant namespace ownership and tenant self-service authoring. |
| **Unique-naming interim** (`acme-config` etc.) | ✅ Works today on stock v0.38.0, no upstream change, full isolation. Cost: an account-encoded namespace name the tenant sees in their own workspace. **This is the fallback if we ship before #14.** |
| **`namespaceSelector` label fan-out on `WatchRule`** | Deferred. More powerful, but changes routing semantics (which folder does each namespace map to?). The single-namespace field is the surgical version; a selector can follow. |
| **Define the watch CRs in the source cluster** | ❌ Namespace-correct by construction, but re-diffuses config across every workspace — the exact thing the config-plane split centralized — and drags the git token back onto the watched cluster (`GitProvider.secretRef` is namespace-local). A reasonable opt-in mode later, not the fix. |
| **Co-located `repo-<repo>` namespaces** | ❌ Co-mingles tenants' git credentials and makes namespace ownership non-per-selection. |
| **Do nothing / stay per-tenant reverser** | ❌ N reversers cannot share one config-plane cluster — they would each watch all config CRs and fight. |

## 6. Recommendation

Two fields, one mechanism.

**On `ClusterProvider`** — `spec.allowedSourceNamespaces`, deny-by-default, names and/or selector
ORed, mirroring `allowedNamespaces` exactly:

```yaml
spec:
  allowedNamespaces:            # config-plane side: who may bind me
    names: [tenant-acme]
  allowedSourceNamespaces:      # source side: which of MY namespaces a WatchRule may address
    names: [repo-config]        # absent or empty = none
    selector: {matchLabels: {mirrored: "true"}}
```

**On `WatchRule`** — `spec.sourceNamespace`, optional, defaulting to `metadata.namespace`, so every
existing rule is untouched. Set to anything other than the rule's own namespace, it is honored only
if the referenced `GitTarget`'s `ClusterProvider` admits it via `allowedSourceNamespaces`; otherwise
it is rejected at reconcile with `Validated=False` / `SourceNamespaceNotAllowed`, **before any watch
is declared**.

Note the deliberate consequence: with no `allowedSourceNamespaces`, a rule may still name its *own*
namespace, so nothing existing changes and deny-by-default costs nobody anything.

### Why the bound belongs in the upstream ask, not in "optional hardening"

`allowedNamespaces` bounds *which config-plane namespace may bind a provider* — **not** which source
namespace may be read. Without a source-side bound, any multi-tenant-per-cluster user gets
whole-cluster read reachability from a single WatchRule, and the upstream field is permanent and
public.

For **gitops-api** the bound is nearly moot — one `ClusterProvider` per kcp workspace, each holding
one tenant's data — but it is what makes the story symmetric with `clusterProviderRef`'s and keeps
authorization on the platform-owned object:

```
ClusterProvider/workspace-acme     kubeConfig → tenant-acme's workspace
  allowedNamespaces.names:       [ tenant-acme ]     # who may bind me
  allowedSourceNamespaces.names: [ repo-config ]     # what they may address
GitTarget  (ns tenant-acme) → clusterProviderRef: workspace-acme
WatchRule  (ns tenant-acme) → sourceNamespace: repo-config
```

## 7. What to change in wishlist #14

1. Lead with the **provider-side allow-list**: the WatchRule field alone is not the ask (§6).
2. State the gate as **`allowedSourceNamespaces` admits it** — never "the provider is remote", and
   never a `kubeConfig` test (§1, §5).
3. Name them `sourceNamespace` and `allowedSourceNamespaces` (§4).
4. Say explicitly it is **reconcile-gated, not webhook-gated** (§3) — otherwise the first upstream
   reader assumes a webhook and prices the ask higher than it is.
5. Raise the `ClusterWatchRule` alternative explicitly (§5, §8) — upstream will ask why the existing
   admin-gated CR is not the answer; the concrete reply is that it works, after two pre-existing
   defects are fixed.
6. State that the Git path **already** uses the source object's namespace (§8a), so the ask is
   selection-side only. This is the single biggest thing that makes #14 look small rather than
   scary, and it is invisible from the API types alone.

---

## 8. Two questions this opened, answered in code

### 8a. Routing needs no change — the source object's namespace already names the folder

The value never comes from a config-plane object at all:

```
watch event (*unstructured, from the source cluster)
  └─ u.GetNamespace()                             watch/target_watch.go:818
      └─ types.ResourceIdentifier{Namespace: …}   types/identifier.go:14
          └─ git.Event.Identifier
              └─ PlacementRequest.Identifier      git/plan_flush.go:340
                  ├─ placementVars → {namespace}  manifestanalyzer/placement.go:436
                  └─ canonicalPath → ToGitPath()  types/identifier.go:58
```

The config-plane namespace travels in a **separate field**, `git.Event.GitTargetNamespace`
([`git/types.go:343`](../../internal/git/types.go#L343)), used only for GitTarget resolution,
credentials, and logging; it never reaches `PlacementRequest` or `ToGitPath()`. The one place a
config-plane namespace becomes a "namespace" in the data plane is
[`watched_type_resolver.go:297`](../../internal/watch/watched_type_resolver.go#L297) —
`namespace: rule.Source.Namespace` — and that is a **watch selector only**, discarded once the
stream is open.

**So the write side needs zero change**: a rule in `tenant-acme` watching `repo-config` remotely
already renders `repo-config/…`. Three invariants confirm this is correct semantics, not an
accident — `IdentityCompletePlacementTemplate` **requires** `{namespace}` for sensitive resources
([`placement.go:550`](../../internal/manifestanalyzer/placement.go#L550)), so a config-plane
`{namespace}` would collapse two source namespaces onto one path and silently break uniqueness;
the store keys `ByResourceIdentity` off the namespace **as written in the Git file**
([`store.go:1033`](../../internal/manifestanalyzer/store.go#L1033)), which must equal the live
object's; and sibling inference plus the `spansMultipleNamespaces` guard
([`placement.go:843`](../../internal/manifestanalyzer/placement.go#L843)) all operate on the source
object's namespace.

### 8b. `ClusterWatchRule` already watches the source cluster in all namespaces — and has two sharp edges

Both halves verified. **Source cluster, yes**: the controller stores only GitTarget coordinates
([`clusterwatchrule_controller.go:202`](../../internal/controller/clusterwatchrule_controller.go#L202));
cluster resolution happens later through the *same* call WatchRule uses
([`watched_type_resolver.go:310`](../../internal/watch/watched_type_resolver.go#L310), byte-identical
to the WatchRule call at :290), so it inherits remote resolution with no special-casing.
**All namespaces, yes**: namespace is hardcoded empty at
[`watched_type_resolver.go:317`](../../internal/watch/watched_type_resolver.go#L317), and
`ruleMatchesScope` ([`store.go:326`](../../internal/rulestore/store.go#L326)) takes no namespace
argument at all.

The two prerequisites that downgrade it from "worth arguing upstream" to "viable, with
prerequisites":

1. **A cluster-wide selection overrides named namespaces for the same GVR.** `SnapshotNamespaces()`
   returns `nil` for cluster-wide, which beats any named namespaces
   ([`watched_type_table.go:79-85`](../../internal/watch/watched_type_table.go#L79-L85)). So a
   ClusterWatchRule on a GVR **collapses co-resident WatchRule namespace scoping** for that GVR.
   Adding a selector means fixing this merge rule first, or a selector-scoped rule silently widens
   (or is widened by) a neighbour.
2. **ClusterWatchRule's `targetRef` is unconsented.** The controller does a plain `r.Get` with no
   access check
   ([`clusterwatchrule_controller.go:161`](../../internal/controller/clusterwatchrule_controller.go#L161)),
   so it may point at *any* GitTarget in *any* namespace. `allowedNamespaces` admitted the
   **GitTarget's** namespace; it never consented to this rule. **Not an escalation today** —
   ClusterWatchRule is cluster-scoped, so only a config-plane cluster-admin can create one, and
   that subject can already read the kubeconfig Secrets. But `allowedNamespaces` is not a fence in
   this direction, so leaning on ClusterWatchRule as *the* multi-tenant answer needs its own
   consent story.

Worth recording: authorization is enforced **only** on the GitTarget, against the GitTarget's own
namespace — `AllowsNamespace` has exactly one non-test call site
([`gittarget_source_cluster.go:68`](../../internal/controller/gittarget_source_cluster.go#L68)),
and the ClusterWatchRule reconciler contains no authorization call at all. The gate is nonetheless
effective transitively: a GitTarget failing it never reaches `DeclareForGitTarget`, so no
`targetWatches` entry exists and ClusterWatchRule streams live only inside *that* GitTarget's
table. (`bootstrap.go:71-91` seeds ClusterWatchRules with no authorization check — benign for the
same reason: inert until the GitTarget declares.)

## 9. Implementation sketch

The write side is untouched (§8a). The change is entirely on the **selection** side:

1. `ClusterProviderSpec` ([`clusterprovider_types.go:89`](../../api/v1alpha3/clusterprovider_types.go#L89))
   — add `AllowedSourceNamespaces *AllowedNamespaces`, reusing the existing struct verbatim.
2. A second predicate beside `AllowsNamespace`
   ([`clusterprovider_types.go:187`](../../api/v1alpha3/clusterprovider_types.go#L187)) — same
   deny-by-default body, different field. Keep them as two thin wrappers over one helper so the two
   policies cannot drift apart.
3. `WatchRuleSpec` ([`watchrule_types.go:46`](../../api/v1alpha3/watchrule_types.go#L46)) — add the
   optional `sourceNamespace`.
4. `rulestore.CompiledRule` ([`store.go:20-40`](../../internal/rulestore/store.go#L20-L40)) — carry
   it; today it holds only `Source types.NamespacedName`.
5. `collectWatchRuleSelections`
   ([`watched_type_resolver.go:297`](../../internal/watch/watched_type_resolver.go#L297)) — use the
   new field instead of `rule.Source.Namespace`.
6. `watchRuleFingerprint`
   ([`watched_type_resolver.go:479-481`](../../internal/watch/watched_type_resolver.go#L479-L481))
   — **must** include it; it currently hashes `rule.Source.Namespace` as the scope input, so
   without this a change to the field would not re-project the table.
7. The reconcile-time gate (§3), modelled on `checkSourceAuthorization`, testing the **allow-list** —
   never `kubeConfig`, never the provider's name (§1).

Steps 6 and 7 are the two that get missed: step 6 produces a stale-watch bug rather than a visible
failure, and step 7 produces a security hole rather than a visible failure.

**One test that must exist:** a `WatchRule` naming its *own* namespace must pass with no
`allowedSourceNamespaces` set. Otherwise deny-by-default breaks every existing rule on upgrade —
the field defaults to `metadata.namespace`, so the gate must only engage when the value actually
differs.
