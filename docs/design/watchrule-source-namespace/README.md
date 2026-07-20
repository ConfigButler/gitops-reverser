# Source-namespace addressing and per-target source scope

> **Design in five PRs. PRs 1 and 2 have landed; PRs 3–5 are not built.** Companion to upstream
> wishlist #14 and [config-plane-split.md](../../finished/config-plane-split.md).
> Written 2026-07-19, split into phases 2026-07-20. Index: [INDEX.md](../../INDEX.md).
>
> Every code reference in this folder was verified against the tree on 2026-07-20. Each PR page
> carries its own evidence section so a regression in any claim is detectable without re-deriving it.

A WatchRule can only watch the namespace it lives in. On a shared config plane that forces a
tenant's configuration namespace and their source namespace to share a name, which collides as soon
as two tenants want the same one. This folder adds a source-namespace field, and — the part that
makes it safe — a per-GitTarget ceiling on which source namespaces may be mirrored into that target
at all, binding **both** rule kinds. Three pre-existing scope defects are fixed first, because each
of them would otherwise turn the new fan-out into silent Git data loss or an unenforced boundary.

## The model

Authorization follows the object references that already exist:

~~~text
WatchRule  ──uses──>  GitTarget  ──uses──>  ClusterProvider
    │                     │                       │
    │                     │                       └ permits this target namespace
    │                     └ permits source namespaces mirrored into it (any rule kind)
    └ requests one source namespace
~~~

- **`WatchRule.spec.sourceNamespace`** — the requested source namespace. Omitted means the
  WatchRule's own namespace.
- **`GitTarget.spec.allowedSourceNamespaces`** — the target owner's allow-list. It is a property of
  the **destination**, not of the requesting rule: when declared it is exhaustive for every rule that
  writes to this GitTarget, ClusterWatchRule included.
- **`ClusterProvider.spec.allowedNamespaces`** — unchanged platform-admin policy: which
  control-cluster namespaces may create GitTargets using the provider.
- **`ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride`** — new, false-by-default
  delegation. While false a WatchRule may use only its own namespace.

The one invariant everything else serves — **a declared policy is exhaustive**:

> When a GitTarget declares `allowedSourceNamespaces`, the source namespaces mirrored into it are
> **exactly** those the policy admits, for every rule of every kind. When it declares none, each rule
> kind keeps its legacy scope.

| `GitTarget.allowedSourceNamespaces` | WatchRule | ClusterWatchRule (`scope: Namespaced` rules) |
|---|---|---|
| Undeclared | Own namespace only (legacy) | All source namespaces (legacy) |
| Declared | Exactly what the policy admits | Exactly what the policy admits |

Nothing changes on upgrade: the field is new, so it is undeclared everywhere until a target owner
opts in.

### No self-namespace exception

An earlier revision let a declared policy still implicitly permit a WatchRule's own namespace, on the
theory that it would be rude to break a legacy rule when a policy is added for an unrelated override.
That is rejected. It would mean the field does not actually bound what reaches the target — a reader
auditing `allowedSourceNamespaces: [repo-config]` would be wrong about the target's contents, which
defeats the reason the field exists — and "ceiling" would be a false description of it.

So a policy lists everything, including the target's own namespace when a legacy WatchRule needs it:

~~~yaml
allowedSourceNamespaces:
  names: [tenant-acme, repo-config]   # own namespace listed explicitly
~~~

The trade-off is a genuine authoring footgun: adding a policy for one override silently denies the
co-resident legacy rules unless their namespace is listed. It is mitigated three ways, and none of
them is "hope". The field is new, so no existing configuration is affected. The denial is loud, not
silent — `SourceNamespaceAuthorized=False` with reason `SourceNamespaceNotAllowed`, `Stalled=True`,
and the stream stopped. And the reason message must name the specific fix: *"namespace tenant-acme is
not in the GitTarget's allowedSourceNamespaces; add it to keep watching this rule's own namespace."*
A footgun you are told about, in the terms of the fix, is an acceptable price for a field that means
what it says.

## The driving use case: multi-tenancy with CRDs on day one

1. **Nothing changes for today's rules.** A WatchRule with no `sourceNamespace` still watches its
   own namespace.
2. **Config namespace and source namespace differ.** A WatchRule in `tenant-acme` selects
   `repo-config` in that tenant's workspace, removing the shared-config-plane collision.
3. **One source cluster, several tenants.** Each GitTarget declares what may be mirrored into it.
   acme's target reaches its workspace namespace without widening zen's target.
4. **A tenant needs CRDs, so it needs a ClusterWatchRule from day one.** A ClusterWatchRule is the
   only way to select cluster-scoped types, so a real multi-tenant deployment runs one per tenant
   GitTarget immediately — this is not a later refinement. That rule kind can also carry
   `scope: Namespaced` entries, and it points at a GitTarget by an explicit cross-namespace
   reference. Without the ceiling, the operator's answer to "will this stream objects from
   namespaces outside my allow-list?" is *hand-audit every ClusterWatchRule and hope*. With it, the
   answer is a field you can read off the GitTarget. This is why
   [PR 5](pr5-clusterwatchrule-source-ceiling.md) is part of the launch set, not a follow-up.
5. **A deliberately cross-namespace in-cluster source.** An explicit platform-admin delegation
   through the operator's cluster RBAC — the same mechanism as remote, but a much sharper sign-off.

### What the ceiling does not do

**Cluster-scoped objects have no namespace, so a namespace allow-list cannot partition them.** A
tenant whose ClusterWatchRule selects CRDs receives *every* CRD the source credential can read, and
`allowedSourceNamespaces` neither narrows nor is consulted for those streams. That is defensible —
CRDs are genuinely cluster-global, and mirroring the cluster's type surface into a tenant repo is
usually the intent — but "my allow-list bounds what this tenant sees" is **false for the
cluster-scoped half**, and a multi-tenant operator must know that before relying on it.

If a tenant must not see another tenant's cluster-scoped objects, this model is deliberately
insufficient. Use one ClusterProvider and source credential per tenant, so the credential's own RBAC
is the boundary. A per-target allow-list for cluster-scoped *types* is a plausible later field; it is
not in this workstream, because it is a different question (which types) from the one being answered
here (which namespaces).

## Implementation phases

Five PRs, ordered so each is independently reviewable and independently revertible. The first three
are pre-existing defect fixes that carry no API change; the last two are the feature.

| # | PR | Scope | Depends on | Status |
|---|---|---|---|---|
| 1 | [Namespace-scoped resync](pr1-namespace-scoped-resync.md) | A per-namespace replay must not sweep other namespaces' manifests of the same type. Bug fix, no API. | — | **landed** |
| 2 | [Stream-scope collapse](pr2-stream-scope-collapse.md) | A cluster-wide selection stops silently widening a co-resident named-namespace stream for the same GVR. Bug fix, no API. | 1 | **landed** |
| 3 | [ClusterWatchRule target admission](pr3-clusterwatchrule-target-admission.md) | A ClusterWatchRule may no longer attach to a GitTarget whose namespace its ClusterProvider does not admit. Bug fix, no API. | — | not started |
| 4 | [The sourceNamespace field and gate](pr4-source-namespace-field.md) | `WatchRule.spec.sourceNamespace`, `GitTarget.spec.allowedSourceNamespaces`, the delegation flag, the gate, `SourceNamespaceAuthorized`, and the source-scope service. | 1, 2 | not started |
| 5 | [The ClusterWatchRule ceiling](pr5-clusterwatchrule-source-ceiling.md) | A declared `allowedSourceNamespaces` narrows a ClusterWatchRule's namespaced streams. | 1, 2, 4 | not started |

**PR 1 gated everything, and has landed.** It was not a cleanup to slot in opportunistically: the
resync sweep was scoped by type but not by namespace, so the first change that let one GitTarget
watch a GVR in more than one namespace would have started deleting other namespaces' manifests from
Git. PRs 2, 4, and 5 each introduce exactly that fan-out, independently, so landing any of them
first would have been silent data loss in a tenant's repository. With PR 1 in, that floor is in
place and the remaining four are unblocked.

**Release gate: do not cut a release between PR 4 and PR 5.** PR 4 ships the field; until PR 5 lands
the field is enforced on the rule kind that cannot bypass it and unenforced on the one that can, and
[use case 4](#the-driving-use-case-multi-tenancy-with-crds-on-day-one) is exactly the configuration
that hits the gap. They may merge separately; they must ship together.

PR 3 is independent of the rest and can go at any point.

### The shape all five are serving

The end state is that a GitTarget's watch set is **exactly the streams that were declared for it** —
no accidental widening from a co-resident rule (PR 2), no attachment the provider never admitted
(PR 3), no scope
that outruns its declaration (PR 4, PR 5), and no sweep that acts outside the scope it was gathered
over (PR 1). Every defect in this folder is the same mistake in a different place: a scope that is
computed in one part of the system and then silently widened or dropped in another. That is why the
fixes precede the feature rather than accompanying it.

## Why the scope lives on GitTarget

GitTarget already binds one source cluster to one Git destination, branch, and path, so what may
arrive at that destination is a property of the target. The reference chain a reader follows is the
same one the controller follows: `WatchRule.sourceNamespace` → `targetRef` →
`GitTarget.allowedSourceNamespaces` → `clusterProviderRef` → `ClusterProvider.allowedNamespaces` and
the delegation flag.

The decisive argument against putting both allow-lists on ClusterProvider is that two fields named
`allowedNamespaces` and `allowedSourceNamespaces` on the *same* object would mean namespaces in two
*different* clusters — an ambiguity no name can fix. Splitting them across the two objects that own
those clusters removes it. A provider-wide source list also cannot express ownership: a provider
admitting `tenant-acme` and `tenant-zen` and listing `acme-config` and `zen-config` has no way to say
which tenant owns which, and becomes a Cartesian product.

The model works because `WatchRule.targetRef` is a `LocalTargetReference` with no namespace field
([watchrule_types.go:24-42](../../../api/v1alpha3/watchrule_types.go#L24-L42)), so every WatchRule
using a target is in that target's namespace. `ClusterWatchRule.targetRef` **is** cross-namespace,
which is precisely why PRs 2 and 4 exist.

## Naming decisions

- **`sourceNamespace`**, not `sourceClusterNamespace` or `remoteNamespace`. "source" is the
  established axis in this API (~110 occurrences, plus `SourceClusterReachable`,
  `SourceClusterResolver`, `SourceClusterAccessDenied`); `clusterProviderRef` already "names the
  SOURCE cluster this GitTarget mirrors FROM"
  ([gittarget_types.go:90](../../../api/v1alpha3/gittarget_types.go#L90)), while "remote" appears
  only in informal prose. `sourceNamespaceOverride` is rejected for the field: it names the mechanism
  relative to a default rather than the thing itself. The delegation *flag* keeps "Override"
  deliberately, because a boolean gate does name a mechanism.
- **`allowedSourceNamespaces`**, not `sourceNamespaces`. The `allowed` prefix is load-bearing: a
  reader meeting an absent field must not have to guess between "none" and "unrestricted", and the
  fail-open reading is the catastrophic one. `allowed*` is already this repo's idiom for the shape
  (`allowedNamespaces`, `allowedBranches`).
- **Deferred rename.** The fully symmetric pair would be `allowedGitTargetNamespaces` +
  `allowedSourceNamespaces`. Do **not** rename in v1alpha3 — `allowedNamespaces` is shipped and is a
  security control. The rename would fail *closed* (old field ignored → no policy → deny-by-default),
  so it is churn rather than danger. Adopt it at the next API version, with conversion.

Both policies use one generic `NamespaceMatcher` shape, with `names` and `selector` ORed, an
omitted-or-empty matcher admitting nothing, and a selector matching labels on the Namespace object in
that field's own cluster:

| Field | Labels come from | Meaning |
|---|---|---|
| `ClusterProvider.allowedNamespaces` | control cluster | Which namespaces may create a GitTarget using the provider. |
| `GitTarget.allowedSourceNamespaces` | source cluster | Which namespaces may be mirrored into this target, by any rule kind. |

## Compatibility

This is a preliminary v1alpha3 API. We intentionally accept the following observable changes; no
migration, deprecation period, or conversion webhook is planned. The generated CRD remains served as
v1alpha3 with the existing status subresource and map-style conditions list, so no stored-object
migration is required — but release notes must call these out.

| Change | Impact | PR |
|---|---|---|
| `sourceNamespace`, `allowedSourceNamespaces`, delegation flag | Additive. An omitted `sourceNamespace` still selects the rule's own namespace, but a manifest using an override requires the new controller. An older controller silently continues the legacy own-namespace watch and must not be used with such manifests. | 4 |
| `SourceNamespaceAuthorized` condition, `SourceAuthorized` printer column | Additive status surface. Scripts consuming a fixed condition set or column layout must tolerate it. PR 4 adds both to WatchRule; PR 5 adds them to ClusterWatchRule. | 4, 5 |
| Denied override is `Failed` | Intentional: an authorization refusal is `Ready=False`/`Reconciling=False`/`Stalled=True`, not a quietly inactive rule. An *unevaluatable* policy is not a refusal — see [establishing versus maintaining](pr4-source-namespace-field.md#establishing-versus-maintaining-a-scope). | 4 |
| Selector-based policies | The source credential now needs Namespace `get`, `list`, `watch`. Without them selector policies cannot be evaluated; exact-name policies keep working. | 4, 5 |
| Stream-scope fix | Narrows a stream that previously became cluster-wide through the named/cluster-wide collapse. Any configuration relying on that accidental widening changes behavior. | 2 |
| ClusterWatchRule target admission | Rejects a ClusterWatchRule whose GitTarget is not admitted by its ClusterProvider. | 3 |
| ClusterWatchRule ceiling | A ClusterWatchRule's namespaced streams narrow to a declared `allowedSourceNamespaces`. No existing config changes, since the field is undeclared everywhere on upgrade. Cluster-scoped rules unaffected. | 5 |

## Alternatives considered

- **No delegation flag** — rejected. A GitTarget policy would otherwise silently turn provider access
  into cross-namespace source reachability.
- **Provider-wide `allowedSourceNamespaces`** — useful only for a one-tenant provider; cannot express
  ownership. This was an earlier revision's recommendation, superseded by the ownership argument.
- **Provider-side pair policies** (`{gitTargetNamespace, sourceNamespaces}`) — can enforce a
  platform-owned per-tenant maximum, which the GitTarget model deliberately cannot. More complex and
  less followable. Reserve for a later stronger-boundary requirement; do not treat GitTarget's policy
  as a substitute for it.
- **Gate the override on `kubeConfig` presence (remote-only)** — rejected. `kubeConfig` is
  connectivity, not permission. It also welds the in-cluster case shut forever, conflating "unsafe by
  default" with "impossible". See the `IsLocalSource()` trap in
  [PR 4](pr4-source-namespace-field.md#locality-is-not-the-switch).
- **Use ClusterWatchRule instead of a WatchRule field** — mechanically viable, since a
  ClusterWatchRule already resolves through the same source cluster, but it costs per-tenant
  namespace ownership and tenant self-service authoring, and needs PRs 1 and 2 regardless.
- **Unique namespace naming** (`acme-config`) — works today with no code change and gives full
  isolation, at the cost of an account-encoded namespace name the tenant sees in their own workspace.
  This is the fallback if a tenant needs the capability before this ships.
- **`namespaceSelector` fan-out on WatchRule** — deferred. More powerful, but changes routing
  semantics (which folder does each namespace map to?). The single-namespace field is the surgical
  version; a selector can follow.
- **Define the watch CRs in the source cluster** — namespace-correct by construction, but re-diffuses
  config across every workspace (the thing the config-plane split centralized) and drags the Git
  token back onto the watched cluster, since `GitProvider.secretRef` is namespace-local. A reasonable
  opt-in mode later, not the fix.
