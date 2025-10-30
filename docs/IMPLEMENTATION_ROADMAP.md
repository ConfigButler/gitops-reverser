# Implementation Roadmap: Dynamic Watch Manager for E2E Tests

## Status: IMPLEMENTATION REQUIRED

This document outlines the **minimum implementation** needed to pass e2e tests. Based on DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md but streamlined for single-pod MVP.

---

## Current Problem

E2E tests fail because:
1. Creating WatchRule doesn't start watching resources immediately
2. Deleting WatchRule doesn't stop watching or clean up Git files
3. Installing CRDs doesn't trigger watching of new resource types

**Root Cause:** Watch Manager only starts informers at pod startup, never updates them dynamically.

---

## Minimal Solution (Single Pod, No Locking)

### Phase 1: Controller → WatchManager Integration ⚠️ CRITICAL

**Goal:** Make controllers trigger WatchManager reconciliation on rule changes.

#### Changes Needed:

**1. Add WatchManager reference to controllers:**

```go
// internal/controller/watchrule_controller.go
type WatchRuleReconciler struct {
    client.Client
    Scheme       *runtime.Scheme
    RuleStore    *rulestore.RuleStore
    WatchManager *watch.Manager  // NEW
}
```

**2. Call WatchManager after RuleStore update:**

```go
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing validation ...
    
    // Add/update rule in store
    r.RuleStore.AddOrUpdateWatchRuleResolved(...)
    
    // NEW: Trigger WatchManager reconciliation
    if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
        log.Error(err, "Failed to reconcile watch manager")
        // Don't fail the entire reconciliation, just log
    }
    
    // ... status update ...
}
```

**3. Handle deletions:**

```go
if apierrors.IsNotFound(err) {
    // Rule deleted
    r.RuleStore.Delete(req.NamespacedName)
    
    // NEW: Trigger reconciliation for deletion
    if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
        log.Error(err, "Failed to reconcile after deletion")
    }
    return ctrl.Result{}, nil
}
```

**Same changes for ClusterWatchRuleReconciler.**

---

### Phase 2: WatchManager Reconciliation Logic

**Goal:** Update informers when rules change and trigger re-seed.

#### Add to internal/watch/manager.go:

```go
type Manager struct {
    // ... existing fields ...
    
    // NEW: Track active informers
    informersMu      sync.Mutex
    activeInformers  map[GVR]context.CancelFunc
    informerFactory  dynamicinformer.DynamicSharedInformerFactory
}

// ReconcileForRuleChange is called by controllers when rules change.
// Single-pod MVP: No debouncing needed since we control pod restarts.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
    log := m.Log.WithName("reconcile")
    log.Info("Reconciling watch manager for rule change")
    
    // Compute desired GVRs from current rules
    requestedGVRs := m.ComputeRequestedGVRs()
    discoverableGVRs := m.FilterDiscoverableGVRs(ctx, requestedGVRs)
    
    // Determine what changed
    added, removed := m.compareGVRs(discoverableGVRs)
    
    if len(added) == 0 && len(removed) == 0 {
        log.Info("No GVR changes detected")
        return nil
    }
    
    log.Info("GVR changes detected", "added", len(added), "removed", len(removed))
    
    // Stop informers for removed GVRs
    for _, gvr := range removed {
        m.stopInformer(gvr)
    }
    
    // Start informers for added GVRs
    for _, gvr := range added {
        m.startInformer(ctx, gvr)
    }
    
    // Trigger re-seed to sync Git with new state
    // Run in background to avoid blocking controller
    go m.seedSelectedResources(ctx)
    
    return nil
}

// compareGVRs returns (added, removed) GVRs compared to current active set.
func (m *Manager) compareGVRs(desired []GVR) (added, removed []GVR) {
    m.informersMu.Lock()
    defer m.informersMu.Unlock()
    
    desiredSet := make(map[GVR]bool)
    for _, gvr := range desired {
        desiredSet[gvr] = true
        if _, exists := m.activeInformers[gvr]; !exists {
            added = append(added, gvr)
        }
    }
    
    for gvr := range m.activeInformers {
        if !desiredSet[gvr] {
            removed = append(removed, gvr)
        }
    }
    
    return added, removed
}

// stopInformer cancels and removes an informer for a specific GVR.
func (m *Manager) stopInformer(gvr GVR) {
    m.informersMu.Lock()
    defer m.informersMu.Unlock()
    
    if cancel, exists := m.activeInformers[gvr]; exists {
        cancel() // Stop the informer
        delete(m.activeInformers, gvr)
        m.Log.Info("Stopped informer", "gvr", gvr)
    }
}

// startInformer starts watching a specific GVR.
func (m *Manager) startInformer(ctx context.Context, gvr GVR) {
    m.informersMu.Lock()
    defer m.informersMu.Unlock()
    
    if _, exists := m.activeInformers[gvr]; exists {
        return // Already running
    }
    
    // Create cancellable context for this informer
    informerCtx, cancel := context.WithCancel(ctx)
    
    // Start informer (reuse existing startDynamicInformers logic)
    // TODO: Extract single-GVR informer start logic from maybeStartInformers
    
    m.activeInformers[gvr] = cancel
    m.Log.Info("Started informer", "gvr", gvr)
}
```

---

### Phase 3: Wire in cmd/main.go

```go
// cmd/main.go
func main() {
    // ... existing setup ...
    
    // Create watch manager
    watchManager := &watch.Manager{
        Client:           mgr.GetClient(),
        Log:              ctrl.Log.WithName("watch"),
        RuleStore:        ruleStore,
        EventQueue:       eventQueue,
        CorrelationStore: correlationStore,
    }
    
    // Setup controllers with watch manager reference
    if err = (&controller.WatchRuleReconciler{
        Client:       mgr.GetClient(),
        Scheme:       mgr.GetScheme(),
        RuleStore:    ruleStore,
        WatchManager: watchManager,  // NEW
    }).SetupWithManager(mgr); err != nil {
        // ...
    }
    
    if err = (&controller.ClusterWatchRuleReconciler{
        Client:       mgr.GetClient(),
        Scheme:       mgr.GetScheme(),
        RuleStore:    ruleStore,
        WatchManager: watchManager,  // NEW
    }).SetupWithManager(mgr); err != nil {
        // ...
    }
    
    // Start watch manager
    if err = mgr.Add(watchManager); err != nil {
        // ...
    }
}
```

---

### Phase 4: Optional CRD Watcher (If E2E Test Requires)

**Note:** Check if e2e test for CRD installation actually requires dynamic detection or if controller restart is acceptable.

If needed, add periodic discovery refresh (simpler than CRD watcher):

```go
func (m *Manager) Start(ctx context.Context) error {
    // ... existing startup ...
    
    // Periodic reconciliation (every 30 seconds for MVP)
    periodicTicker := time.NewTicker(30 * time.Second)
    defer periodicTicker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-periodicTicker.C:
            log.V(1).Info("Periodic reconciliation check")
            if err := m.ReconcileForRuleChange(ctx); err != nil {
                log.Error(err, "Periodic reconciliation failed")
            }
        }
    }
}
```

---

## Testing Strategy

### Unit Tests

```go
// internal/watch/manager_test.go

func TestManager_ReconcileForRuleChange_AddsInformer(t *testing.T) {
    // Setup: Manager with no active informers
    // Add rule to RuleStore
    // Call ReconcileForRuleChange
    // Assert: Informer started for new GVR
}

func TestManager_ReconcileForRuleChange_RemovesInformer(t *testing.T) {
    // Setup: Manager with active informer
    // Remove rule from RuleStore
    // Call ReconcileForRuleChange
    // Assert: Informer stopped
}
```

### E2E Verification

Run existing e2e tests - they should now pass:
- `make test-e2e`

Key tests:
- ConfigMap creation → Git commit
- ConfigMap deletion → Git file removed
- WatchRule deletion → Orphan files cleaned up
- CRD installation → CRD committed (if periodic refresh enabled)

---

## MVP Constraints (Documented)

✅ **Single Pod Operation:** No distributed locking needed  
✅ **Controller Restart Safe:** Re-computes GVRs on startup  
✅ **Simple Reconciliation:** No debouncing, no complex retry logic  
✅ **No Status Reporting:** Controllers don't update status based on WatchManager results

**Future Work (Not MVP):**
- [ ] Multi-pod coordination with leader election
- [ ] Deduplication cache clearing on re-seed
- [ ] Smart re-seed (only affected GVRs)
- [ ] CRD watcher for immediate detection (<30s latency)
- [ ] Status conditions from WatchManager to CRDs

---

## Estimated Effort

| Phase | Description | Hours |
|-------|-------------|-------|
| 1 | Controller integration | 1.5 |
| 2 | WatchManager reconciliation | 2.0 |
| 3 | Wiring in main.go | 0.5 |
| 4 | Periodic refresh (optional) | 0.5 |
| Testing | Unit + E2E verification | 1.5 |
| **Total** | | **6 hours** |

---

## Success Criteria

✅ `make lint` passes  
✅ `make test` passes with new unit tests  
✅ `make test-e2e` passes (all scenarios)  
✅ Creating WatchRule → Resources appear in Git within 30s  
✅ Deleting WatchRule → Orphan files removed within 30s  
✅ Pod restart → No duplicate commits

---

## Next Steps

1. Implement Phase 1 (controller integration)
2. Implement Phase 2 (reconciliation logic)
3. Wire in main.go (Phase 3)
4. Run `make test-e2e` to verify
5. Add periodic refresh if CRD test fails (Phase 4)
6. Update documentation

---

## References

- Full design: [DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md](DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md)
- Spec: [cluster-source-of-truth-spec.md](cluster-source-of-truth-spec.md)
- E2E tests: [test/e2e/e2e_test.go](../test/e2e/e2e_test.go)