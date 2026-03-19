# Bug Report: Snapshot Reconciliation Ignores Namespace Scope

**Status:** Confirmed -> Resolved!
**Severity:** High — causes spurious commits that break the commit-loop prevention guarantee
**Discovered via:** `make test-e2e-bi` (bi-directional e2e test)

---

## Summary

When the gitops-reverser controller performs a **full cluster snapshot** (the `reconcile: sync N resources from cluster snapshot` code path), it lists resources **cluster-wide** with no namespace filter. This means a `WatchRule` scoped to a single test namespace inadvertently pulls secrets from `cert-manager`, `flux-system`, `kube-system`, `gitea-e2e`, and every other namespace on the cluster into the live Git path.

The consequence is that after Flux applies committed resources back to the cluster, a snapshot is triggered, which produces a commit containing secrets it was never supposed to touch — breaking the commit-loop prevention that is the core feature under test.

---

## Observed Failure

Running `make test-e2e-bi`, the bi-directional e2e test failed at:

```
test/e2e/bi_directional_e2e_test.go:326
Consistently expected commit count to stay at <secretCommitCount> but it grew by 1
```

Inspecting the Git log of the test repo (`.stamps/repos/<run>/bi-directional/<id>/`):

```
97d53b1 reconcile: sync 27 resources from cluster snapshot   ← SPURIOUS
6cc837e [UPDATE] v1/secrets/bi-secret-<id>
9c4d294 [CREATE] v1/secrets/bi-secret-<id>
c2722b7 [CREATE] v1/secret1s/sops-age-key
3179204 [CREATE] v1/secrets/git-creds-ssh-<run>
4dca51e [CREATE] v1/secrets/git-creds-invalid-<run>
f6da0a0 [CREATE] v1/secrets/git-creds-<run>
3743e9b [CREATE] v1/secrets/bi-controller-sops-<id>
d6cc2bf [UPDATE] shop.example.com/v1/icecreamorders/bi-alice-order-<id>
fb396c1 bi-directional: add two icecream orders
0e059d3 chore(bootstrap): initialize path bi-directional/<id>/live
b1760a3 bi-directional: add icecreamorder crd
```

The spurious commit adds 19 files across namespaces the test never owned:

```
live/v1/secrets/cert-manager/cert-manager-webhook-ca.sops.yaml
live/v1/secrets/flux-system/bi-flux-auth-<id>.sops.yaml
live/v1/secrets/flux-system/bi-sops-<id>.sops.yaml
live/v1/secrets/gitea-e2e/gitea-init.sops.yaml
live/v1/secrets/gitea-e2e/gitea.sops.yaml
live/v1/secrets/gitops-reverser/admission-server-cert.sops.yaml
live/v1/secrets/kube-system/k3s-serving.sops.yaml
live/v1/secrets/prometheus-operator/prometheus-prometheus-shared-e2e.sops.yaml
live/v1/secrets/valkey-e2e/sh.helm.release.v1.valkey.v1.sops.yaml
... (19 files total)
```

---

## Root Cause

There are two compounding bugs. Either one alone would prevent the spurious commit; together they explain both *why* the snapshot is wrong and *why* it fires at all after the initial setup.

### Bug 1 — Snapshot lists resources cluster-wide (namespace scoping missing)

### The broken path: `GetClusterStateForGitDest` → `listResourcesForGVR`

**`internal/watch/manager.go:773`**

```go
// List resources (cluster-wide for now, namespace filtering would go here)
list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
```

The call uses `dc.Resource(gvr)` (cluster-wide) instead of `dc.Resource(gvr).Namespace(ns)`. The comment acknowledges this is a placeholder.

**`internal/watch/manager.go:802`**

```go
func (m *Manager) objectMatchesGitTarget(
    _ *unstructured.Unstructured,
    _ *configv1alpha1.GitTarget,
) bool {
    // For now, simple match - in the future could filter by namespace, labels, etc.
    return true
}
```

The post-list filter is a stub that accepts every object unconditionally.

### Bug 2 — Snapshot re-runs when triggered by the wrong events

Re-running the snapshot is not inherently wrong. There are legitimate reasons to do it: a new `WatchRule` is added (the watch scope has grown), a `WatchRule` is deleted (resources may need to be removed from Git), or the controller restarts (the informer cache is gone). In those cases a re-snapshot is correct and necessary — the whole point of reconciliation loops is to converge to the right state no matter how many times they run.

The problem here is that the snapshot re-runs **for the wrong reason, via an accidental path**.

`evaluateSnapshotGate` in `gittarget_controller.go` is called unconditionally on every reconcile loop iteration. Inside it, `StartReconciliation` fires `RequestClusterState` on the reused `FolderReconciler` with no guard:

```go
// gittarget_controller.go:477 — called every reconcile loop
func (r *GitTargetReconciler) evaluateSnapshotGate(...) {
    stream.BeginReconciliation()
    reconciler := r.EventRouter.ReconcilerManager.CreateReconciler(gitDest, stream) // reuses existing
    reconciler.StartReconciliation(ctx) // fires RequestClusterState unconditionally
    ...
}
```

`ReconcilerManager.CreateReconciler` returns the cached reconciler after the first call — so subsequent `StartReconciliation` calls overwrite its internal cluster/repo state with whatever the next snapshot returns.

The correct triggers for a re-snapshot are events that imply the **watch scope changed** (a WatchRule was added, updated, or deleted) or that **incremental state was lost** (controller restart). An encryption secret being touched by Flux says neither of those things — it is an entirely unrelated event that happens to go through the same reconcile loop.

### Why the snapshot fires late — the encryption secret re-trigger

The GitTarget controller watches **all `Secret` objects** in the cluster via `encryptionSecretToGitTargets` (see `SetupWithManager`). It maps a Secret event to a GitTarget reconcile if the secret is the GitTarget's `Encryption.SecretRef` — i.e. the controller-generated SOPS age key.

In the test, the GitTarget's encryption secret is `bi-controller-sops-<id>`. The Secret WatchRule commits this secret to Git (commit #5 in the log). When Flux reconciles the live kustomization it applies `bi-controller-sops-<id>` back to the cluster, firing a `Secret UPDATE` event. That event maps to the GitTarget → GitTarget re-reconciles → `evaluateSnapshotGate` runs a second time → `StartReconciliation` fires again → cluster-wide snapshot.

This is why the spurious commit is **last** rather than first: the initial snapshot (Phase 1 below) found zero matching resources and emitted nothing. The second snapshot is triggered specifically by Flux touching the encryption secret.

### The full event sequence

```
Phase 1 — GitTarget initial setup
  GitTarget created
  → evaluateSnapshotGate (1st run)
  → StartReconciliation → RequestClusterState
  → cluster-wide list: secrets GVR, but NO Secret WatchRule exists yet
  → diff = 0 → no commit emitted
  Bootstrap gate: "chore(bootstrap): initialize path"    ← commit 2

Phase 2 — IceCreamOrders flow normally via informers
  Two orders committed → Flux applies → informers fire UPDATE
  → "[UPDATE] icecreamorders/bi-alice-order-<id>"        ← commit 4

Phase 3 — Secret WatchRule applied
  WatchRule controller starts namespace-scoped informers for secrets
  → ADDED events fire for every existing secret in test-ns
  → "[CREATE] v1/secrets/bi-controller-sops-<id>"        ← commit 5
  → "[CREATE] v1/secrets/git-creds-*"                    ← commits 6–9
  Test creates and patches bi-secret
  → "[CREATE/UPDATE] v1/secrets/bi-secret-<id>"          ← commits 10–11

Phase 4 — Flux reconciles live kustomization (THE TRIGGER)
  Flux applies committed secrets back to cluster
  → bi-controller-sops-<id> receives a Secret UPDATE event
  → encryptionSecretToGitTargets maps it to the GitTarget
  → GitTarget re-reconciles
  → evaluateSnapshotGate (2nd run, no SnapshotSynced guard)
  → StartReconciliation → RequestClusterState (Bug 2)
  → cluster-wide list: all secrets on cluster, 27 resources (Bug 1)
  → diff vs Git (only test-ns secrets present): 19 files "missing"
  → "reconcile: sync 27 resources from cluster snapshot"  ← commit 12 (SPURIOUS)
```

Either fix alone breaks the symptom in this specific case:
- Fix Bug 1 (namespace scope): the re-snapshot only sees test-ns secrets → diff = 0 → no spurious commit, even though the re-snapshot fires.
- Fix Bug 2 (wrong trigger): the re-snapshot never fires for this reason, so Bug 1 is never exercised by this path.

**Bug 1 is the more fundamental correctness problem** — a namespace-unscoped snapshot is wrong regardless of what triggered it. Bug 2 is a design problem: the snapshot should only re-run when the watch scope changes or state is lost, not on every arbitrary GitTarget reconcile. Both should be fixed, but in priority order: fix Bug 1 first because a correctly-scoped snapshot is safe to run repeatedly.

### Why the normal (incremental) event path is correct

The live-event informers in `watch/manager.go` already use `getNamespacesForGVR` / `collectWatchRuleNamespaces` to scope dynamic informers to the WatchRule's own namespace. Individual `[CREATE]`/`[UPDATE]` commits in the log above are correctly scoped to the test namespace. The bug only exists in the **snapshot reconciliation path**, which runs a separate cluster list instead of using the informer cache.

### What data is already available

The rule store already records each WatchRule's source namespace in `CompiledRule.Source.Namespace`. Inside `GetClusterStateForGitDest`, the loop at line 642 already iterates over the matching `wrRules` — the namespace is right there:

```go
for _, rule := range wrRules {
    if rule.GitTargetRef == gitTargetObj.Name &&
        rule.GitTargetNamespace == gitTargetObj.Namespace {
        // rule.Source.Namespace  <-- THIS IS THE NAMESPACE TO LIST IN
        for _, rr := range rule.ResourceRules {
            m.addGVRsFromResourceRule(rr, gvrSet)
        }
    }
}
```

---

## Affected Code

| File | Line | Bug | Issue |
|------|------|-----|-------|
| `internal/watch/manager.go` | 773–774 | 1 | Cluster-wide `List` — namespace not passed |
| `internal/watch/manager.go` | 802–810 | 1 | `objectMatchesGitTarget` always returns `true` |
| `internal/watch/manager.go` | 640–658 | 1 | GVR set built without collecting associated namespaces |
| `internal/controller/gittarget_controller.go` | 477–527 | 2 | `evaluateSnapshotGate` called unconditionally every reconcile loop — no early-exit when `SnapshotSynced=True` |
| `internal/reconcile/folder_reconciler.go` | 86–105 | 2 | `StartReconciliation` has no idempotency guard — re-fires `RequestClusterState` even if snapshot already completed |

---

## Fix Plan

### Step 1 — Thread namespace sets through `GetClusterStateForGitDest`

Change the GVR accumulation loop to also collect the namespaces from each matching WatchRule, building a `map[GVR][]string` (GVR → namespaces to list):

```go
type gvrNamespaces struct {
    namespaces map[string]struct{} // empty means cluster-wide
    clusterWide bool
}
gvrMap := make(map[schema.GroupVersionResource]*gvrNamespaces)

for _, rule := range wrRules {
    if rule.GitTargetRef == gitTargetObj.Name &&
        rule.GitTargetNamespace == gitTargetObj.Namespace {
        ns := rule.Source.Namespace
        for _, rr := range rule.ResourceRules {
            for _, gvr := range gvrsFromRule(rr) {
                entry := gvrMap[gvr]
                if entry == nil {
                    entry = &gvrNamespaces{namespaces: make(map[string]struct{})}
                    gvrMap[gvr] = entry
                }
                entry.namespaces[ns] = struct{}{}
            }
        }
    }
}

// ClusterWatchRules always list cluster-wide
for _, cwrRule := range cwrRules {
    if cwrRule.GitTargetRef == gitTargetObj.Name &&
        cwrRule.GitTargetNamespace == gitTargetObj.Namespace {
        for _, gvr := range gvrsFromClusterRule(cwrRule) {
            entry := gvrMap[gvr]
            if entry == nil {
                entry = &gvrNamespaces{}
                gvrMap[gvr] = entry
            }
            entry.clusterWide = true
        }
    }
}
```

### Step 2 — Namespace-scope the `List` call in `listResourcesForGVR`

Pass the namespace set to `listResourcesForGVR` and use `Namespace(ns)` when listing:

```go
func (m *Manager) listResourcesForGVR(
    ctx context.Context,
    dc dynamic.Interface,
    gvr schema.GroupVersionResource,
    namespaces []string, // NEW: empty = cluster-wide (for ClusterWatchRules)
    objects map[string]unstructured.Unstructured,
) ([]types.ResourceIdentifier, error) {
    if shouldIgnoreResource(gvr.Group, gvr.Resource) {
        return nil, nil
    }

    var allItems []unstructured.Unstructured

    if len(namespaces) == 0 {
        // ClusterWatchRule or cluster-scoped resource: list cluster-wide
        list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
        if err != nil {
            return nil, fmt.Errorf("failed to list %v: %w", gvr, err)
        }
        allItems = list.Items
    } else {
        // WatchRule: list only in the namespaces that have a matching rule
        for _, ns := range namespaces {
            list, err := dc.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
            if err != nil {
                return nil, fmt.Errorf("failed to list %v in namespace %s: %w", gvr, ns, err)
            }
            allItems = append(allItems, list.Items...)
        }
    }

    var resources []types.ResourceIdentifier
    for i := range allItems {
        obj := &allItems[i]
        id := types.NewResourceIdentifier(
            gvr.Group, gvr.Version, gvr.Resource,
            obj.GetNamespace(), obj.GetName(),
        )
        resources = append(resources, id)
        objects[id.Key()] = *sanitize.Sanitize(obj)
    }
    return resources, nil
}
```

### Step 3 — Remove `objectMatchesGitTarget`

The stub is dead code once namespace scoping is in the `List` call. Delete it to avoid future confusion.

### Step 4 — Trigger re-snapshots from rule-change events, not the generic reconcile loop (Bug 2)

The snapshot should re-run when the watch scope changes — i.e. when a `WatchRule` referencing this `GitTarget` is created, updated, or deleted. It should **not** re-run just because the GitTarget's encryption secret was touched.

The right approach is to move the re-snapshot trigger out of `evaluateSnapshotGate` (which runs on every reconcile loop) and into the `WatchRule` controller, which already calls `ReconcileForRuleChange` when rules change. `ReconcileForRuleChange` can emit a `RequestClusterState` event directly, without going through the GitTarget reconcile loop.

`evaluateSnapshotGate` should then be limited to the **initial bootstrap only** — bail out immediately once `SnapshotSynced=True` is set:

```go
func (r *GitTargetReconciler) evaluateSnapshotGate(...) {
    // Initial snapshot only. Re-snapshots on rule changes are triggered by
    // the WatchRule controller via ReconcileForRuleChange, not here.
    if meta.IsStatusConditionTrue(target.Status.Conditions, GitTargetConditionSnapshotSynced) {
        return nil, metav1.ConditionTrue, "Initial snapshot reconciliation completed", 0, nil
    }
    // ... rest of the gate (runs once, on first setup)
}
```

Note: this does not prevent re-snapshotting when needed. It moves the responsibility to the correct trigger point (rule changes) rather than letting it happen accidentally via unrelated events.

---

## Unit Test Plan

### Test location: `internal/watch/manager_snapshot_test.go` (new file)

These tests use a fake dynamic client to assert which namespaces are listed during snapshot reconciliation.

#### Test 1 — WatchRule scopes snapshot to its own namespace

**Setup:** One WatchRule in namespace `ns-a` watching `secrets`. One existing secret in `ns-a`, one in `ns-b`.
**Assert:** `GetClusterStateForGitDest` returns only the secret from `ns-a`.

#### Test 2 — Two WatchRules in different namespaces targeting the same GitTarget

**Setup:** WatchRule-1 in `ns-a` and WatchRule-2 in `ns-b`, both pointing to the same GitTarget, both watching `configmaps`. Configmaps exist in `ns-a`, `ns-b`, and `ns-c`.
**Assert:** Resources from `ns-a` and `ns-b` are returned; `ns-c` is excluded.

#### Test 3 — ClusterWatchRule lists cluster-wide

**Setup:** One ClusterWatchRule watching `nodes`.
**Assert:** All nodes are returned (cluster-wide list, not namespace-scoped).

#### Test 4 — Mixed WatchRule + ClusterWatchRule for same GitTarget

**Setup:** WatchRule in `ns-a` watching `secrets`; ClusterWatchRule watching `nodes`. Resources exist in `ns-a`, `ns-b`, and as cluster-scoped nodes.
**Assert:** Secrets only from `ns-a`; all nodes returned.

#### Test 5 — WatchRule with no existing resources returns empty, not error

**Setup:** WatchRule in `ns-empty` watching `configmaps`. No configmaps exist in that namespace.
**Assert:** Returns empty slice, no error, no resources from other namespaces.

### Test location: `internal/watch/manager_test.go` (extend existing)

#### Test 6 — Regression: secrets from `flux-system` not included when WatchRule is in test namespace

Directly reproduces the observed failure. Creates a WatchRule in `test-ns`, populates secrets in `test-ns` and `flux-system`, runs `GetClusterStateForGitDest`, and asserts none of the `flux-system` secrets appear.

### Test location: `internal/reconcile/folder_reconciler_test.go` (extend existing)

#### Test 7 — Snapshot reconcile commit contains only namespace-scoped resources

Integration-level: stand up a FolderReconciler with a fake cluster state containing resources across multiple namespaces; assert the resulting `ReconcileBatch` only includes resources from the WatchRule's namespace.

#### Test 8 — `StartReconciliation` is idempotent (Bug 2)

Call `StartReconciliation` twice on the same `FolderReconciler`. Assert that `RequestClusterState` is emitted exactly once, not twice. The second call must be a no-op.

### Test location: `internal/controller/gittarget_controller_test.go` (extend existing)

#### Test 9 — `evaluateSnapshotGate` skips re-run when `SnapshotSynced=True` (Bug 2)

Set up a GitTarget with `SnapshotSynced=True` already in its status conditions. Call `evaluateSnapshotGate`. Assert that `StartReconciliation` is never called (i.e. no `RequestClusterState` event is emitted). This directly tests the re-trigger path that Flux's encryption secret update exploits.

#### Test 10 — Encryption secret update does not trigger a second snapshot (Bug 2, regression)

Simulates the exact trigger: create a GitTarget in `SnapshotSynced=True` state, then fire a `Secret` UPDATE event for its encryption secret. Assert the reconcile loop completes without calling `StartReconciliation`.

#### Test 11 — Adding a new WatchRule does trigger a re-snapshot (Bug 2, positive case)

Verifies that the fix for Bug 2 does not prevent legitimate re-snapshots. Add a second `WatchRule` referencing an already-Ready GitTarget. Assert that a `RequestClusterState` event is emitted by the `WatchRule` controller path (via `ReconcileForRuleChange`), not via `evaluateSnapshotGate`.

---

## Invariant for Future Tests

After this fix, the following invariant should hold and can be checked at the e2e level (already exercised by the bi-directional test once the fix is in):

> After Flux applies resources from a gitops-reverser commit back to the cluster, the commit count in the repository **must not increase** — the controller must detect that the cluster state already matches Git and produce no new commit.
