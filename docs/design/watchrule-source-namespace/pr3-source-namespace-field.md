# PR 3 — the sourceNamespace field and its authorization gate

> Phase 3 of [source-namespace addressing](README.md). **Depends on:** PR 1. **Blocked from release
> without:** PR 4 — see the [release gate](README.md#implementation-phases). API change: three new
> fields, one new condition, one printer column.

Adds `WatchRule.spec.sourceNamespace`, `GitTarget.spec.allowedSourceNamespaces`,
`ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride`, and the reconciler gate that binds
them. The model and naming rationale are in the [overview](README.md#the-model).

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

  # Source-cluster namespaces that may be mirrored into this target, by any rule kind.
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

Accepted because tenant-acme may use the provider, the provider delegates overrides, and the
GitTarget admits `repo-config`. A GitTarget in tenant-zen carries its own policy and inherits nothing
from acme's.

## What the delegation flag means

The flag does not grant access by itself — the GitTarget must still admit the namespace. It says the
platform admin delegates source-namespace selection to admitted GitTargets. That is a real authority
grant: a target owner can configure a broad selector, including one matching every source namespace,
so the source credential's RBAC remains the hard maximum. Set it only when the owner of an admitted
GitTarget is trusted to choose a subset of what that credential may read.

The flag gates **granting only**. `allowedSourceNamespaces` plays two roles — widening a WatchRule
beyond its own namespace, and (in PR 4) narrowing a ClusterWatchRule below cluster-wide — and only
the widening one is an authority grant. Gating a *restriction* behind a delegation flag would mean an
admin has to grant extra authority in order to reduce scope.

### Remote and in-cluster: same mechanism, very different sign-off

For a **remote** source this is normally a clear delegation, because the coupling being removed was
never a boundary. The config-plane namespace and the source namespace are on different clusters, so
"who may create a WatchRule in config-plane namespace `repo-config`" already told you nothing about
who may read `repo-config` on the source. What governs a remote mirror is a pair that already ships:
`allowedNamespaces` on the config-plane side, and the kubeconfig's own RBAC on the source. Neither
depends on the two namespaces sharing a name, so naming one widens nothing.

For an **in-cluster** provider the same-name coupling *was* the boundary. The config plane is the
watched cluster, so "who can cause namespace A to be mirrored" and "who has RBAC in A" are the same
question, and ordinary namespace RBAC answers it. Setting the flag on an in-cluster provider
deliberately bypasses live namespace RBAC: the owner of an admitted GitTarget in tenant-acme can
select tenant-zen's namespace, and every object in it is read through the operator's cluster-wide
credential and written into a Git destination acme controls. That is a real cross-namespace read
escalation — sharper than the intra-namespace RBAC-split edge the config-plane split already flags
and consciously leaves open — bounded only by the operator's own cluster RBAC, which is broad by
design.

It remains legitimate for a cluster-admin to grant on purpose. It must never happen by default or as
a side effect of another field. That is the whole reason the flag exists and defaults to false.

If a platform needs a platform-admin-owned per-tenant *maximum*, this model is intentionally
insufficient — use one ClusterProvider and credential per tenant, or add a later provider-side pair
policy. Do not silently treat GitTarget's policy as a substitute for that stronger boundary.

### Locality is not the switch

A ClusterProvider is in-cluster when `kubeConfig` is omitted and remote when it is set
([clusterprovider_types.go:174-178](../../../api/v1alpha3/clusterprovider_types.go#L174-L178)). The
name `default` is only the value of an omitted GitTarget `clusterProviderRef`
([gittarget_types.go:98](../../../api/v1alpha3/gittarget_types.go#L98)). Neither decides
authorization; only the explicit delegation flag and the policies do. Deriving an addressing
capability from a connectivity setting would mean an admin who sets `kubeConfig` to reach a cluster
silently hands WatchRule authors a power they were never asked about.

> **Implementation trap.** `GitTarget.IsLocalSource()`
> ([gittarget_types.go:238](../../../api/v1alpha3/gittarget_types.go#L238)) — despite the name — **is
> a name test**. It exists solely to seed the pre-discovery `SourceClusterReachable` default before
> the watch manager is wired
> ([gittarget_controller.go:250](../../../internal/controller/gittarget_controller.go#L250)), and the
> manager overwrites it immediately. It is not a locality predicate and must never be used as one.
> Anything needing "is this the operator's own cluster" tests `kubeConfig`. The same warning is
> recorded in [multi-cluster-author-attribution.md](../../finished/multi-cluster-author-attribution.md).

## The gate

The effective source namespace is controller logic; an API-server default cannot refer to
`metadata.namespace`:

~~~go
effectiveSourceNamespace := rule.Spec.SourceNamespace
if effectiveSourceNamespace == "" {
    effectiveSourceNamespace = rule.Namespace
}
~~~

**The legacy case needs no new authorization.** If the effective source namespace equals the
WatchRule's namespace, the rule works with no delegation flag and no GitTarget policy. An explicit
*different* namespace requires all three:

1. the GitTarget's namespace is admitted by the ClusterProvider;
2. the ClusterProvider delegation flag is true; and
3. the GitTarget policy admits the effective source namespace.

This is cross-object authorization (WatchRule → GitTarget → ClusterProvider), and source selectors
also require remote state, so it is not expressible in CEL. Per
[where-validation-lives.md](../../spec/where-validation-lives.md) that makes it a **reconciler check,
not a webhook** — the same shape and ordering as `checkSourceAuthorization` /
`GitTargetReasonNamespaceNotAuthorized`
([gittarget_source_cluster.go:41](../../../internal/controller/gittarget_source_cluster.go#L41)).
Settled repo-wide; no further argument needed. The check runs before
`RuleStore.AddOrUpdateWatchRule`
([watchrule_controller.go:225](../../../internal/controller/watchrule_controller.go#L225)).

`sourceNamespace` is optional with `MinLength=1` when present. `allowedSourceNamespaces` is an
optional deny-by-default `NamespaceMatcher`.

## Reactivity and source-cluster RBAC

Policy changes must grant and revoke promptly, not merely when a WatchRule happens to be edited.
Half of this is already wired:

| Input changes | Required reaction | Status today |
|---|---|---|
| GitTarget `allowedSourceNamespaces` | Reconcile its WatchRules. | **Already wired** — the WatchRule controller watches GitTarget with `GenerationChangedPredicate` ([watchrule_controller.go:450-453](../../../internal/controller/watchrule_controller.go#L450-L453)); a spec edit bumps the generation. |
| ClusterProvider `allowedNamespaces` or delegation flag | Reconcile affected GitTargets **and their WatchRules**. | **Half wired** — ClusterProvider → GitTargets exists ([gittarget_controller.go:1138-1142](../../../internal/controller/gittarget_controller.go#L1138-L1142)). ClusterProvider → WatchRules does **not**: that reconcile changes only GitTarget *status*, which `GenerationChangedPredicate` deliberately ignores. Needs a new mapper. |
| Control-cluster Namespace labels | Reconcile affected GitTargets. | **Already wired** — `namespaceToGitTargets` with `LabelChangedPredicate` ([gittarget_controller.go:1147-1150](../../../internal/controller/gittarget_controller.go#L1147-L1150)). |
| Source-cluster Namespace labels | Reconcile WatchRules whose GitTarget uses a source selector. | **Not wired** — there is no Namespace informer in `internal/watch` at all today. Entirely new. |

The watch manager already owns source-cluster watch lifecycles. This adds one label-filtered
Namespace informer **per active source cluster**, not one per WatchRule, emitting only meaningful
label changes and mapping them to WatchRules whose GitTarget resolves through that cluster. PR 4
extends the same informer to ClusterWatchRules.

The in-cluster manager role already grants `namespaces` `get`/`list`/`watch`
([config/rbac/role.yaml](../../../config/rbac/role.yaml)). A remote provider used with a source
selector needs the same for the identity in its kubeconfig: GET supplies current labels during
reconciliation, LIST and WATCH keep grants and revocations current. A permission denial or
non-retryable watcher setup error makes a selector policy fail closed with
`SourceNamespacePolicyUnavailable`; retryable errors remain InProgress and are retried.
**Exact-name entries remain usable without source Namespace access** — a deliberate degradation path,
not an oversight, and the half most likely to regress unnoticed.

On any refusal or revocation, remove the compiled rule from RuleStore and replan the watch manager to
stop its stream **before** publishing terminal status.

## Status contract (kstatus-compatible)

WatchRule already uses the project's conditions-first contract, including `observedGeneration` and
the Ready / Reconciling / Stalled trio. This extends that contract; it adds no phase, state string,
or second readiness model, so `sigs.k8s.io/cli-utils/pkg/kstatus/status` clients see the same
Current / InProgress / Failed results as for the other CRDs.

Add the positive, state-style **`SourceNamespaceAuthorized`** condition:

- **`True`** — the effective source namespace is authorized for this observed generation. Reason
  `LegacySourceNamespace` when it is the rule's own namespace, `SourceNamespaceAllowed` when the
  override passed the three-part gate.
- **`False`** — the override cannot run. Reason `SourceNamespaceNotAllowed` for a disabled flag or a
  non-matching/missing GitTarget policy; `SourceNamespacePolicyUnavailable` when a selector policy
  cannot be evaluated.
- **`Unknown`** — authorization is still being established, or a retryable source-cluster read/watch
  error is being retried. Reason `CheckingSourceNamespacePolicy`. Do not turn a temporary connection
  problem into a terminal failure.

Even legacy rules set it to `True`, so the effective authorization is always visible and automation
has one condition to inspect. `GitTargetReady` remains the health of the referenced GitTarget and
must not be reused for source authorization; `ResourcesResolved` and `StreamsRunning` keep their
meanings and explain the Ready aggregate *after* this gate passes.

`SourceNamespaceAuthorized` becomes an additional prerequisite of the existing `applyRuleKstatus`
aggregation, so `Ready=True` means the source namespace is authorized *and* the GitTarget is ready
*and* resources resolved *and* streams running — never merely that the gate passed.

| Situation | SourceNamespaceAuthorized | Ready | Reconciling | Stalled | kstatus |
|---|---|---|---|---|---|
| Selector cache starting, or retryable source error pending | Unknown | False | True | False | InProgress |
| Authorized, but target validation / resolution / replay in progress | True | False | True | False | InProgress |
| Authorized and all existing prerequisites healthy | True | True | False | False | Current |
| Delegation disabled, policy denies, or selector permanently unavailable | False | False | False | True | Failed |

A terminal refusal or revocation must **first** stop the compiled rule, **then** set
`SourceNamespaceAuthorized=False` plus the Failed trio. A retryable failure instead leaves the
compiled rule out of the store while status stays InProgress and the reconciler retries. This
distinction avoids both continuing an unauthorized watch and declaring a transient remote outage
permanently broken.

Every condition written here, including the generic trio, carries the current `observedGeneration`.
`lastTransitionTime` changes only on a status change, per the existing upsert helper. Existing Ready
and Reason printer columns stay primary; add a priority-1 `SourceAuthorized` column so
`kubectl get watchrules -o wide` exposes the gate.

## Routing remains source-native

**No write-path change is required.** Events already carry the namespace of the source object, which
feeds its resource identity and Git placement. `sourceNamespace` changes the *watched* namespace; it
must not substitute the WatchRule's control-cluster namespace into the Git path. This is the single
biggest thing making the change small rather than scary, and it is invisible from the API types
alone — so it is traced in [Appendix A](#appendix-a-the-source-objects-namespace-already-names-the-git-folder).

## Implementation steps

1. **Generalize the matcher.** Rename/extend `AllowedNamespaces`
   ([clusterprovider_types.go:48](../../../api/v1alpha3/clusterprovider_types.go#L48)) into a
   reusable `NamespaceMatcher`, keeping `AllowsNamespace`
   ([clusterprovider_types.go:191](../../../api/v1alpha3/clusterprovider_types.go#L191)) and the new
   source-side predicate as two thin wrappers over one helper, so the two policies cannot drift.
   The Go type name may change; the **JSON field name must not**.
2. **API fields.** `WatchRule.spec.sourceNamespace`
   ([watchrule_types.go:46](../../../api/v1alpha3/watchrule_types.go#L46)),
   `GitTarget.spec.allowedSourceNamespaces`
   ([gittarget_types.go](../../../api/v1alpha3/gittarget_types.go)),
   `ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride`
   ([clusterprovider_types.go:77](../../../api/v1alpha3/clusterprovider_types.go#L77)), and the
   priority-1 `SourceAuthorized` printer column. Run `task generate` and `task manifests`.
3. **Carry the effective namespace into the compiled rule.** `rulestore.CompiledRule`
   ([store.go:20-39](../../../internal/rulestore/store.go#L20-L39)) holds only
   `Source types.NamespacedName`; add an explicit source-namespace field rather than overloading
   `Source`, which also names the rule object.
4. **Use it for selection.** `collectWatchRuleSelections`
   ([watched_type_resolver.go:297](../../../internal/watch/watched_type_resolver.go#L297)) sets
   `namespace: rule.Source.Namespace`; switch it to the effective source namespace.
5. **Resync path.** Confirm the snapshot/reconcile scope follows the same field — `desiredFromObject`
   reads `u.GetNamespace()`
   ([scope_resolve.go:200](../../../internal/watch/scope_resolve.go#L200)), but the *scope* it
   iterates comes from the watched-type table, so step 4 should be sufficient. Verify, do not assume.
6. **Fingerprints.** `watchRuleFingerprint`
   ([watched_type_resolver.go:478-482](../../../internal/watch/watched_type_resolver.go#L478-L482))
   hashes `rule.Source.Namespace` as its `src=` component; it **must** hash the effective source
   namespace instead, or a change to the field will not re-project the table. This produces a stale
   watch rather than a visible failure — one of the two steps that never announces itself.
7. **The gate and status.** Add the three-part check in `reconcileWatchRuleViaTarget`, after the
   GitTarget fetch
   ([watchrule_controller.go:186](../../../internal/controller/watchrule_controller.go#L186)) and
   before `AddOrUpdateWatchRule`
   ([watchrule_controller.go:225](../../../internal/controller/watchrule_controller.go#L225)),
   modelled on `checkSourceAuthorization`. Set the condition per the contract above; on a terminal
   refusal remove any compiled rule and replan **before** the Failed trio. Extend `applyRuleKstatus`
   so the domain condition is a prerequisite and Unknown yields InProgress rather than a separate
   status path. This produces a security hole rather than a visible failure — the other silent step.
8. **Reactivity.** Add the ClusterProvider → WatchRules mapper (the GitTarget → WatchRules edge
   exists but is generation-filtered, so it will not carry a provider-driven change) and the
   per-source-cluster label-filtered Namespace informer.
9. **Docs.** Every statement listed under
   [Docs that become false](#docs-that-become-false-when-this-ships), the
   WatchRule section of [status-conditions-guide.md](../../spec/status-conditions-guide.md), and the
   INDEX entry.

## Docs that become false when this ships

Must change in the same PR:

- the generated CRD description stating that WatchRule watches only its own namespace;
- [configuration.md](../../configuration.md) — "It only watches resources in its own namespace";
- [configuration.md](../../configuration.md) — "It has no effect on which namespaces are read from
  the source cluster; that remains entirely the source connection's Kubernetes RBAC." This is a
  security-relevant claim about `allowedNamespaces` that this design directly changes.

## Test plan

Grouped by what they prove, because several exist to catch a *silent* failure.

### The one test that must exist

**A WatchRule that omits `sourceNamespace` passes with no GitTarget policy and no delegation flag.**
If this fails, deny-by-default has broken every existing rule on upgrade. The gate must engage only
when the effective source namespace actually differs from the rule's own namespace. Place it first
and make its name say so.

### Gate correctness — `internal/controller`, table-driven

Modelled on `TestCheckSourceAuthorization`
([gittarget_source_cluster_test.go:117](../../../internal/controller/gittarget_source_cluster_test.go#L117)):

| Case | Expected |
|---|---|
| `sourceNamespace` omitted, no policy, flag false | allowed (legacy) |
| equals the rule's own namespace, no policy, flag false | allowed |
| differs, flag false | denied, `SourceNamespaceNotAllowed` |
| differs, flag true, target policy absent | denied (deny-by-default) |
| differs, flag true, target policy empty `{}` | denied (empty ≠ unrestricted) |
| differs, flag true, target names it | allowed |
| differs, flag true, target selector matches its labels | allowed |
| differs, flag true, target names a *different* namespace | denied |
| tenant-zen's target policy, acme's requested namespace | denied (target isolation) |
| invalid selector on the target policy | denied, `SourceNamespacePolicyUnavailable` |
| ClusterProvider read error (non-NotFound) | requeued as error, not silently denied |

Plus the degradation path: with source Namespace access forbidden, an **exact-name** entry still
admits while a **selector** entry fails closed with `SourceNamespacePolicyUnavailable`. Both halves
need a case — the first is the one that will regress unnoticed.

### The gate actually stops the data plane

- `TestReconcile_DeniedSourceNamespaceStartsNoWatch` — mirrors
  `TestReconcile_UnauthorizedNamespaceStartsNoWatch`
  ([gittarget_source_cluster_test.go:320](../../../internal/controller/gittarget_source_cluster_test.go#L320)):
  a denied rule leaves no compiled rule in RuleStore and starts no stream.
- **Revocation:** a rule accepted, then denied by a tightened GitTarget policy, must have its
  compiled rule *removed* and the watch manager replanned — not merely reported unready. A gate that
  only writes a condition is not a gate.
- Conditions: `SourceNamespaceAuthorized=False`, Ready=False, Reconciling=False, Stalled=True, with
  the correct reason per terminal refusal class.

### Status and kstatus

- WatchRule table tests using the real `status.Compute` helper, following the existing GitTarget and
  GitProvider kstatus tests. Assert all four rows of the table above.
- The domain condition independently: legacy is True with `LegacySourceNamespace`; an allowed
  override is True with `SourceNamespaceAllowed`; a denied override is False; a retryable selector
  lookup is Unknown.
- `observedGeneration` on the domain condition and every generic condition equals the WatchRule
  generation after a `sourceNamespace`, GitTarget policy, or ClusterProvider flag change — preventing
  a stale success from rendering as healthy.

### Silent-failure guards — `internal/watch`

- **Fingerprint:** two rules differing only in `sourceNamespace` produce different
  `watchRuleFingerprint` values. Without this, step 6's omission is invisible until a stale watch is
  noticed in production.
- **Selection:** `collectWatchRuleSelections` emits the effective source namespace — assert directly
  on the resulting `watchSelection.namespace`.
- **Re-projection:** changing `sourceNamespace` on a stored rule rebuilds the watched-type table
  (`rulesFingerprint` gates the rebuild at
  [watched_type_resolver.go:88-96](../../../internal/watch/watched_type_resolver.go#L88-L96)).

### Reactivity — envtest

- Flipping `allowWatchRuleSourceNamespaceOverride` re-reconciles affected WatchRules. This
  specifically catches the generation-predicate gap: without the new mapper the flag change reaches
  the GitTarget's *status* only, and the WatchRules never re-run.
- Editing `GitTarget.allowedSourceNamespaces` re-reconciles its WatchRules — should pass on existing
  wiring; assert it so a later predicate change cannot silently break it.
- Adding/removing the matching label on a **source-cluster** Namespace grants/revokes within a
  bounded time.

### End-to-end

- A WatchRule in tenant-acme with `sourceNamespace: repo-config` against a remote source cluster
  produces Git paths under `repo-config/…`, **not** `tenant-acme/…`. This is the end-to-end proof of
  the appendix below, and the one assertion that would catch a regression there.
- A refused override surfaces `SourceNamespaceAuthorized=False`, Ready=False, Reconciling=False,
  Stalled=True, kstatus Failed, and writes nothing to Git.

## Done when

- The legacy test above passes and no existing WatchRule changes behavior.
- A denied override leaves no stream running.
- `task lint`, `task test`, `task test-e2e` pass.
- PR 4 is queued — the field must not reach a release without its ClusterWatchRule half.

---

## Appendix A: the source object's namespace already names the Git folder

Verified against the tree on 2026-07-20. Evidence, not argument: it exists so the next reader need
not re-derive it, and so a regression is detectable.

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

The resync/reconcile path ingests it the same way at a second site: `desiredFromObject` also reads
`u.GetNamespace()` ([scope_resolve.go:200](../../../internal/watch/scope_resolve.go#L200)).

The config-plane namespace travels in a **separate field**, `git.Event.GitTargetNamespace`
([git/types.go:343](../../../internal/git/types.go#L343)). Every non-test use falls into exactly
three buckets — GitTarget/credential resolution, worker and commit-window identity keying, and the
write-permission guard plus logging. No `PlacementRequest` literal references it, and
`event.Identifier` is never rewritten from it.

The one place a config-plane namespace becomes a "namespace" in the data plane is
`namespace: rule.Source.Namespace`
([watched_type_resolver.go:297](../../../internal/watch/watched_type_resolver.go#L297)) — a **watch
selector only**, discarded once the stream is open, because the identifier is rebuilt from
`u.GetNamespace()`. That line is precisely what step 4 changes.

**So the write side needs zero change**: a rule in tenant-acme watching `repo-config` remotely already
renders `repo-config/…`. Three invariants confirm this is correct semantics rather than an accident:

1. A placement template for a core Secret must be identity-complete — it must contain `{name}` and
   either `{namespace}` or `{namespaceOrCluster}`
   ([placement.go:550-552](../../../internal/manifestanalyzer/placement.go#L550-L552), enforced
   statically at
   [gittarget_placement_validation.go:67](../../../internal/controller/gittarget_placement_validation.go#L67)).
   A config-plane `{namespace}` would collapse two source namespaces onto one path and silently break
   uniqueness. The enforcement is admission-time and covers core Secrets; operator-configured
   sensitive types rely on write-time guards instead.
2. The store keys `ByResourceIdentity` off the namespace **as written in the Git file**
   ([store.go:1033](../../../internal/manifestanalyzer/store.go#L1033)), which must equal the live
   object's. Strictly this is the *effective* identity: a namespace-less document can inherit its
   namespace from a kustomization's `namespace:` transformer, with provenance in `NamespaceSource`.
   Either way it is never a config-plane namespace.
3. Sibling inference pins on the source object's namespace (`cohortMembers`,
   [placement.go:661](../../../internal/manifestanalyzer/placement.go#L661)), and the
   `spansMultipleNamespaces` guard
   ([placement.go:843](../../../internal/manifestanalyzer/placement.go#L843)) reads the *existing
   store documents'* namespaces to prove a candidate file is namespace-agnostic. Two different
   sources, neither of them config-plane.
