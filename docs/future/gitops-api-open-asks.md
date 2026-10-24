# The gitops-api team's open asks

Status: **open asks from a production user**, condensed on 2026-10-24 from their standing list
(revision 15, 2026-10-20, written against `v0.53.0`). The asks keep their numbers. Closed asks are
left out: #30 shipped as `GitReceiver` (#432), #33 went with the prefetch in #425, #22 is fixed
(`pkg/manifestanalyzer/repo.go`), #26 and #27 shipped (`git_branch_targets`,
`commit_requests_total`), #25 shipped in 0.44.0 (#343), and #6 is answered by immutability that
fails at admission with "delete and recreate". Kind and field names are updated to `main`
(`FluxClaim` for `FluxTrigger`, `KubeProvider` for `ClusterProvider`, `ClaimedResourceReconciled`
for `ResumedElsewhere`).

Their suggested order, as of revision 15: #32, then #31, then #34 when the e2e budget allows;
then #23, #11 and #10; #28 when two owners share a cluster.

## 32. A troubleshooting row: something replaces the Kustomization's whole spec

**Ask.** One row in [the Flux guide](../bi-directional-flux.md)'s troubleshooting table: repeated
`ClaimedResourceReconciled` Events mean a controller writes the `Kustomization` with a full-spec
Update that omits `spec.suspend`; make it carry the field across.

**Why.** The guide covers a manifest that *sets* `spec.suspend` (`SuspendOwnedElsewhere`). The more
common case in practice is a controller that never sets the field and replaces the whole spec
(`controllerutil.CreateOrUpdate` with a spec it builds from scratch). An Update that removes a field
another manager owns takes no ownership of it, so `SuspendOwnedElsewhere` can never fire: the
symptom is a resume Event plus an `OperationUnresolved` on every reconcile of that controller, and
Flux applying unchecked until the claim suspends it again. They reproduced it on purpose. The fix on
their side was three lines; finding it from the guide alone would not have been obvious.

## 31. Count "resumed elsewhere" in a metric, not only in an Event

**Ask.** A counter for an apply the claim did not start (the resume, and the `OperationUnresolved`
that follows it), labeled like `apply_requests_total`. Possibly also a timestamp in the claim's
status.

**Why.** A resume behind the operator's back is the one way left for Flux to apply unchecked, so it
is what they most want to alarm on. Today it is only an Event, and an Event is a poor alarm source:
the API server aggregates identical ones (three resumes in three minutes showed as one Event with
`count: 3`) and drops them after an hour. They also saw the detection time vary from 2s to about
75s, which is the claim's steady poll; a watch on the held applier would close that window. That
watch is designed in [watching the held applier](../design/applier-claim-held-applier-watch.md).

## 34. An e2e for an in-cluster applier with a remote source cluster

**Ask.** One e2e where the `GitTarget`'s `KubeProvider` names a remote cluster and the `FluxClaim`'s
names the operator's own, with an API edit landing during an apply.

**Why.** It is their production topology: Flux runs in the management cluster and applies into a
remote workspace through `spec.kubeConfig`; GitOps Reverser reads that workspace remotely and holds
the `Kustomization` locally. They proved the hold, the apply cycle, the receiver and `ForeignCommit`
in it, but never saw the apply window withhold anything (`apply_window_collisions_total` stayed at
zero), so the part that reads `managedFields` on the remote object is untested in this topology.
An e2e that provokes one collision would settle it.

Two smaller things they noted: `OperationUnresolved` is recorded three times for one operation (on
the claim, the `GitTarget` and the `WatchRule`), and `apply_requests_total{outcome="sent"}` was
said to count both "the applier was asked" and "nothing needed asking". (On `main` an apply that is
not needed is never sent, so it is not counted; check whether that answers them.)

## 28. Two operator instances cannot share one config-plane cluster

**Ask.** A way for two instances sharing one config-plane cluster to partition the CR set, so that
exactly one instance reconciles a given `GitTarget`. Smallest useful first, and they stack:

1. **`--watch-namespaces`**, a `Cache.DefaultNamespaces` pass-through. It partitions the namespaced
   kinds, which since #430 includes `KubeProvider`; only `ClusterWatchRule` stays cluster-scoped.
2. **An explicit owner selector**, a label selector or a `spec.instance`-style field paired with an
   `--instance-name` flag, which covers the cluster-scoped kinds too.

**Why.** It is the same mechanism the chart's replica guard
(`charts/gitops-reverser/templates/validate-replica-count.yaml`) says is missing for HA: "Standby
replicas need ownership coordination first, so that two instances never write the same GitTarget."
They hit it moving onto a cluster that already runs an instance for its owner's own workloads: two
populations, different source clusters, providers, credentials and namespaces, and still both
instances would reconcile both sets. RBAC is no workaround: controller-runtime issues a cluster-wide
List, so a narrowed ClusterRole fails the cache sync and the manager exits.

**What sharing costs them**, for calibration: one Valkey holding both populations' attribution
facts; one upgrade cadence; one RBAC posture that is the union of both; and, customer-visible, a
restart for one owner's reasons costs the other's users their commit message, because
`RenderMatchesLive` starts `Unknown` after a restart and an open window is dropped. They are not
blocked: one shared instance works.

## 23. Deletion-as-intent picks the wrong actor when a controller clears the finalizer

**Probably fixed by #431; verify with their reproduction.** #431 credits a terminating object's
removal to its deletion evidence first.

**Ask.** Attribute a deletion-as-intent `DELETE` to the actor who **set** `deletionTimestamp`, not
to whoever makes the next modification while it is set.

**Why.** Measured on `v0.40.1`: a human deletes a CR that their operator finalizes, and the commit
names the operator's service account. The operator's finalizer-removal `UPDATE` is itself a
modification of a terminating object, so it renders as the `DELETE` and wins the commit; the human's
own delete arrives a moment later and folds to a no-op because the file is already gone. The more
finalizers an object has, the more likely a controller's cleanup write is observed first.

**Reproduction** (about 90 seconds): create a namespaced CR with a finalizer, mirror it with a
`ClusterWatchRule`, delete it as a human, let the controller clear the finalizer, read the commit.

## 11. Serialize `[CREATE]` and `[UPDATE]` bodies identically

**Ask.** One encoder for both paths.

**Why.** A create rendered list items at 2-space indent, an update at 4, so every install churned
all the lines of a list to carry one field. Unverified since `v0.35.0`, but the two paths still
reach different encoders (`pkg/manifestanalyzer` renders via JSON to YAML; `internal/git/manifestedit`
uses `yaml.v3` with `SetIndent(2)`). Cosmetic, but it makes `git diff` on the mirror useless for
seeing what changed.

## 10. Sibling inference can place an object in another namespace's directory

**Ask.** Consider the namespace when inferring where a *new* document goes. Cosmetic.

**Why.** The cold-start layout is namespace-first, but once a layout exists, placement follows
siblings. Two tenants syncing a repository named `infra` produce two objects named `infra` in
different namespaces, and one lands in the other's directory. Nothing is corrupted, but
`git log -- tenants/tenant-a/` shows another tenant's history.

## A possible #24: say when a stored field was pruned

From their 0.43 upgrade: a deleted field is pruned by the API server, so the object stays
`Ready=True` and quietly uses a default or a wider scope. They had to write a test asserting the
*absence* of the old spellings. A condition or Event on an object applied carrying a field the CRD
no longer serves would make a migration self-diagnosing. Not filed as an ask.
