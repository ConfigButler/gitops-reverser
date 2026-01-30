# GitOps Reverser Design Status

**Last Updated:** 2025-10-20  
**Project Phase:** MVP Implementation in Progress

---

## Document Hierarchy

```
DESIGN_STATUS.md (you are here) ← Start here for current state
│
├── cluster-source-of-truth-spec.md ← Authoritative specification (what we're building)
├── cluster-source-of-truth-plan.md ← Implementation tracking and detailed plan
│
├── IMPLEMENTATION_ROADMAP.md ← Current focus: Dynamic watch manager for E2E
└── DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md ← Detailed design (being implemented)
```

---

## Current State Summary

### ✅ What's Implemented & Working

**Core Cluster-as-Source-of-Truth:**
- List+Watch ingestion via [`internal/watch/manager.go`](../internal/watch/manager.go)
- ValidatingWebhook for username capture at `/process-validating-webhook`
- Dual-signal correlation (webhook→watch enrichment) via KV store
- GitDestination CRD with branch + baseFolder support
- WatchRule/ClusterWatchRule controllers with validation
- Deduplication to prevent status-only change commits
- Git operations: batching, orphan detection, conflict resolution
- Fixed 1 MiB batching, uncapped orphan deletion per spec

**What Works Today:**
- Creating resources → Committed to Git via webhook + watch
- Deleting resources → Removed from Git immediately
- Status-only changes → Filtered out (no commits)
- Multiple rules → Proper destination routing

### ⚠️ Critical Gap: Dynamic Informer Management

**Problem:** Watch Manager only starts informers at pod startup.

**Impact on E2E Tests:**
```
❌ Create WatchRule → Resources NOT watched until pod restart
❌ Delete WatchRule → Orphan files remain in Git
❌ Install CRD → New resource type NOT detected
```

**Solution:** Implement controller→WatchManager integration (see IMPLEMENTATION_ROADMAP.md)

---

## MVP Design Decisions

### Simplifications for Alpha Release

✅ **Single Pod Operation**
- No distributed locking needed
- Leader election ensures one active pod
- Documentation will note multi-pod requirements for future

✅ **No Migration/Compat Concerns**
- Alpha posture: surfaces can break
- Prioritize clean design over backwards compatibility

✅ **ValidatingWebhook Retained Permanently**
- Provides reliable username capture
- FailurePolicy=Ignore for graceful degradation
- Leader-only routing prevents duplicate processing

✅ **Kubernetes Reconciliation as Base**
- Cluster decides (not Git)
- Watch events are source of truth
- Git is output, not input

### Out of Scope (Future Work)

- ❌ Multi-repo ownership/locking
- ❌ Git → Cluster sync (reverse direction)
- ❌ AccessPolicy authorization
- ❌ Object/Namespace selectors on rules
- ❌ Migration tooling

---

## Implementation Status

| Component | Status | Notes |
|-----------|--------|-------|
| **Core Architecture** | ✅ Complete | Spec-aligned |
| WatchRule/ClusterWatchRule CRDs | ✅ Complete | Validation working |
| GitDestination CRD | ✅ Complete | Branch + baseFolder |
| Controllers | ⚠️ Partial | Validate but don't trigger WatchManager |
| Watch Manager | ⚠️ Partial | Starts at pod startup only |
| Dynamic Informers | ❌ Missing | **Blocking E2E tests** |
| Correlation Store | ✅ Complete | 91.6% test coverage |
| Git Worker | ✅ Complete | Batching, orphans, conflicts handled |
| Deduplication | ✅ Complete | Content hash-based |
| Metrics | ✅ Complete | OTEL exported |

---

## Next Steps (Priority Order)

### 1. ⚠️ CRITICAL: Make E2E Tests Pass

**See:** [`IMPLEMENTATION_ROADMAP.md`](IMPLEMENTATION_ROADMAP.md)

**Tasks:**
- [ ] Add WatchManager reference to controllers
- [ ] Implement `ReconcileForRuleChange()` in WatchManager
- [ ] Add dynamic informer start/stop logic
- [ ] Wire in `cmd/main.go`
- [ ] Verify `make test-e2e` passes

**Estimated:** 6 hours

### 2. Documentation Updates

- [ ] Update DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md status
- [ ] Update cluster-source-of-truth-plan.md with implementation progress
- [ ] Document MVP limitations clearly (single pod, etc.)

### 3. Future Enhancements (Post-MVP)

- Periodic discovery refresh for CRD detection (<30s latency)
- Deduplication cache clearing on informer changes
- Status conditions from WatchManager to CRDs
- Multi-pod coordination design

---

## E2E Test Requirements

Tests expect:

1. **ConfigMap CRUD:**
   - Create ConfigMap → File appears in Git
   - Update ConfigMap → File updated in Git
   - Delete ConfigMap → File removed from Git

2. **WatchRule Lifecycle:**
   - Create WatchRule → Start watching resources
   - Delete WatchRule → Clean up orphaned Git files

3. **Custom Resources:**
   - IceCreamOrder CRD instances → Committed to Git
   - Labels/annotations preserved, status filtered
   - Updates reflected, deletions remove files

4. **Cluster Resources:**
   - ClusterWatchRule for CRDs
   - CRD installation → Committed to Git
   - CRD deletion → File removed

**Current Failure Mode:** Tests timeout waiting for Git commits after rule changes because informers don't start dynamically.

---

## MVP Constraints (User Documentation)

**For Initial Tests:**

⚠️ **Single Active Pod Requirement**
```yaml
# Deployment must have:
replicas: 1  # Only one pod writes to Git
```

⚠️ **No Concurrent Rule Changes**
- Allow 30 seconds between WatchRule create/delete operations
- Controller needs time to reconcile and update informers

⚠️ **CRD Detection Latency**
- New CRDs detected within 30 seconds (if periodic refresh enabled)
- Or restart controller pod for immediate detection

⚠️ **No Git Conflicts Expected**
- Single pod = no write conflicts
- Multiple repos/branches supported safely

**These constraints will be documented in README.md and CONTRIBUTING.md**

---

## Questions for Decision

1. **Periodic Discovery Interval:** 30s (fast) vs 5min (conservative)?
   - Recommendation: Start with 30s for MVP, make configurable

2. **E2E Test Approach:** Fix implementation vs adjust tests?
   - Decision: Fix implementation (tests validate real-world usage)

3. **Status Reporting:** Add WatchManager→Controller status updates now or later?
   - Recommendation: Later (not blocking E2E)

---

## References

- **Specification:** [`cluster-source-of-truth-spec.md`](cluster-source-of-truth-spec.md)
- **Implementation Plan:** [`cluster-source-of-truth-plan.md`](cluster-source-of-truth-plan.md)
- **Current Focus:** [`IMPLEMENTATION_ROADMAP.md`](IMPLEMENTATION_ROADMAP.md)
- **Detailed Design:** [`DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md`](DYNAMIC_WATCH_MANAGER_REVISED_PLAN.md)
- **E2E Tests:** [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go)
- **Implementation Rules:** [`.kilocode/rules/implementation-rules.md`](../.kilocode/rules/implementation-rules.md)