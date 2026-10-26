# The gitops-api team's open asks

Status: **open asks from a production user**, condensed on 2026-10-24 from their standing list
(revision 15, 2026-10-20, written against `v0.53.0`). The asks keep their numbers. Closed asks are
left out: #30 shipped as `GitReceiver` (#432), #33 went with the prefetch in #425, #22 is fixed
(`pkg/manifestanalyzer/repo.go`), #26 and #27 shipped (`git_branch_targets`,
`commit_requests_total`), #25 shipped in 0.44.0 (#343), and #6 is answered by immutability that
fails at admission with "delete and recreate". Since revision 15: #32 is a row in
[the Flux guide](../bi-directional-flux.md#troubleshooting); #31 is
`apply_claimed_resource_reconciled_total`, labeled by applier kind and claim; ask #23 is pinned at the
fact index, for a finalizer cleared by a PATCH or an UPDATE, in either order
(`TestFactIndex_AFinalizerClearedByAControllerNamesTheDeleter`); and #11 shipped in 0.41.0 (#290):
create and update write through one encoder, `internal/yamlstyle`, and a test fails on a second
one. A file GitOps Reverser did not write is re-indented once, on its first edit. Kind and field
names are updated to `main` (`FluxClaim` for `FluxTrigger`, `KubeProvider` for `ClusterProvider`,
`ClaimedResourceReconciled` for `ResumedElsewhere`).

Their suggested order, as of revision 15: #34 when the e2e budget allows; then #10; #28 when two
owners share a cluster. #28 is deferred: see below for why.

## 34. An e2e for an in-cluster applier with a remote source cluster

**Ask.** One e2e where the `GitTarget`'s `KubeProvider` names a remote cluster and the `FluxClaim`'s
names the operator's own, with an API edit landing during an apply.

**Why.** It is their production topology: Flux runs in the management cluster and applies into a
remote workspace through `spec.kubeConfig`; GitOps Reverser reads that workspace remotely and holds
the `Kustomization` locally. They proved the hold, the apply cycle, the receiver and `ForeignCommit`
in it, but never saw the apply window withhold anything (`apply_window_collisions_total` stayed at
zero), so the part that reads `managedFields` on the remote object is untested in this topology.
An e2e that provokes one collision would settle it.

Two smaller things they noted, both answered. `OperationUnresolved` on the `GitTarget` and the
`WatchRule` is not recorded again: each object's readiness takes the reason of the one it depends
on (`ApplierClaimReady` on the `GitTarget`, `GitTargetReady` on the rule), so one operation shows on
three objects by design. And `apply_requests_total{outcome="sent"}` counts only an applier that was
asked: an apply that is not needed is never sent, so it is not counted.

## 28. Two operator instances cannot share one config-plane cluster

**Ask.** A way for two instances sharing one config-plane cluster to partition the CR set, so that
exactly one instance reconciles a given `GitTarget`. Smallest useful first, and they stack:

1. **`--watch-namespaces`**, a `Cache.DefaultNamespaces` pass-through. It partitions the namespaced
   kinds, which since #430 includes `KubeProvider`; only `ClusterWatchRule` stays cluster-scoped.
2. **An explicit owner selector**, a label selector or a `spec.instance`-style field paired with an
   `--instance-name` flag, which covers the cluster-scoped kinds too.

**Why.** They hit it moving onto a cluster that already runs an instance for its owner's own
workloads: two populations, different source clusters, providers, credentials and namespaces, and
still both instances would reconcile both sets. RBAC is no workaround: controller-runtime issues a
cluster-wide List, so a narrowed ClusterRole fails the cache sync and the manager exits.

**What sharing costs them**, for calibration: one Valkey holding both populations' attribution
facts; one upgrade cadence; one RBAC posture that is the union of both; and, customer-visible, a
restart for one owner's reasons costs the other's users their commit message, because
`RenderMatchesLive` starts `Unknown` after a restart and an open window is dropped. They are not
blocked: one shared instance works.

**Deferred, not built.** Step 1 was built and dropped before merge (#445). It removed two of the
four costs above, the shared Valkey and the restart reaching the other owner's users. It could not
remove the other two:

- **One upgrade cadence stays.** The CRDs are cluster-scoped, so both instances run against one
  schema, and an API break (we make them without migrations) breaks whichever instance lags.
- **One RBAC posture stays.** The ClusterRole is still cluster-wide.

It is also not the HA mechanism the chart's replica guard
(`charts/gitops-reverser/templates/validate-replica-count.yaml`) asks for. Two instances serving
disjoint namespaces never contend for a `GitTarget`; two replicas of one instance would, and need
leader or ownership coordination instead. Against that, it adds a lasting cost: a second tenancy
model beside one instance serving many namespaces through grants (`consumerNamespaces`,
cross-namespace provider references). Every cross-namespace reference gains a "namespace this
instance does not serve" failure, and every cluster-scoped kind needs its own ownership check.

The cost their users see is the restart dropping an open window. That is better fixed inside one
instance, for every installation: the durable pending writes in [the TODO](../TODO.md). A second
owner who needs its own upgrade cadence or RBAC needs its own cluster; #28 would give it neither.

## 10. Sibling inference can place an object in another namespace's directory

**Ask.** Consider the namespace when inferring where a *new* document goes. Cosmetic.

**Why.** The cold-start layout is namespace-first, but once a layout exists, placement follows
siblings. Two tenants syncing a repository named `infra` produce two objects named `infra` in
different namespaces, and one lands in the other's directory. Nothing is corrupted, but
`git log -- tenants/tenant-a/` shows another tenant's history.

## A possible #24: say when a stored field was pruned

From their 0.43 upgrade: a deleted field is pruned by the API server, so the object stays
`Ready=True` and quietly uses a default or a wider scope. They had to write a test asserting the
*absence* of the old spellings. When an applied object still carries a field the CRD no longer
serves, a condition or `Event` on that object would make a migration self-diagnosing. Not filed as an ask.
