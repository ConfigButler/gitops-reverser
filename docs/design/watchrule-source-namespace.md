# Addressing a source namespace from a WatchRule

> **design, ready to implement** — nothing here is built. Companion to upstream wishlist #14 and
> [config-plane-split.md](../finished/config-plane-split.md).
> Written 2026-07-19, revised 2026-07-20. Index: [INDEX.md](../INDEX.md).
>
> Every code reference in this document was re-verified against the tree on 2026-07-20. The
> evidence for the two load-bearing claims — that the write path needs no change, and that two
> existing defects bypass the gate proposed here — is in [Appendix A](#appendix-a-verified-in-code).

## Expected use cases

1. **Nothing changes for today's rules.** A WatchRule with no sourceNamespace continues to watch
   the namespace containing the rule.
2. **A tenant's configuration namespace and source namespace differ.** A WatchRule in tenant-acme
   selects repo-config in its workspace. This removes the collision caused by a shared config
   plane.
3. **One source cluster serves several tenants.** Each GitTarget declares the source namespaces
   that its WatchRules may select. tenant-acme's target can select its workspace namespace without
   giving tenant-zen's target any new scope.
4. **An in-cluster source is deliberately cross-namespace.** This is an explicit platform-admin
   delegation through the operator's cluster RBAC. The mechanism is the same for remote and
   in-cluster providers, but the in-cluster sign-off is especially sensitive.

## The model

The authorization follows the existing object references:

~~~text
WatchRule  ──uses──>  GitTarget  ──uses──>  ClusterProvider
    │                     │                       │
    │                     │                       └ permits this target namespace
    │                     └ permits source namespaces for its WatchRules
    └ requests one source namespace
~~~

- **WatchRule.spec.sourceNamespace** is the requested source namespace. When omitted, its effective
  value is the WatchRule's namespace.
- **GitTarget.spec.allowedSourceNamespaces** is the target owner's allow-list for the source
  namespaces that WatchRules using this target may request.
- **ClusterProvider.spec.allowedNamespaces** remains the existing platform-admin policy: it permits
  a control-cluster namespace to create GitTargets that use the provider.
- **ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride** is a new, false-by-default
  platform-admin delegation. When false, a WatchRule may use only its own namespace; when true, a
  GitTarget using this provider may grant a different source namespace through
  allowedSourceNamespaces.

The three objects therefore have distinct responsibilities: the rule asks, the target scopes that
target's work, and the provider delegates use of its source-cluster credential.

## Example

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: workspaces
spec:
  kubeConfig:
    secretRef:
      name: workspaces-kubeconfig

  # Existing gate: who may create a GitTarget using this provider.
  allowedNamespaces:
    selector:
      matchLabels:
        gitops.configbutler.ai/workspace-tenant: "true"

  # New gate: an admitted GitTarget may authorize source-namespace overrides.
  allowWatchRuleSourceNamespaceOverride: true
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: acme
  namespace: tenant-acme
spec:
  providerRef:
    name: acme-git
  branch: main
  path: tenants/acme
  clusterProviderRef:
    name: workspaces

  # Source-cluster namespaces that WatchRules using this target may select.
  allowedSourceNamespaces:
    names: [repo-config]
    selector:
      matchLabels:
        gitops.configbutler.ai/mirrorable: "true"
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: repo-config
  namespace: tenant-acme
spec:
  targetRef:
    name: acme
  sourceNamespace: repo-config
  rules:
    - resources: [configmaps]
~~~

The WatchRule is accepted because tenant-acme may use the provider, the provider delegates source
namespace overrides, and the GitTarget allows repo-config. A GitTarget in tenant-zen has its own
allowedSourceNamespaces policy, so it does not inherit the acme target's selection scope.

## Why this belongs on GitTarget

GitTarget already binds one source cluster to one Git destination, branch, and path. The selected
source namespaces determine what arrives at that destination, so the target is the natural place
to declare them. A reader can follow the same chain the controller follows:

~~~text
WatchRule.sourceNamespace
  → WatchRule.targetRef
  → GitTarget.allowedSourceNamespaces
  → GitTarget.clusterProviderRef
  → ClusterProvider.allowedNamespaces and delegation flag
~~~

Putting both allowed properties on ClusterProvider instead is attractive, but they become two
unrelated unions. A provider that admits tenant-acme and tenant-zen, then lists acme-config and
zen-config, cannot express which tenant owns which source namespace. A provider-side pair list can
represent that relationship, but duplicates the per-target source scope and makes the ordinary
reference path harder to read.

The GitTarget model works because a target is already namespaced: all current WatchRules that use
it are in that namespace. No cross-namespace GitTarget reference is added or implied.

It also dissolves a naming problem the provider-side design carried. Two fields called
allowedNamespaces and allowedSourceNamespaces on the *same* object would mean namespaces in two
*different* clusters, which no name can make legible. Splitting them across the two objects that
own those clusters removes the ambiguity rather than documenting around it.

## What the provider delegation means

The boolean is no longer redundant once the source allow-list lives on GitTarget. It does not grant
access by itself: the relevant GitTarget must still list or select the requested namespace. Instead
it says that the platform admin delegates source-namespace selection to admitted GitTargets.

That is a meaningful authority grant. A target owner can configure a broad selector, including one
that matches every source namespace, so the source cluster's kubeconfig RBAC remains the hard
maximum. Set the boolean only when the owner of an admitted GitTarget is trusted to choose a subset
of what that credential may read.

### Remote and in-cluster are the same mechanism and a different sign-off

For a **remote** source this is normally a clear delegation, because the coupling being removed was
never a boundary. The config-plane namespace and the source namespace are on different clusters, so
"who may create a WatchRule in config-plane namespace repo-config" already told you nothing about
who may read repo-config on the source. What governs a remote mirror is a pair that already ships:
allowedNamespaces on the config-plane side, and the kubeconfig's own RBAC on the source cluster.
Neither depends on the two namespaces sharing a name, so naming one widens nothing.

For an **in-cluster** provider the same-name coupling *was* the boundary, and this is the case that
deserves a deliberate decision rather than a default. The config plane is the watched cluster, so
"who can cause namespace A to be mirrored" and "who has RBAC in A" are the same question, and
ordinary namespace RBAC answers it. Setting the flag on an in-cluster provider deliberately bypasses
live namespace RBAC: the owner of an admitted GitTarget in tenant-acme can select tenant-zen's
namespace, and every object in it is read through the operator's cluster-wide credential and written
into a Git destination that the acme owner controls. That is a real cross-namespace read
escalation — sharper than the intra-namespace RBAC-split edge the config-plane split already flags
and consciously leaves open — and the escalation is bounded only by the operator's own cluster RBAC,
which is broad by design.

It remains a legitimate thing for a cluster-admin to grant on purpose. It must never happen by
default, or as a side effect of some other field. That is the whole reason the flag exists and
defaults to false.

If a platform instead needs a platform-admin-owned, per-tenant maximum source namespace policy, this
model alone is intentionally insufficient. Use one ClusterProvider and source credential per tenant,
or add a later provider-side pair policy. Do not silently treat GitTarget's policy as a substitute
for that stronger boundary.

### Locality is not the switch

A ClusterProvider is in-cluster when kubeConfig is omitted and remote when it is set
([clusterprovider_types.go:174-178](../../api/v1alpha3/clusterprovider_types.go#L174-L178)). The
name default is only the value of an omitted GitTarget clusterProviderRef
([gittarget_types.go:98](../../api/v1alpha3/gittarget_types.go#L98)). Neither decides
authorization; only the explicit delegation flag and policies do. Deriving an addressing capability
from a connectivity setting would mean an admin who sets kubeConfig to reach a cluster silently
hands WatchRule authors a power they were never asked about.

> **Implementation trap.** `GitTarget.IsLocalSource()`
> ([gittarget_types.go:238](../../api/v1alpha3/gittarget_types.go#L238)) — despite the name — **is a
> name test**. It exists solely to seed the pre-discovery SourceClusterReachable default before the
> watch manager is wired ([gittarget_controller.go:250](../../internal/controller/gittarget_controller.go#L250)),
> and the manager overwrites it immediately. It is not a locality predicate and must never be used
> as one. Anything that needs to know "is this the operator's own cluster" tests kubeConfig. The
> same warning is recorded in
> [multi-cluster-author-attribution.md](../finished/multi-cluster-author-attribution.md).

## Names and labels behave the same way

Both namespace policies use one generic NamespaceMatcher shape:

| Field | Labels come from | Meaning |
|---|---|---|
| ClusterProvider.allowedNamespaces | control cluster | Which namespaces may create a GitTarget using the provider. |
| GitTarget.allowedSourceNamespaces | source cluster | Which namespaces WatchRules using this target may select. |

For both fields:

- names and selector are ORed;
- an omitted or empty matcher admits nothing;
- a name match needs no Namespace label read;
- a selector matches labels on the Namespace object in the field's respective cluster.

The source policy is scoped to one GitTarget, not a shared provider. It does not need an SSA map
list; the existing names set remains a set, and the selector remains one object.

### Why these names

**sourceNamespace**, not sourceClusterNamespace or remoteNamespace. "source" is the established
axis in this API (~110 occurrences, plus SourceClusterReachable, SourceClusterResolver,
SourceClusterAccessDenied); clusterProviderRef already "names the SOURCE cluster this GitTarget
mirrors FROM" ([gittarget_types.go:90](../../api/v1alpha3/gittarget_types.go#L90)), while "remote"
appears only in informal prose and never as an API term. "Which cluster?" is answered by targetRef →
GitTarget → clusterProviderRef, exactly like everything else about the source.
`sourceNamespaceOverride` is rejected for the field: it names the mechanism relative to a default
rather than the thing itself. The delegation *flag* does carry "Override" deliberately, because a
boolean gate names a mechanism rather than a thing.

**allowedSourceNamespaces**, not sourceNamespaces. The `allowed` prefix is load-bearing. The
allow-list *is* the gate precisely because absent-or-empty means none, so the name must carry
deny-by-default, or a reader meeting an absent field has no cue whether it means "none" or
"unrestricted". The fail-open reading is the catastrophic one, so the name must foreclose it.
`allowed*` is already this repo's idiom for exactly this shape (allowedNamespaces on ClusterProvider,
allowedBranches on GitProvider).

**Deferred:** the fully symmetric pair would be `allowedGitTargetNamespaces` +
`allowedSourceNamespaces`. Do **not** rename in v1alpha3 — allowedNamespaces is shipped and is a
security control. Worth recording that the rename would fail *closed* (old field ignored → no policy
→ deny-by-default → Validated=False), so it is churn rather than danger. Adopt it at the next API
version, with conversion, and not before.

## Reactivity and source-cluster RBAC

Policy changes must grant and revoke access promptly, not merely when a WatchRule happens to be
edited. Half of this is already wired; the table marks what is not:

| Input changes | Required reaction | Status today |
|---|---|---|
| GitTarget allowedSourceNamespaces | Reconcile its WatchRules. | **Already wired** — the WatchRule controller watches GitTarget with GenerationChangedPredicate ([watchrule_controller.go:450-453](../../internal/controller/watchrule_controller.go#L450-L453)); a spec edit bumps the generation. |
| ClusterProvider allowedNamespaces or delegation flag | Reconcile affected GitTargets **and their WatchRules**. | **Half wired** — ClusterProvider → GitTargets exists ([gittarget_controller.go:1138-1142](../../internal/controller/gittarget_controller.go#L1138-L1142)). ClusterProvider → WatchRules does **not**: that reconcile changes only GitTarget *status*, which GenerationChangedPredicate deliberately ignores. Needs a new mapper. |
| Control-cluster Namespace labels | Reconcile affected GitTargets. | **Already wired** — `namespaceToGitTargets` with LabelChangedPredicate ([gittarget_controller.go:1147-1150](../../internal/controller/gittarget_controller.go#L1147-L1150)). |
| Source-cluster Namespace labels | Reconcile WatchRules whose GitTarget uses a source selector. | **Not wired** — there is no Namespace informer in `internal/watch` at all today. Entirely new. |

The watch manager already owns source-cluster watch lifecycles. This design adds one
label-filtered Namespace informer per active source cluster, not one per WatchRule. It emits only
meaningful label changes and maps them to WatchRules whose GitTarget resolves through that source
cluster.

The in-cluster manager role already grants namespaces get/list/watch
([config/rbac/role.yaml](../../config/rbac/role.yaml)):

~~~yaml
apiGroups: [""]
resources: [namespaces]
verbs: [get, list, watch]
~~~

A remote provider used with a GitTarget source selector needs the same permissions for the identity
in its kubeconfig. GET supplies current labels during reconciliation; LIST and WATCH keep selector
grants and revocations current. A permission denial or non-retryable watcher setup error makes a
selector override fail closed with SourceNamespacePolicyUnavailable; retryable source-cluster errors
remain InProgress and are retried. Exact-name entries remain usable without source Namespace access
— this is a deliberate degradation path, not an oversight, and it is worth testing explicitly.

On any refusal or revocation, the controller removes the compiled rule from RuleStore and replans
the watch manager to stop its stream before publishing the terminal status specified below.

## Reconciliation and validation

The effective source namespace is controller logic; an API-server default cannot refer to
metadata.namespace:

~~~go
effectiveSourceNamespace := rule.Spec.SourceNamespace
if effectiveSourceNamespace == "" {
    effectiveSourceNamespace = rule.Namespace
}
~~~

The legacy case needs no new authorization: if the effective source namespace equals the WatchRule
namespace, the rule continues to work with no delegation flag and no GitTarget policy. An explicit
different namespace requires all three checks:

1. the GitTarget's namespace is admitted by the ClusterProvider;
2. the ClusterProvider delegation flag is true; and
3. the GitTarget policy admits the effective source namespace.

This is cross-object authorization (WatchRule → GitTarget → ClusterProvider), and source selectors
also require remote state, so it is not expressible in CEL. Per
[where-validation-lives.md](../spec/where-validation-lives.md) that makes it a **reconciler check,
not a webhook** — the same shape and ordering as `checkSourceAuthorization` /
`GitTargetReasonNamespaceNotAuthorized`
([gittarget_source_cluster.go:41](../../internal/controller/gittarget_source_cluster.go#L41)).
Settled repo-wide; no further argument needed here. The check runs before
`RuleStore.AddOrUpdateWatchRule`
([watchrule_controller.go:225](../../internal/controller/watchrule_controller.go#L225)).

sourceNamespace is optional with MinLength=1 when present. GitTarget.allowedSourceNamespaces is an
optional deny-by-default NamespaceMatcher.

Documentation that becomes false when this ships, and must change in the same PR:

- the generated CRD description stating that WatchRule watches only its own namespace;
- [configuration.md](../configuration.md) — "It only watches resources in its own namespace";
- [configuration.md](../configuration.md) — "It has no effect on which namespaces are read from the
  source cluster; that remains entirely the source connection's Kubernetes RBAC." This is a
  security-relevant claim about allowedNamespaces that this design directly changes.

## Status contract (kstatus-compatible)

WatchRule already uses the project's conditions-first status contract, including
observedGeneration and the Ready / Reconciling / Stalled kstatus trio. This change extends that
contract; it does not add a phase, state string, or a second readiness model. That means Argo CD,
Flux, and other clients using `sigs.k8s.io/cli-utils/pkg/kstatus/status` see the same Current,
InProgress, and Failed results as they do for the other CRDs.

### SourceNamespaceAuthorized is the domain condition

Add the positive, state-style `SourceNamespaceAuthorized` condition to WatchRule:

- `True` means the effective source namespace is authorized for this observed generation. Use
  `LegacySourceNamespace` when it is the rule's own namespace, and `SourceNamespaceAllowed` when
  the override passed the three-part gate.
- `False` means the controller has determined that the override cannot run. Use
  `SourceNamespaceNotAllowed` for a disabled delegation flag or a non-matching/missing GitTarget
  policy, and `SourceNamespacePolicyUnavailable` when a selector policy cannot be evaluated (for
  example, required source-cluster Namespace access is denied).
- `Unknown` means authorization is still being established or a retryable source-cluster read/watch
  error is being retried. Use `CheckingSourceNamespacePolicy`; do not turn a temporary connection
  problem into a terminal failure.

Even legacy rules set this condition to `True`. That makes the effective authorization visible and
gives automation one condition to inspect, while preserving legacy behavior.

`GitTargetReady` remains the health of the referenced GitTarget; it must not be reused to report
source authorization. `ResourcesResolved` and `StreamsRunning` retain their existing meanings.
They explain the Ready aggregate after the source-namespace gate has passed.

### Generic kstatus readings

`SourceNamespaceAuthorized` becomes an additional prerequisite of the existing
`applyRuleKstatus` aggregation. `Ready=True` therefore means the source namespace is authorized,
the GitTarget is ready, resources resolved, and streams running for the observed generation. It
never means merely that the authorization gate passed.

| Situation | SourceNamespaceAuthorized | Ready | Reconciling | Stalled | kstatus result |
|---|---|---|---|---|---|
| Selector cache is starting, or a retryable source read/watch error is pending | Unknown | False | True | False | InProgress |
| Source namespace is authorized but target validation, resolution, or replay is still in progress | True | False | True | False | InProgress |
| Source namespace is authorized and all existing WatchRule prerequisites are healthy | True | True | False | False | Current |
| Delegation is disabled, policy denies, or selector evaluation is permanently unavailable | False | False | False | True | Failed |

A terminal refusal or revocation must first stop the compiled rule and then set
`SourceNamespaceAuthorized=False` plus the Failed trio. A retryable failure instead leaves the
compiled rule out of the store while status remains InProgress and the reconciler retries. This
distinction avoids both continuing an unauthorized watch and declaring a transient remote outage
permanently broken.

Every condition written for this feature, including the generic trio, carries the current
observedGeneration. `lastTransitionTime` changes only when a condition's status changes, following
the existing upsert helper. The existing Ready and Reason printer columns remain the primary
summary; add a priority-1 `SourceAuthorized` column for the domain condition when generating the
CRD so `kubectl get watchrules -o wide` exposes the gate directly.

## Routing remains source-native

No write-path change is required. Events already carry the namespace of the source object, which
feeds its resource identity and Git placement. sourceNamespace changes the watched namespace; it
must not substitute the WatchRule's control-cluster namespace into the Git path.

This is the single biggest thing that makes the change small rather than scary, and it is invisible
from the API types alone — so it is traced through the code in
[Appendix A.1](#a1-the-source-objects-namespace-already-names-the-git-folder).

## Prerequisite fixes

Two defects in the existing ClusterWatchRule path **bypass the gate this design introduces**. They
are pre-existing and are not caused by this work, but both let a co-resident ClusterWatchRule
deliver exactly the outcome allowedSourceNamespaces exists to prevent. Shipping the gate without
fixing them would ship an authorization boundary with a documented hole. Full evidence in
[Appendix A.2](#a2-two-clusterwatchrule-defects-that-bypass-this-gate).

### P1 — a cluster-wide selection collapses named-namespace scoping for the same GVR

`SnapshotNamespaces()` returns nil when a WatchedType has the `""` namespace key, and nil means
all-namespaces at every read site
([watched_type_table.go:78-92](../../internal/watch/watched_type_table.go#L78-L92)). A WatchRule
scoped to one namespace and a ClusterWatchRule scoped cluster-wide, on the same GVR and the same
GitTarget, fold into one WatchedType — and the cluster-wide entry wins. The named namespace survives
only in the plan hash.

Under this design that is a WatchRule gate bypass: a ClusterWatchRule may legitimately select every
source namespace once its GitTarget passes the provider-admission gate, but a co-resident WatchRule
must not silently inherit that cluster-wide stream. Otherwise a WatchRule authorized only for
repo-config receives events from every namespace the credential can read; its
`allowedSourceNamespaces` check passed only before the data plane widened it.

Two further notes for whoever fixes it:

- The operation sets collapse too, not just the namespaces: `targetWatchSpecs` uses
  `operationSpec(wt.NamespaceOps[""])` and discards the per-namespace op sets
  ([target_watch.go:214-224](../../internal/watch/target_watch.go#L214-L224)).
- **This behavior is currently asserted as intended** by
  `TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces`
  ([watched_type_table_test.go:64-82](../../internal/watch/watched_type_table_test.go#L64-L82)),
  documented there as "matching the historic gvrSnapshotEntry collapse". So this is a
  design-intent-versus-security-intent conflict, not an oversight, and the fix must consciously
  replace that test rather than quietly work around it.

**Fix:** keep cluster-wide and named selections as distinct streams for the same GVR, or subtract
the named-namespace scope from the cluster-wide one, and preserve the per-namespace operation sets.
Replace the test above with one asserting non-collapse.

### P2 — ClusterWatchRule's targetRef is unconsented

The ClusterWatchRule reconciler resolves its GitTarget with a plain `r.Get` and no authorization
check of any kind ([clusterwatchrule_controller.go:160-162](../../internal/controller/clusterwatchrule_controller.go#L160-L162));
the 537-line file contains no authorization call at all. `bootstrap.go` seeds rules the same way
([bootstrap.go:71-91](../../internal/watch/bootstrap.go#L71-L91)). So any ClusterWatchRule may
attach itself to any GitTarget in any namespace and widen that target's mirror scope to cluster-wide,
without the GitTarget's namespace or its ClusterProvider ever consenting to the rule.
`allowedNamespaces` admitted the *GitTarget's* namespace; it never consented to this rule.

**Not an escalation today.** ClusterWatchRule is cluster-scoped, so only a config-plane cluster-admin
can create one, and that subject can already read the kubeconfig Secrets. The gate is also effective
transitively: a GitTarget failing `checkSourceAuthorization` never reaches `DeclareForGitTarget`, so
no `targetWatches` entry exists and `refreshRunningTargetWatches` skips any table whose destination
is not already running ([target_watch.go:175-193](../../internal/watch/target_watch.go#L175-L193)).
An unauthorized GitTarget therefore builds a resident table but starts no stream.

This is not merely a ClusterWatchRule-creator authorization question. `allowedNamespaces` is the
ClusterProvider's explicit admission of the **GitTarget namespace** to use that provider. A
ClusterWatchRule has no namespace of its own, but its target does; letting it select an unadmitted
target is therefore inconsistent with the meaning of `allowedNamespaces`.

**Required fix:** factor the GitTarget provider-admission check into a shared helper and run it for
the referenced GitTarget namespace before a ClusterWatchRule is stored, both in the reconciler and
in `bootstrap.go`. If it fails, remove any existing compiled ClusterWatchRule, replan the watch
manager, set `GitTargetReady=False` with reason `GitTargetNamespaceNotAuthorized`, and publish the
terminal kstatus trio (`Ready=False`, `Reconciling=False`, `Stalled=True`). Changes to a
ClusterProvider's `allowedNamespaces` must also requeue the affected ClusterWatchRules so a later
revocation has the same effect.

This provider-admission check is separate from `GitTarget.allowedSourceNamespaces`. A
ClusterWatchRule currently selects every source namespace, so silently applying the namespaced
WatchRule policy would either mean "all namespaces must match" or accidentally turn it into a
different scope. Keep that as a separate, explicit future API decision; do not use it as a reason
to leave the provider's `allowedNamespaces` gate bypassable. P1 remains necessary to ensure a
cluster-wide rule cannot widen a co-resident namespaced stream.

Note one thing that is *already* confined: `providerNS := target.Namespace`
([clusterwatchrule_controller.go:173](../../internal/controller/clusterwatchrule_controller.go#L173)),
so the GitProvider is resolved in the GitTarget's own namespace. The unconsented edge is
rule → GitTarget only.

## Implementation plan

Ordered so each step is independently reviewable. Steps 8 and 9 are the two that get missed: step 8
produces a stale-watch bug rather than a visible failure, and step 9 produces a security hole rather
than a visible failure. Neither announces itself.

1. **Prerequisite P1** — fix the cluster-wide/named collapse in
   [watched_type_table.go](../../internal/watch/watched_type_table.go), preserving per-namespace
   operation sets, and replace
   `TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces`.
2. **Prerequisite P2** — factor the ClusterProvider `allowedNamespaces` check into a shared
   GitTarget-provider admission helper. Call it for the referenced GitTarget namespace from the
   ClusterWatchRule reconciler
   ([clusterwatchrule_controller.go:160](../../internal/controller/clusterwatchrule_controller.go#L160))
   and [bootstrap.go:71-91](../../internal/watch/bootstrap.go#L71-L91). On denial, remove the
   compiled rule, stop the stream, set `GitTargetReady=False` and the terminal kstatus trio, and
   requeue affected ClusterWatchRules when the provider policy changes.
3. **Generalize the matcher.** Rename/extend `AllowedNamespaces`
   ([clusterprovider_types.go:48](../../api/v1alpha3/clusterprovider_types.go#L48)) into a reusable
   NamespaceMatcher, keeping `AllowsNamespace`
   ([clusterprovider_types.go:191](../../api/v1alpha3/clusterprovider_types.go#L191)) and the new
   source-side predicate as two thin wrappers over one helper, so the two policies cannot drift
   apart.
4. **API fields.** `WatchRule.spec.sourceNamespace`
   ([watchrule_types.go:46](../../api/v1alpha3/watchrule_types.go#L46)),
   `GitTarget.spec.allowedSourceNamespaces`
   ([gittarget_types.go](../../api/v1alpha3/gittarget_types.go)),
   `ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride`
   ([clusterprovider_types.go:77](../../api/v1alpha3/clusterprovider_types.go#L77)), and the
   priority-1 `SourceAuthorized` WatchRule printer column. Run `task generate` and `task
   manifests`.
5. **Carry the effective namespace into the compiled rule.** `rulestore.CompiledRule`
   ([store.go:20-39](../../internal/rulestore/store.go#L20-L39)) today holds only
   `Source types.NamespacedName`; add an explicit source-namespace field rather than overloading
   `Source`, which also names the rule object.
6. **Use it for selection.** `collectWatchRuleSelections`
   ([watched_type_resolver.go:297](../../internal/watch/watched_type_resolver.go#L297)) currently
   sets `namespace: rule.Source.Namespace`; switch it to the effective source namespace.
7. **Resync path.** Confirm the snapshot/reconcile scope follows the same field —
   `desiredFromObject` reads `u.GetNamespace()`
   ([scope_resolve.go:200](../../internal/watch/scope_resolve.go#L200)), but the *scope* it iterates
   comes from the watched-type table, so step 6 must be sufficient. Verify rather than assume.
8. **Fingerprints.** `watchRuleFingerprint`
   ([watched_type_resolver.go:478-482](../../internal/watch/watched_type_resolver.go#L478-L482))
   hashes `rule.Source.Namespace` as its `src=` component; it **must** hash the effective source
   namespace instead, or a change to the field will not re-project the table. Note
   `clusterWatchRuleFingerprint`
   ([watched_type_resolver.go:491-500](../../internal/watch/watched_type_resolver.go#L491-L500))
   has no `src=` component because ClusterWatchRule remains all-source-namespaces. If a later API
   gives it a limited source scope, that scope must be fingerprinted too.
9. **The gate and status.** Add the three-part check in `reconcileWatchRuleViaTarget`, after the GitTarget fetch
   ([watchrule_controller.go:186](../../internal/controller/watchrule_controller.go#L186)) and
   before `AddOrUpdateWatchRule`
   ([watchrule_controller.go:225](../../internal/controller/watchrule_controller.go#L225)), modelled
   on `checkSourceAuthorization`. Set `SourceNamespaceAuthorized=True` for both legacy and allowed
   overrides, `False` for terminal refusals, and `Unknown` while retrying. On a terminal refusal,
   remove any existing compiled rule and replan the watch manager before setting the Failed
   kstatus trio. Extend `applyRuleKstatus` so this domain condition is a prerequisite and Unknown
   produces the InProgress trio rather than a separate status path.
10. **Reactivity.** Add the ClusterProvider → WatchRules **and ClusterWatchRules** mappers (the
    GitTarget → WatchRules edge already exists but is generation-filtered, so it will not carry a
    provider-driven change), and the per-source-cluster label-filtered Namespace informer.
11. **Docs.** The three statements listed under
    [Reconciliation and validation](#reconciliation-and-validation), the WatchRule section of
    [status-conditions-guide.md](../spec/status-conditions-guide.md), and the INDEX entry.

## Test plan

The tests are grouped by what they prove, because several of them exist to catch a *silent* failure
rather than a visible one.

### The one test that must exist

**A WatchRule that omits sourceNamespace must pass with no GitTarget policy and no delegation flag.**
If this fails, deny-by-default has broken every existing rule on upgrade. The gate must engage only
when the effective source namespace actually differs from the rule's own namespace. Place it first
and make its name say so.

### Gate correctness — `internal/controller`, table-driven

Modelled on `TestCheckSourceAuthorization`
([gittarget_source_cluster_test.go:117](../../internal/controller/gittarget_source_cluster_test.go#L117)):

| Case | Expected |
|---|---|
| sourceNamespace omitted, no policy, flag false | allowed (legacy) |
| sourceNamespace equals the rule's own namespace, no policy, flag false | allowed |
| sourceNamespace differs, flag false | denied, `SourceNamespaceNotAllowed` |
| sourceNamespace differs, flag true, target policy absent | denied (deny-by-default) |
| sourceNamespace differs, flag true, target policy empty `{}` | denied (empty ≠ unrestricted) |
| sourceNamespace differs, flag true, target names it | allowed |
| sourceNamespace differs, flag true, target selector matches its labels | allowed |
| sourceNamespace differs, flag true, target names a *different* namespace | denied |
| tenant-zen's target policy, acme's requested namespace | denied (target isolation) |
| invalid selector on the target policy | denied, `SourceNamespacePolicyUnavailable` |
| ClusterProvider read error (non-NotFound) | requeued as error, not silently denied |

Plus, from the degradation path in [Reactivity](#reactivity-and-source-cluster-rbac): with source
Namespace access forbidden, an **exact-name** entry still admits, while a **selector** entry fails
closed with `SourceNamespacePolicyUnavailable`. Both halves need a case — the first is the one that
will regress unnoticed.

### The gate actually stops the data plane

- `TestReconcile_DeniedSourceNamespaceStartsNoWatch` — mirrors
  `TestReconcile_UnauthorizedNamespaceStartsNoWatch`
  ([gittarget_source_cluster_test.go:320](../../internal/controller/gittarget_source_cluster_test.go#L320)):
  a denied rule must leave no compiled rule in RuleStore and start no stream.
- **Revocation:** a rule that was accepted, then denied by a tightened GitTarget policy, must have
  its compiled rule *removed* and the watch manager replanned — not merely reported unready. A gate
  that only writes a condition is not a gate.
- Conditions: `SourceNamespaceAuthorized=False`, Ready=False, Reconciling=False, and Stalled=True
  with the correct reason for each terminal refusal class.

### Status and kstatus — `internal/controller`

- Add WatchRule table tests using the real `sigs.k8s.io/cli-utils/pkg/kstatus/status.Compute`
  helper, following the existing GitTarget and GitProvider kstatus tests. Assert all four rows in
  the generic-status table above: source-policy checking and normal replay compute InProgress; a
  healthy rule computes Current; a terminal denial computes Failed.
- Assert the domain condition independently: legacy source namespace is True with
  `LegacySourceNamespace`; an allowed override is True with `SourceNamespaceAllowed`; a denied
  override is False; and a retryable selector lookup is Unknown.
- Assert `observedGeneration` on the domain condition and every generic condition equals the
  WatchRule generation after a source namespace, GitTarget policy, or ClusterProvider flag change.
  This prevents a stale success from being rendered as healthy.

### Silent-failure guards — `internal/watch`

- **Fingerprint:** two rules differing only in sourceNamespace must produce different
  `watchRuleFingerprint` values. Without this, step 8's omission is invisible until a stale watch is
  noticed in production.
- **Selection:** `collectWatchRuleSelections` must emit the effective source namespace, not
  `rule.Source.Namespace` — assert directly on the resulting `watchSelection.namespace`.
- **Re-projection:** changing sourceNamespace on a stored rule must rebuild the watched-type table
  (`rulesFingerprint` gates the rebuild at
  [watched_type_resolver.go:88-96](../../internal/watch/watched_type_resolver.go#L88-L96)).

### Prerequisite fixes

- **P1 replacement test:** a WatchRule scoped to `team-a` and a cluster-wide ClusterWatchRule on the
  same GVR and GitTarget must **not** collapse to one all-namespaces stream. This replaces
  `TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces` — the old assertion is the
  behavior being fixed, so leaving it green would mean the fix did not land.
- **P1 operation sets:** a `CREATE`-only named rule co-resident with an `UPDATE` cluster-wide rule
  must preserve both op sets.
- **P1 as a gate bypass:** the integration-level version — an authorized sourceNamespace plus a
  co-resident ClusterWatchRule must not widen the **WatchRule's** stream beyond the authorized
  namespace. The ClusterWatchRule retains its independent cluster-wide stream. This is the test
  that proves P1 matters to *this* design rather than being unrelated cleanup.
- **P2 — direct refusal:** a ClusterWatchRule referencing a GitTarget whose namespace the
  ClusterProvider does not admit must be refused in both the reconciler and the bootstrap path.
  The reconciler case must leave no compiled rule or running stream and set
  `GitTargetReady=False`, `Ready=False`, `Reconciling=False`, and `Stalled=True` with reason
  `GitTargetNamespaceNotAuthorized`.
- **P2 — revocation:** start from an admitted, running ClusterWatchRule, then remove its GitTarget
  namespace from `ClusterProvider.allowedNamespaces`. The provider-to-ClusterWatchRule mapper must
  requeue it, remove its compiled rule, stop the stream, and publish the same terminal status.

### Reactivity — envtest

- Flipping `allowWatchRuleSourceNamespaceOverride` on the ClusterProvider re-reconciles affected
  WatchRules. This one specifically catches the generation-predicate gap: without the new mapper the
  flag change reaches the GitTarget's *status* only, and the WatchRules never re-run.
- Editing GitTarget.allowedSourceNamespaces re-reconciles its WatchRules (should pass on the
  existing wiring — assert it so a later predicate change cannot silently break it).
- Adding/removing the matching label on a **source-cluster** Namespace grants/revokes within a
  bounded time.

### End-to-end

- A WatchRule in tenant-acme with `sourceNamespace: repo-config` against a remote source cluster
  produces Git paths under `repo-config/…`, **not** `tenant-acme/…`. This is the end-to-end proof of
  [Appendix A.1](#a1-the-source-objects-namespace-already-names-the-git-folder) — the claim that the
  write path needs no change — and it is the one assertion that would catch a regression there.
- A refused override surfaces Ready=False / Stalled=True and writes nothing to Git. It also
  surfaces `SourceNamespaceAuthorized=False`, Reconciling=False, and kstatus Failed.

## Compatibility and deliberate breaking changes

This is a preliminary v1alpha3 API. We intentionally accept the following observable changes; no
migration, deprecation period, or conversion webhook is planned.

| Change | Impact |
|---|---|
| `WatchRule.spec.sourceNamespace`, `GitTarget.spec.allowedSourceNamespaces`, and the ClusterProvider delegation flag | Additive fields. An omitted sourceNamespace still selects the rule's own namespace, but manifests that use an override require the new controller behavior. An older controller will continue the legacy own-namespace watch and must not be used with such manifests. |
| `SourceNamespaceAuthorized` condition and `SourceAuthorized` printer column | Additive status surface. Scripts that consume a fixed condition set or column layout must tolerate the new data. |
| Ready reasons and kstatus outcome for a denied override | Intentional status-contract change: an authorization refusal is Failed (`Ready=False`, `Reconciling=False`, `Stalled=True`), not a quietly inactive rule. |
| Selector-based overrides | The remote source credential now needs Namespace `get`, `list`, and `watch`. Without those permissions, selector overrides fail closed; exact-name policies remain usable. |
| P1 stream-scope fix | Deliberately narrows a ClusterWatchRule stream that previously became cluster-wide through the named/cluster-wide collapse. Any configuration relying on that accidental widening changes behavior. |
| P2 ClusterWatchRule target authorization | Deliberately rejects a ClusterWatchRule whose selected GitTarget is not admitted by its ClusterProvider. |

The generated CRD remains served as v1alpha3 with the existing status subresource and map-style
conditions list. These changes do not require stored-object migration, but release notes must call
out the behavior changes above.

## Alternatives, briefly

- **No delegation flag:** rejected. A GitTarget policy would otherwise silently turn provider access
  into cross-namespace source reachability.
- **Provider-wide allowedSourceNamespaces:** useful only for a one-tenant provider. With several
  tenants it becomes a Cartesian product and cannot express ownership. This was the previous
  revision's recommendation; the ownership argument above is what superseded it.
- **Provider-side pair policies:** can enforce platform-owned per-tenant maxima, but are more
  complex and less followable than the GitTarget model. Reserve this for a later stronger-boundary
  requirement.
- **Remote-only override (gate on kubeConfig presence):** rejected. kubeConfig is connectivity, not
  permission; it also welds the in-cluster case shut forever, conflating "unsafe by default" with
  "impossible".
- **ClusterWatchRule selector instead:** mechanically viable — ClusterWatchRule already watches the
  GitTarget's *source* cluster across all namespaces through the same resolution path (Appendix
  A.2) — but it costs per-tenant namespace ownership and tenant self-service authoring, and it
  requires P1 and P2 fixed first regardless. Raise it as an alternative; do not lead with it.
- **Unique-naming interim** (`acme-config` etc.): works today with no code change and gives full
  isolation, at the cost of an account-encoded namespace name the tenant sees in their own
  workspace. This is the fallback if a tenant needs the capability before this ships.
- **namespaceSelector label fan-out on WatchRule:** deferred. More powerful, but changes routing
  semantics (which folder does each namespace map to?). The single-namespace field is the surgical
  version; a selector can follow.
- **Define the watch CRs in the source cluster:** namespace-correct by construction, but re-diffuses
  config across every workspace — the exact thing the config-plane split centralized — and drags the
  Git token back onto the watched cluster (`GitProvider.secretRef` is namespace-local). A reasonable
  opt-in mode later, not the fix.

---

## Appendix A: verified in code

Re-verified against the tree on 2026-07-20. This appendix is evidence, not argument: it exists so
the next reader does not have to re-derive it, and so a regression in any of it is detectable.

### A.1 The source object's namespace already names the Git folder

The value never comes from a config-plane object at all:

~~~text
watch event (*unstructured, from the source cluster)
  └─ u.GetNamespace()                             watch/target_watch.go:818
      └─ types.ResourceIdentifier{Namespace: …}   types/identifier.go:18
          └─ git.Event.Identifier
              └─ PlacementRequest.Identifier      git/plan_flush.go:341
                  ├─ placementVars → {namespace}  manifestanalyzer/placement.go:436
                  └─ canonicalPath → ToGitPath()  types/identifier.go:58
~~~

The resync/reconcile path ingests the namespace the same way, at a second site:
`desiredFromObject` also reads `u.GetNamespace()`
([scope_resolve.go:200](../../internal/watch/scope_resolve.go#L200)).

The config-plane namespace travels in a **separate field**, `git.Event.GitTargetNamespace`
([git/types.go:343](../../internal/git/types.go#L343)). Every non-test use falls into exactly three
buckets — GitTarget/credential resolution, worker and commit-window identity keying, and the
write-permission guard plus logging. No `PlacementRequest` literal anywhere references it, and
`event.Identifier` is never rewritten from it.

The one place a config-plane namespace becomes a "namespace" in the data plane is
`namespace: rule.Source.Namespace`
([watched_type_resolver.go:297](../../internal/watch/watched_type_resolver.go#L297)) — a **watch
selector only**, discarded once the stream is open, because the identifier is rebuilt from
`u.GetNamespace()`. That line is precisely what step 6 of the implementation plan changes.

**So the write side needs zero change**: a rule in tenant-acme watching repo-config remotely already
renders `repo-config/…`. Three invariants confirm this is correct semantics rather than an accident:

1. A placement template for a core Secret must be identity-complete — it must contain `{name}` and
   either `{namespace}` or `{namespaceOrCluster}`
   ([placement.go:550-552](../../internal/manifestanalyzer/placement.go#L550-L552), enforced
   statically at [gittarget_placement_validation.go:67](../../internal/controller/gittarget_placement_validation.go#L67)).
   A config-plane `{namespace}` would collapse two source namespaces onto one path and silently
   break uniqueness. Note the enforcement is admission-time and covers core Secrets;
   operator-configured sensitive types rely on write-time guards instead.
2. The store keys `ByResourceIdentity` off the namespace **as written in the Git file**
   ([store.go:1033](../../internal/manifestanalyzer/store.go#L1033)), which must equal the live
   object's. Strictly this is the *effective* identity: a namespace-less document can inherit its
   namespace from a kustomization's `namespace:` transformer, with provenance in `NamespaceSource`.
   Either way it is never a config-plane namespace.
3. Sibling inference pins on the source object's namespace (`cohortMembers`,
   [placement.go:661](../../internal/manifestanalyzer/placement.go#L661)), and the
   `spansMultipleNamespaces` guard
   ([placement.go:843](../../internal/manifestanalyzer/placement.go#L843)) reads the *existing store
   documents'* namespaces to prove a candidate file is namespace-agnostic. Two different sources,
   neither of them config-plane.

### A.2 Two ClusterWatchRule defects that bypass this gate

Both halves of the mechanism are confirmed. **Source cluster, yes**: the controller stores only
GitTarget coordinates
([clusterwatchrule_controller.go:202-208](../../internal/controller/clusterwatchrule_controller.go#L202-L208));
cluster resolution happens later through the *same* call WatchRule uses
([watched_type_resolver.go:312](../../internal/watch/watched_type_resolver.go#L312), byte-identical
to the WatchRule call at :290), so it inherits remote resolution with no special-casing.
**All namespaces, yes**: namespace is hardcoded empty at
[watched_type_resolver.go:317-318](../../internal/watch/watched_type_resolver.go#L317-L318), and
`ruleMatchesScope` ([store.go:328-339](../../internal/rulestore/store.go#L328-L339)) takes no
namespace argument at all — its own comment concedes "Simplified MVP: Namespaced scope matches all
namespaces (no NamespaceSelector)."

**P1 mechanism.** `buildWatchedTypeTable`
([watched_type_table.go:128-155](../../internal/watch/watched_type_table.go#L128-L155)) is a pure
union: both the `""` key (ClusterWatchRule) and `"team-a"` (WatchRule) land in the same
`namespaceOps` map for the same GVR. The collapse happens later, at read time —
`ClusterWide()` merely tests for presence of the `""` key
([watched_type_table.go:73-76](../../internal/watch/watched_type_table.go#L73-L76)), so
`SnapshotNamespaces()` short-circuits to nil. Two read sites consume that nil as all-namespaces:
`targetWatchSpecs` ([target_watch.go:214-224](../../internal/watch/target_watch.go#L214-L224)) and
`snapshotGVRsFromTable` ([scope_resolve.go:180](../../internal/watch/scope_resolve.go#L180)). The
`team-a` entry survives in `NamespaceOps` for the plan hash only.

**P2 mitigation, confirmed.** Authorization is enforced **only** on the GitTarget, against the
GitTarget's own namespace — `AllowsNamespace` has exactly one non-test call site
([gittarget_source_cluster.go:68](../../internal/controller/gittarget_source_cluster.go#L68)). The
gate is nonetheless effective transitively: `checkSourceAuthorization` runs inside the Validated
gate and returns before `DeclareForGitTarget`
([gittarget_controller.go:218](../../internal/controller/gittarget_controller.go#L218)), which is
what populates `gitTargetClusters` and creates the `targetWatches` entry. The rule-change path
cannot bootstrap a watch on its own: `refreshRunningTargetWatches`
([target_watch.go:175-193](../../internal/watch/target_watch.go#L175-L193)) snapshots the *existing*
`targetWatches` keys and skips any table whose destination is not already running. So a
ClusterWatchRule pointing at an unauthorized GitTarget builds a resident table but starts no stream.
`bootstrap.go` is benign for the same reason.
