# PR 4 review follow-ups

> Work list from two reviews of PR 4 ([#259](https://github.com/ConfigButler/gitops-reverser/pull/259)):
> an automated pass and an independent review agent. **Every claim below was re-verified against the
> code before being accepted**, and one of the two reported blockers did not survive that check — its
> premise is empirically false, though its recommendation still stands for a different reason. This
> document is the work intake, not a second design: nothing here changes the
> [scope-by-kind decision](pr4-cluster-scope-only.md).

At the time of writing all CI is green — lint, unit, and all six e2e legs — so nothing here was caught
by a gate. That is the point of writing it down: these are the failures the suite does not have a test
for yet, and each item below names the test that would have caught it.

## Summary

| # | Item | Verdict | Status |
|---|---|---|---|
| B1 | Source scope is resolved against the **config plane** for a GitTarget that has not declared yet | Confirmed | **Fixed** |
| B2 | `allowedSourceNamespaces.names: ["*"]` is accepted and silently mirrors nothing | Confirmed, **severity corrected** | **Fixed** |
| F1 | `refreshSourceNamespaceScopes` is serial and unbounded — one hung source cluster stalls every tenant's reconcile | Confirmed | **Fixed** |
| F2 | A retained source-scope grant survives WatchRule deletion | Confirmed | **Fixed** |
| D1 | Migration guidance overstates deny-by-default | Confirmed | **Fixed** |
| D2 | The historical baseline says PR 4 defers selector wildcards; PR 4 ships them | Confirmed | **Fixed** |
| D3 | A facts doc still describes the old ClusterWatchRule model | Confirmed | **Fixed** |
| — | Split `SourceScopeService`; deep-copy the remaining `CompiledResourceRule` slices; move tests | Declined / deferred | Not taken |

All seven landed on this branch. Each section below keeps the analysis that justified the fix; the
**Landed** note at its end records what was actually done and which test holds it.

## B1 — an undeclared GitTarget resolves its selector against the wrong cluster

**Blocker.** [`ResolveSourceNamespace`](../../../internal/watch/source_namespace_scope.go) and
`EnumerateSourceNamespaces` both key the Namespace snapshot on
[`clusterIDForGitTarget`](../../../internal/watch/cluster_context.go), which *deliberately* hides the
not-yet-declared case behind the config-plane default:

~~~go
if id := m.gitTargetClusters[gitDest.Key()]; id != "" {
    return id
}
return configPlaneClusterID
~~~

That default is correct for the read paths it was written for — a status read racing the first
`Declare` — and wrong for this one. Authorization is not a status read: for a **remote** GitTarget the
selector is then evaluated against **config-plane** Namespace labels, so a namespace admitted here can
be a namespace the source cluster never labelled, and vice versa. The window is real because the
WatchRule reconcile does not wait on the GitTarget's `Declare`: it resolves the GitTarget and
GitProvider and gates immediately ([`watchrule_controller.go`](../../../internal/controller/watchrule_controller.go)),
while `DeclareForGitTarget` is called by the *GitTarget* controller
([`gittarget_controller.go`](../../../internal/controller/gittarget_controller.go)). After a restart the
two run concurrently.

Startup bootstrap is **not** the exposed path, contrary to how the finding was first reported.
`BootstrapRules` runs before any refresh, so every snapshot is missing, every selector question is
`Unknown`, and the rule is simply left uncompiled — fail-closed. The exposure is the ordinary
reconcile, once the config-plane snapshot has synced.

**Fix.** Key the snapshot on `target.SourceCluster()` — the GitTarget the resolver already holds —
rather than on the Declare-time cache. There is no id-mapping subtlety to get wrong: the controller
passes `target.SourceCluster()` to `DeclareForGitTarget` **verbatim**, so the two agree by
construction. (Note the fallback is not even locally equivalent: an undeclared target resolves to
`configPlaneClusterID` — the empty string — while a declared local one is stored under `"default"`.
Those are two different `clusterContext`s with two different snapshots.)

**Test.** A bootstrap/reconcile test with **divergent** config-plane and remote Namespace labels: a
namespace that the config plane admits and the remote does not must not be admitted. Without divergent
labels the test passes against the bug.

**Landed.** Both resolver entry points key on `sourceScopeClusterID(target)` —
`target.SourceCluster()` — in [`source_namespace_scope.go`](../../../internal/watch/source_namespace_scope.go),
held by `TestResolveSourceNamespace_ReadsTheGitTargetsOwnCluster`. Two things worth recording:

- The **existing** tests had to change, and that is the strongest evidence the defect was real. Seven
  of them seeded the config-plane snapshot for a GitTarget naming ClusterProvider `workspaces`, and
  passed only because the resolver read the wrong cluster. They now seed the target's own cluster.
- It also fixes a latent enqueue miss nobody had noticed. `enqueueSourceNamespaceChange` matches the
  armed cluster id against `gitTargetClusters`, whose values are `SourceCluster()` — so an
  undeclared target that armed `""` could never match itself, and its grants and revocations waited
  for the periodic requeue instead of arriving on the edge.

## B2 — `names: ["*"]` is accepted, and it is a silent no-op

**Blocker, for release-timing reasons rather than severity.** [`NamespaceMatcher.Names`](../../../api/v1alpha3/namespace_matcher.go)
carries no per-item validation, and
[`expandWildcard`](../../../internal/authz/source_namespace.go) copies policy names verbatim into the
resolved scope:

~~~go
admitted := append([]string(nil), policy.Names...)
~~~

So `names: ["*"]` plus a `sourceNamespace: "*"` item resolves to a scope containing the literal string
`*`.

**The reported severity was "authorizes all namespaces". That is not what happens.** The claim rests on
Kubernetes interpreting `*` as an all-namespaces list/watch. It does not — it treats it as a literal
namespace name. Measured against the live e2e cluster, which holds 23 ConfigMaps across namespaces:

~~~console
$ kubectl get --raw '/api/v1/namespaces/*/configmaps?limit=3'
{"kind":"ConfigMapList","apiVersion":"v1","metadata":{"resourceVersion":"22812"},"items":[]}

$ kubectl get --raw '/api/v1/namespaces/*/configmaps?watch=true&timeoutSeconds=4'
                                            # no events, clean exit
$ kubectl get --raw '/api/v1/configmaps?watch=true&sendInitialEvents=true&…'
{"type":"ADDED","object":{"kind":"ConfigMap",…,"namespace":"cert-manager",…   # control: streams
~~~

The percent-encoded form behaves identically. The event-matching half agrees: `matchesSourceNamespace`
in [`store.go`](../../../internal/rulestore/store.go) is exact string equality, with no glob.

So the real defect is the *opposite* of an escalation: the rule reports `SourceNamespaceAllowed` and
`Ready`, plans one stream against a namespace that cannot exist (namespace names are DNS labels), and
mirrors **nothing** — while the operator believes they granted everything. A silent no-op wearing a
green condition is the failure mode this design already refuses elsewhere: it is exactly why
`NoAdmittedSourceNamespaces` exists as a distinct reason.

**Fix.** Reject `*` in `NamespaceMatcher.names` at the CRD level (DNS-label validation on the items),
and defensively at runtime so an object stored before the validation lands fails loudly rather than
silently narrowing. `*` stays valid **only** in `WatchRule.spec.rules[].sourceNamespace`. The
"admit every namespace" declaration is, and remains, `selector: {}` — which is the form that carries
the snapshot and audit guarantees.

This is a blocker because of *when*, not *how bad*: adding an API rejection after release is itself a
breaking change, so it belongs in the same breaking release as the rest of PR 4. Both `NamespaceMatcher`
users are affected — `GitTarget.spec.allowedSourceNamespaces` and
`ClusterProvider.spec.allowedNamespaces`.

**Landed.** `Names` carries DNS-1123 item validation
([`namespace_matcher.go`](../../../api/v1alpha3/namespace_matcher.go)), which both CRDs inherit from
the shared type, plus `ValidateNames()` for the stored-object case. The gate treats a failure as
`SourceScopeUnavailable` — a policy that cannot be evaluated, not a smaller one — so the
establishing/maintaining contract handles it correctly in both directions. Held by
`TestNamespaceMatcher_ValidateNamesRejectsPatterns` and
`TestResolveWatchRuleSourceScope_PatternInPolicyNamesCannotBeEvaluated`, including that one bad entry
condemns the whole policy: admitting the well-formed remainder is the silent narrowing this design
refuses everywhere else. `ClusterProvider.spec.allowedNamespaces` gets the schema rejection; it needs
no runtime half, because there a literal `*` already fails closed (it admits no namespace) rather
than narrowing one.

## F1 — one hung source cluster stalls every tenant's reconcile

[`refreshSourceNamespaceScopes`](../../../internal/watch/source_namespace_scope.go) walks wanted
clusters **sequentially**, and the Namespace `List` runs on the shared source-cluster config — which
[deliberately carries no `rest.Config.Timeout`](../../../internal/watch/source_cluster_resolver.go) so
that long-lived watches are never cut off. Only the *dial* is bounded (15s). A cluster that accepts the
connection and then hangs on the response blocks indefinitely, and `ReconcileForRuleChange` never
reaches `refreshWatchedTypeTables` / `refreshRunningTargetWatches`
([`manager.go`](../../../internal/watch/manager.go)).

This is the same failure the catalog refresh already solved, one file over — its own comment states the
rule:

> Serial refresh made total latency grow as remoteCount × the discovery timeout — one unreachable
> remote could burn the full timeout before the next even started, delaying every other tenant.
> — [`manager_catalog.go`](../../../internal/watch/manager_catalog.go)

**Fix.** Follow that precedent exactly: bound the finite List with a request timeout stamped on a
**copy** of the config (as `clusterDiscovery` does in
[`cluster_context.go`](../../../internal/watch/cluster_context.go), so the shared config's watches are
never deadlined), and refresh remotes with the same bounded concurrency as
`refreshRemoteCatalogsConcurrently`.

**Test.** A stalled fake source cluster must not prevent the tables from refreshing.
`source_namespace_scope.go` is the PR's least-covered file (**61.3%** patch coverage, 68 lines missed —
its refresh failure paths are almost entirely untested), so this fix carries the coverage debt with it.

**Landed.** Each cluster is listed on its own goroutine under `sourceNamespaceListTimeout`, bounded by
`maxConcurrentSourceNamespaceRefreshes`. A `context.WithTimeout` is the right tool here, unlike the
discovery path that needs a `rest.Config` timeout — `List` takes a context, `ServerGroupsAndResources`
does not. Two tests: `…_BoundsEveryClustersList` asserts every list runs under a deadline no longer
than the bound, and `…_OneWedgedClusterCannotStarveTheOthers` uses a barrier that only falls through
once every cluster is inside its list at the same moment, which a serial loop can never reach.

## F2 — a retained grant outlives its WatchRule

The delete path in [`watchrule_controller.go`](../../../internal/controller/watchrule_controller.go)
removes the rule from the store and triggers a manager reconcile, but never calls
`ForgetSourceScopeGrant`. Grants are keyed by `NamespacedName` **and** spec hash, so a delete followed by
recreating the same name with the same spec inherits the old grant.

That matters because the grant is precisely what distinguishes *establishing* a scope from
*maintaining* one, and the two branches are deliberately opposite. A recreated rule is establishing:
an unevaluatable policy must produce the terminal, actionable refusal (`False` /
`SourceNamespacePolicyUnavailable` / `Stalled=True`). Inheriting the grant makes it read as
maintaining instead, so it reports `Unknown` and `Reconciling` indefinitely — nothing runs, and
nothing says why. Note the precise consequence: it does **not** resurrect a stream, because the
maintaining branch compiles nothing and the deleted rule was already out of the store. The damage is
a rule stuck silently in-progress under a name a different tenant may now own. The
`ForgetSourceScopeGrant` docstring already says it is called "on a REFUSAL or a deletion"; the
deletion half was never wired.

**Test.** Delete and recreate under the same key with the resolver unavailable — the recreated rule must
refuse rather than report progress.

**Landed.** The delete branch of `Reconcile` now calls `ForgetSourceScopeGrant`, held by
`TestReconcile_DeletedWatchRuleForgetsItsRetainedScope`, which recreates a byte-identical rule —
the case the spec hash cannot catch, so only forgetting the grant can. Verified to fail against the
unfixed controller (it reports `Stalled=False`, i.e. the silent in-progress state) and pass with it.

## Docs

- **D1 — deny-by-default is overstated.** [`README.md`](README.md) and
  [`pr4-cluster-scope-only.md`](pr4-cluster-scope-only.md) both tell operators that a target with no
  declared policy "admits nothing". The code disagrees: `ReasonLegacySourceNamespace` in
  [`source_namespace.go`](../../../internal/authz/source_namespace.go) admits a rule whose every item
  watches its own namespace against a target that declares no policy. As written, UPGRADING.md would
  tell every existing operator their rules break. Qualify it: the warning applies to **converted
  wildcard or cross-namespace items**.
- **D2 — the historical baseline contradicts what shipped.**
  [`historical-top-level-source-namespace-baseline.md`](historical-top-level-source-namespace-baseline.md)
  says "PR 4 defers selector-backed wildcards". PR 4 ships them, and resolves them from the
  source-namespace snapshot. Correct the row or mark it superseded.
- **D3 — a facts doc still carries the old model.**
  [`kubernetes-api-discovery.md`](../../facts/kubernetes-api-discovery.md) says ClusterWatchRule "can
  watch cluster-scoped and namespaced resources (by rule scope)". Cluster-scoped only, as of PR 4.

**Landed.** All three corrected. D1 is stated as the two halves that pull in opposite directions —
own-namespace items keep working untouched, wildcard and cross-namespace items are denied — because
the unqualified denial is the more expensive error: it tells every existing operator their rules
break on upgrade, which is false.

## Declined and deferred

- **Split `SourceScopeService` into resolver + retained-grant cache.** Reasonable, and the observation
  that callers use `RetainedSourceScope`'s slice as a boolean is fair — but it is a refactor of an
  interface this PR introduces, and it does not change behavior. Better as its own change once B1/F2
  have settled where the cluster key comes from, since both touch the same seam.
- **Deep-copy the remaining `CompiledResourceRule` slices** (`Operations`, `APIGroups`, `APIVersions`,
  `Resources`) in [`store.go`](../../../internal/rulestore/store.go). No consumer mutates them, and
  `deepCopyCompiledClusterRule` is shallow in the same way — so this is pre-existing, uniform, and not
  PR 4's to change. If it lands, it lands on its own.
- **Assert on `kubectl create namespace` in the e2e suite.** Actively wrong here: every suite under
  [`test/e2e/`](../../../test/e2e/) uses `_, _ =` with an explicit *"idempotent; ignore AlreadyExists"*
  comment, so asserting breaks reruns against a warm cluster.
- **Rename `TestWatchRuleSourceNamespaceDecision_…`.** Already done — it is
  `TestSourceNamespaceDecision_TerminalClassification`. The comment was stale.
- **Move the type tests out of `namespace_matcher_test.go`.** Those tests cover one contract across the
  three types that share the shape; splitting them by type scatters it.

## Sequencing

B1, B2, F2 and the docs are all in-scope for PR 4 itself — B2 because an API rejection added after
release is a second breaking change, and B1/F2 because they are authorization defects in the feature
this PR exists to add. F1 is a stability fix in the same package and rides along.

PR 4 remains blocked from release by [PR 5](pr5-gittarget-deletion-safety.md) either way: the
do-not-release window between the two merges is unchanged by anything here.

Validation is the standard sequence — `task lint`, `task test`, `task test-e2e` — with the e2e legs run
sequentially. The new tests raised unit coverage from 77.5% to 77.8%, so the auto-bumped
`.coverage-baseline` is committed with them.
