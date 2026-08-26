# PR 3 — a ClusterWatchRule may attach to a GitTarget its provider never admitted

> Phase 3 of [source-namespace addressing](README.md). **Depends on:** nothing — independent of the
> other PRs and orderable at any point. Bug fix — no API change, no CRD regeneration.
>
> **Status: landed.** The admission decision moved to `internal/authz.GitTargetAdmitted` and is now
> applied by all three call sites that compile a rule or start a data plane for a GitTarget — the
> GitTarget reconciler, the ClusterWatchRule reconciler, and the watch manager's startup bootstrap.
> A refusal stops the data plane *before* it publishes `GitTargetNamespaceNotAuthorized`, and two new
> mappers (ClusterProvider → ClusterWatchRules, Namespace → ClusterWatchRules) make a revocation
> converge on the event rather than on the ~10 minute periodic reconcile. The rest of this page is
> the record of what was wrong and what shipped; sections below are past-tense by design, so a
> regression is recognisable against them.

## What this PR is, precisely

It is a **RuleStore and data-plane consistency guard**: it makes the rule-compilation path apply the
same ClusterProvider admission check the GitTarget controller already applies, so a ClusterWatchRule
cannot compile against a GitTarget that admission rejected.

It is **not** a new consent mechanism, and the plan should not be read as adding one. There is no
target-side authorization policy here — nothing lets a GitTarget owner say which ClusterWatchRules
may reference their target. The check re-applies the *same* provider→GitTarget-namespace admission
from a second call site. A genuine target-side consent policy (say,
`GitTarget.spec.allowedClusterWatchRules`) would be a separate API decision and is not proposed.

What this PR does buy is that the admission decision is enforced wherever rules are compiled, rather
than only where GitTargets are reconciled — which matters because those two paths can disagree, as
below.

## The defect

The ClusterWatchRule reconciler resolves its GitTarget with a plain `r.Get` and **no authorization
check of any kind**
([clusterwatchrule_controller.go:160-162](../../../internal/controller/clusterwatchrule_controller.go#L160-L162));
the 537-line file contains no authorization call at all — confirmed by grep, and `AllowsNamespace`
has exactly one non-test call site anywhere in the tree
([gittarget_source_cluster.go:68](../../../internal/controller/gittarget_source_cluster.go#L68)).
`bootstrap.go` seeds rules the same way
([bootstrap.go:71-91](../../../internal/watch/bootstrap.go#L71-L91)).

Since `ClusterWatchRule.targetRef` is a `NamespacedTargetReference` with a **required** namespace
([clusterwatchrule_types.go:22-44](../../../api/v1alpha3/clusterwatchrule_types.go#L22-L44)), any
ClusterWatchRule may attach itself to any GitTarget in any namespace and widen that target's mirror
scope to cluster-wide, without the compilation path ever consulting the ClusterProvider's admission
policy for that target's namespace.

`allowedNamespaces` is the ClusterProvider's explicit admission of the **GitTarget namespace** to use
that provider. The compilation path never consults it.

## Not an escalation today — and why to fix it anyway

ClusterWatchRule is cluster-scoped, so only a config-plane cluster-admin can create one, and that
subject can already read the kubeconfig Secrets directly. Nobody gains access they lacked.

The gate is also effective *transitively*, which is worth knowing before assuming a live hole:
`checkSourceAuthorization` runs inside the Validated gate and returns before `DeclareForGitTarget`
([gittarget_controller.go:218](../../../internal/controller/gittarget_controller.go#L218)), which is
what populates `gitTargetClusters` and creates the `targetWatches` entry. The rule-change path cannot
bootstrap a watch on its own — `refreshRunningTargetWatches`
([target_watch.go:175-193](../../../internal/watch/target_watch.go#L175-L193)) snapshots the
*existing* `targetWatches` keys and skips any table whose destination is not already running. So a
ClusterWatchRule pointing at an unauthorized GitTarget builds a resident table but starts no stream.
`bootstrap.go` is benign for the same reason.

Fix it regardless, for two reasons. First, the transitive protection is incidental: it depends on
ordering inside a controller that nobody is currently required to preserve, so a future refactor of
`DeclareForGitTarget` silently converts it into a real hole with no test to catch that. Second, it
makes `allowedNamespaces` mean what it says. A platform admin reading a ClusterProvider's admission
list should be able to conclude that no rule anywhere is mirroring through that credential on behalf
of an unadmitted target.

Note one thing that is *already* confined: `providerNS := target.Namespace`
([clusterwatchrule_controller.go:173](../../../internal/controller/clusterwatchrule_controller.go#L173)),
so the GitProvider is resolved in the GitTarget's own namespace. The unchecked edge is
rule → GitTarget only.

## The fix

Factor the GitTarget provider-admission check into a shared helper and run it for the referenced
GitTarget's namespace before a ClusterWatchRule is stored — in **both** the reconciler
([clusterwatchrule_controller.go:160](../../../internal/controller/clusterwatchrule_controller.go#L160))
and [bootstrap.go:71-91](../../../internal/watch/bootstrap.go#L71-L91). A helper used by one of the
two paths is the failure mode to avoid: bootstrap runs before the reconciler on every restart.

On denial: remove any existing compiled ClusterWatchRule, replan the watch manager to stop its
stream, then set `GitTargetReady=False` with reason `GitTargetNamespaceNotAuthorized` and publish
the terminal kstatus trio (`Ready=False`, `Reconciling=False`, `Stalled=True`). Stop the data plane
*before* publishing status — a gate that only writes a condition is not a gate.

Changes to a ClusterProvider's `allowedNamespaces` must requeue affected ClusterWatchRules, so a
later revocation has the same effect as an initial denial. The ClusterProvider → GitTargets mapper
already exists
([gittarget_controller.go:1138-1142](../../../internal/controller/gittarget_controller.go#L1138-L1142));
ClusterProvider → ClusterWatchRules does not.

This check is separate from, and does not replace,
[`GitTarget.allowedSourceNamespaces`](README.md#the-model). They answer different
questions: provider admission asks *may this target use this credential at all*, the ceiling asks
*which source namespaces may reach this target's destination*.

## Tests

- **Direct refusal:** a ClusterWatchRule referencing a GitTarget whose namespace the ClusterProvider
  does not admit is refused in the reconciler **and** in the bootstrap path. The reconciler case
  leaves no compiled rule and no running stream, and sets `GitTargetReady=False`, `Ready=False`,
  `Reconciling=False`, `Stalled=True` with reason `GitTargetNamespaceNotAuthorized`.
- **Revocation:** start from an admitted, running ClusterWatchRule, then remove its GitTarget
  namespace from `ClusterProvider.allowedNamespaces`. The new mapper must requeue it, remove the
  compiled rule, stop the stream, and publish the same terminal status.
- **Admission still works:** an admitted target's ClusterWatchRule runs unchanged — the regression
  guard for a helper that is accidentally too strict.
- **Ordering:** assert the compiled rule is gone *before* the terminal condition is observable, or at
  minimum that no stream survives a refusal, so a status-only implementation fails.

## Done when

- Both the reconciler and `bootstrap.go` call one shared admission helper.
- A provider policy change requeues ClusterWatchRules, not only GitTargets.
- `task lint`, `task test`, `task test-e2e` pass.
