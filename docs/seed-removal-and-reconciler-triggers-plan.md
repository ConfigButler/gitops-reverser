# Seed Removal and Reconciler Trigger Enhancement Plan

**Date**: 2025-10-30  
**Status**: Planning Phase  
**Related**: [`sync-architecture-completion-plan-v4-final.md`](sync-architecture-completion-plan-v4-final.md)

## Overview

Remove the WatchManager seed mechanism (`seedSelectedResources()`) and make FolderReconciler responsible for ALL initial population and reconciliation. Additionally, add triggers so FolderReconciler re-reconciles when WatchManager's informer state changes.

## Current Architecture Problems

### Problem 1: FolderReconciler Emits Events Without Objects

**Current Flow** (Broken for CREATE):
```
FolderReconciler.reconcile()
  ├─ toCreate = resources in cluster but not in Git
  ├─ EmitCreateEvent(identifier)  
  │   └─ Event{Identifier: ✅, Object: ❌ nil}
  └─ Routes to GitDestinationEventStream → BranchWorker → Git

BranchWorker.generateLocalCommits()
  └─ sanitize.MarshalToOrderedYAML(event.Object)  // ❌ CRASHES - Object is nil!
```

**Current Workaround**: Seed function provides full objects via UPDATE events
```
WatchManager.seedSelectedResources()
  └─ Lists all resources → Creates UPDATE events with full objects → Git
```

### Problem 2: No Reconciliation Trigger on Informer Changes

**Current Behavior**:
- WatchManager adds/removes informers when rules change
- FolderReconciler never knows informers changed
- No re-reconciliation happens
- Result: Cluster state might be stale in reconciler

**What Should Happen**:
- Informer added → New resources might exist → Trigger reconciliation
- Informer removed → Resources might have been deleted → Trigger reconciliation

---

## Proposed Solution

### Architecture: Event-Driven Reconciliation with Object Fetching

```
┌─────────────────────────────────────────────────────────────────┐
│ WatchManager                                                    │
│  ├─ Informer Added/Removed                                      │
│  │   └─ Emit: InformerStateChanged(gitDests: []ResourceRef)     │
│  │                                                               │
│  └─ Live Watch Events                                           │
│      └─ Route: UPDATE events (with full objects) → EventStream  │
└─────────────────────────────────────────────────────────────────┘
                            ↓
┌─────────────────────────────────────────────────────────────────┐
│ EventRouter                                                     │
│  ├─ Route InformerStateChanged events → FolderReconcilers       │
│  └─ Route ControlEvents & StateEvents                           │
└─────────────────────────────────────────────────────────────────┘
                            ↓
┌─────────────────────────────────────────────────────────────────┐
│ FolderReconciler                                                │
│  ├─ On InformerStateChanged:                                    │
│  │   └─ StartReconciliation() (re-request cluster/git state)    │
│  │                                                               │
│  ├─ On StartReconciliation:                                     │
│  │   ├─ Request cluster state (identifiers)                     │
│  │   └─ Request git state (identifiers)                         │
│  │                                                               │
│  └─ On Both States Received:                                    │
│      ├─ toCreate: Fetch full objects → Emit enriched events     │
│      ├─ toDelete: Emit DELETE (no object needed)                │
│      └─ existingInBoth: Emit RECONCILE_RESOURCE trigger         │
└─────────────────────────────────────────────────────────────────┘
                            ↓
┌─────────────────────────────────────────────────────────────────┐
│ GitDestinationEventStream                                       │
│  ├─ Buffers events during reconciliation                        │
│  ├─ Deduplicates in live mode                                   │
│  └─ Forwards to BranchWorker with full objects                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Implementation Plan

### Phase 1: Add InformerStateChanged Event Type

**File**: `internal/events/events.go`

```go
// InformerStateChangedEvent notifies that WatchManager's informers changed.
// Affected GitDestinations should re-reconcile.
type InformerStateChangedEvent struct {
	// GitDestinations that may be affected by the informer changes
	AffectedGitDests []types.ResourceReference
	
	// Optional: details about what changed
	AddedGVRs   []string
	RemovedGVRs []string
}

// Add to ControlEventType
const (
	// ... existing ...
	InformerStateChanged ControlEventType = "INFORMER_STATE_CHANGED"
)
```

**Estimated**: 0.5 days

---

### Phase 2: WatchManager Emits InformerStateChanged

**File**: `internal/watch/manager.go`

**Modify `ReconcileForRuleChange()`** (line ~837):

```go
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	// ... existing code for starting/stopping informers ...
	
	// After informer changes, notify affected GitDestinations
	if len(added) > 0 || len(removed) > 0 {
		affectedGitDests := m.getAffectedGitDestinations(added, removed)
		
		for _, gitDest := range affectedGitDests {
			if err := m.EventRouter.EmitInformerStateChanged(gitDest, added, removed); err != nil {
				log.Error(err, "Failed to emit InformerStateChanged", "gitDest", gitDest.String())
			}
		}
	}
	
	// REMOVE: go m.seedSelectedResources(ctx)
	return nil
}

// getAffectedGitDestinations determines which GitDestinations are affected by GVR changes.
func (m *Manager) getAffectedGitDestinations(added, removed []GVR) []types.ResourceReference {
	gitDestSet := make(map[string]types.ResourceReference)
	
	// Check WatchRules and ClusterWatchRules to find affected destinations
	wrRules := m.RuleStore.SnapshotWatchRules()
	for _, rule := range wrRules {
		for _, rr := range rule.ResourceRules {
			for _, gvr := range append(added, removed...) {
				if m.compiledResourceRuleMatchesGVR(rr, gvr) {
					ref := types.NewResourceReference(rule.GitDestinationRef, rule.GitDestinationNamespace)
					gitDestSet[ref.Key()] = ref
				}
			}
		}
	}
	
	// ... similar for ClusterWatchRules ...
	
	result := make([]types.ResourceReference, 0, len(gitDestSet))
	for _, ref := range gitDestSet {
		result = append(result, ref)
	}
	return result
}
```

**Estimated**: 1 day

---

### Phase 3: EventRouter Routes InformerStateChanged Events

**File**: `internal/watch/event_router.go`

```go
// EmitInformerStateChanged emits an informer state change notification.
func (r *EventRouter) EmitInformerStateChanged(
	gitDest types.ResourceReference,
	addedGVRs, removedGVRs []GVR,
) error {
	// Get the reconciler
	reconciler, exists := r.ReconcilerManager.GetReconciler(gitDest)
	if !exists {
		r.Log.V(1).Info("No reconciler found for InformerStateChanged", "gitDest", gitDest.String())
		return nil
	}
	
	// Trigger re-reconciliation
	return reconciler.StartReconciliation(context.Background())
}
```

**Estimated**: 0.5 days

---

### Phase 4: Enhance FolderReconciler to Fetch Objects

**File**: `internal/reconcile/folder_reconciler.go`

**Add Client Dependency**:
```go
type FolderReconciler struct {
	gitDest types.ResourceReference
	
	clusterResources []types.ResourceIdentifier
	gitResources     []types.ResourceIdentifier
	
	eventEmitter   EventEmitter
	controlEmitter events.ControlEventEmitter
	client         client.Client  // NEW: for fetching objects
	logger         logr.Logger
}
```

**Enhance reconcile() to fetch objects**:
```go
func (r *FolderReconciler) reconcile() {
	if r.clusterResources == nil || r.gitResources == nil {
		return
	}

	toCreate, toDelete, existingInBoth := r.findDifferences(r.clusterResources, r.gitResources)

	// CREATE: Fetch objects from cluster and emit enriched events
	for _, resource := range toCreate {
		obj, err := r.fetchObjectFromCluster(context.Background(), resource)
		if err != nil {
			r.logger.Error(err, "Failed to fetch object for CREATE", "resource", resource.String())
			continue
		}
		
		if err := r.eventEmitter.EmitCreateEventWithObject(resource, obj); err != nil {
			r.logger.Error(err, "Failed to emit create event", "resource", resource.String())
		}
	}

	// DELETE: No object needed
	for _, resource := range toDelete {
		if err := r.eventEmitter.EmitDeleteEvent(resource); err != nil {
			r.logger.Error(err, "Failed to emit delete event", "resource", resource.String())
		}
	}

	// RECONCILE: Fetch object and emit
	for _, resource := range existingInBoth {
		obj, err := r.fetchObjectFromCluster(context.Background(), resource)
		if err != nil {
			r.logger.Error(err, "Failed to fetch object for reconcile", "resource", resource.String())
			continue
		}
		
		if err := r.eventEmitter.EmitReconcileEventWithObject(resource, obj); err != nil {
			r.logger.Error(err, "Failed to emit reconcile event", "resource", resource.String())
		}
	}
}

// fetchObjectFromCluster fetches the full unstructured object from cluster.
func (r *FolderReconciler) fetchObjectFromCluster(
	ctx context.Context,
	identifier types.ResourceIdentifier,
) (*unstructured.Unstructured, error) {
	// Build GVK from identifier
	gvk := schema.GroupVersionKind{
		Group:   identifier.Group,
		Version: identifier.Version,
		Kind:    identifier.KindFromResource(), // Need helper method
	}
	
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	
	// Fetch from cluster
	err := r.client.Get(ctx, client.ObjectKey{
		Name:      identifier.Name,
		Namespace: identifier.Namespace,
	}, obj)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", identifier.String(), err)
	}
	
	return obj, nil
}
```

**Estimated**: 1.5 days

---

### Phase 5: Update GitDestinationEventStream to Accept Objects

**File**: `internal/reconcile/git_destination_event_stream.go`

```go
// Update EventEmitter interface
type EventEmitter interface {
	EmitCreateEventWithObject(resource types.ResourceIdentifier, obj *unstructured.Unstructured) error
	EmitDeleteEvent(resource types.ResourceIdentifier) error
	EmitReconcileEventWithObject(resource types.ResourceIdentifier, obj *unstructured.Unstructured) error
}

// Implementation
func (s *GitDestinationEventStream) EmitCreateEventWithObject(
	resource types.ResourceIdentifier,
	obj *unstructured.Unstructured,
) error {
	event := git.Event{
		Operation:  "CREATE",
		Identifier: resource,
		Object:     obj,  // ✅ Now has object!
		BaseFolder: s.baseFolder,  // Need to store this
	}
	s.OnWatchEvent(event)
	return nil
}
```

**Estimated**: 1 day

---

### Phase 6: Add Kind Resolution Helper

**File**: `internal/types/identifier.go`

```go
// KindFromResource attempts to singularize the resource name to get Kind.
// This is a best-effort heuristic. More robust: query discovery for Kind.
func (r ResourceIdentifier) KindFromResource() string {
	// Simple heuristic: remove 's' from end
	resource := r.Resource
	if strings.HasSuffix(resource, "s") {
		return strings.TrimSuffix(resource, "s")
	}
	return resource
}

// Better alternative: Cache GVR->GVK mapping from discovery
// This would require RESTMapper or discovery client
```

**Estimated**: 0.5 days

---

### Phase 7: Remove Seed Function

**File**: `internal/watch/manager.go`

**Remove**:
- `seedSelectedResources()` (line ~356)
- `seedListAndProcess()` (line ~530)  
- `processListedObject()` (line ~579)
- `dynamicClientFromConfig()` (line ~382) - if only used by seed
- `discoverableGVRs()` (line ~397) - if only used by seed

**Update `ReconcileForRuleChange()`**:
```go
// Remove: go m.seedSelectedResources(ctx)
// Replaced by: InformerStateChanged events trigger reconcilers
```

**Estimated**: 0.5 days

---

### Phase 8: Wire InformerStateChanged to FolderReconciler

**File**: `internal/controller/gitdestination_controller.go`

Ensure FolderReconciler receives InformerStateChanged and re-triggers StartReconciliation.

**Estimated**: 0.5 days

---

## Benefits of This Refactor

| Aspect | Before | After | Improvement |
|--------|--------|-------|-------------|
| **Initial Population** | WatchManager seed (global) | FolderReconciler (per GitDest) | Scoped, on-demand |
| **Object Fetching** | Seed lists all resources | Reconciler fetches only needed ones | More efficient |
| **Reconciliation Triggers** | Manual (rule changes only) | Automatic (informer state changes) | Self-healing |
| **Code Complexity** | Dual mechanisms (seed + reconcile) | Single mechanism (reconcile) | Simpler |
| **Informer Awareness** | Reconciler unaware of informer state | Reconciler reacts to informer changes | Better coupling |

---

## Detailed Event Flow After Refactor

### Scenario 1: New GitDestination Created

```
1. GitDestination Controller
   └─ Creates FolderReconciler
   └─ Calls StartReconciliation()

2. FolderReconciler
   ├─ Emits: RequestClusterState(gitDest)
   └─ Emits: RequestRepoState(gitDest)

3. EventRouter
   ├─ Calls: WatchManager.GetClusterStateForGitDest()
   │   └─ Returns: []ResourceIdentifier (no objects yet)
   └─ Calls: BranchWorker.ListResourcesInBaseFolder()
       └─ Returns: []ResourceIdentifier (from Git files)

4. EventRouter Routes State Events
   ├─ RouteClusterStateEvent(identifiers) → FolderReconciler
   └─ RouteRepoStateEvent(identifiers) → FolderReconciler

5. FolderReconciler.reconcile()
   ├─ Computes: toCreate, toDelete, existingInBoth
   │
   ├─ For each toCreate:
   │   ├─ Fetch full object from cluster using client.Get()
   │   └─ EmitCreateEventWithObject(identifier, obj)
   │
   ├─ For each toDelete:
   │   └─ EmitDeleteEvent(identifier) // No object needed
   │
   └─ For each existingInBoth:
       ├─ Fetch full object from cluster
       └─ EmitReconcileEventWithObject(identifier, obj)

6. GitDestinationEventStream
   ├─ Receives events WITH full objects
   ├─ Buffers/deduplicates
   └─ Forwards to BranchWorker

7. BranchWorker
   └─ Writes YAML to Git (now has full objects!)
```

### Scenario 2: WatchRule Added (New Informer)

```
1. WatchRule Controller
   └─ Calls: WatchManager.ReconcileForRuleChange()

2. WatchManager
   ├─ Starts new informer for GVR
   ├─ Determines affected GitDestinations
   └─ Emits: InformerStateChanged(gitDests, addedGVRs)

3. EventRouter
   └─ Routes to each affected FolderReconciler

4. FolderReconciler
   └─ Calls: StartReconciliation() (re-fetch state)
       └─ Will detect new resources from new informer
```

### Scenario 3: Live Resource Updated

```
1. WatchManager Informer
   └─ Detects change → Routes UPDATE event (with full object) to EventStream

2. GitDestinationEventStream
   ├─ Deduplicates if seen before
   └─ Forwards to BranchWorker

3. BranchWorker
   └─ Writes to Git
```

---

## Critical Design Decisions

### Decision 1: Object Fetching Strategy

**Option A**: Fetch in FolderReconciler
- ✅ Cleaner separation (reconciler owns logic)
- ✅ Only fetches objects actually needed
- ❌ Adds client dependency to reconciler
- ❌ Multiple cluster API calls (one per resource)

**Option B**: Fetch in EventRouter before routing
- ✅ Reconciler stays pure (no client dependency)
- ❌ Fetches objects that might be deduplicated later
- ❌ EventRouter becomes stateful

**Recommendation**: **Option A** - FolderReconciler is already domain-aware, should own object fetching

---

### Decision 2: Kind Resolution

**Problem**: ResourceIdentifier has `Resource` (plural, e.g., "pods") but need `Kind` (singular, e.g., "Pod") for client.Get()

**Option A**: Simple heuristic (trim 's')
```go
func (r ResourceIdentifier) KindFromResource() string {
	return strings.TrimSuffix(r.Resource, "s")
}
```
- ✅ Fast, no dependencies
- ❌ Fails for irregular plurals (e.g., "Ingresses" → "Ingresse", should be "Ingress")

**Option B**: Use RESTMapper
```go
func (r *FolderReconciler) getKind(identifier types.ResourceIdentifier) (string, error) {
	gvr := schema.GroupVersionResource{
		Group:    identifier.Group,
		Version:  identifier.Version,
		Resource: identifier.Resource,
	}
	
	gvk, err := r.restMapper.KindFor(gvr)
	if err != nil {
		return "", err
	}
	return gvk.Kind, nil
}
```
- ✅ Correct for all resources
- ✅ Standard Kubernetes approach
- ❌ Requires RESTMapper dependency

**Recommendation**: **Option B** - Use RESTMapper for correctness

---

### Decision 3: When to Trigger Reconciliation

**Triggers**:
1. ✅ GitDestination created/updated → Controller calls StartReconciliation()
2. ✅ InformerStateChanged → EventRouter calls StartReconciliation()  
3. ❓ Periodic re-reconciliation? (e.g., every 5 minutes)

**Recommendation**: Start with (1) and (2), add (3) only if drift is observed

---

## Migration Path

### Step 1: Add Object Fetching (Keep Seed)
- Implement object fetching in FolderReconciler
- Update EventEmitter interfaces
- Keep seed running (parallel mode)
- Verify objects are fetched correctly

### Step 2: Add InformerStateChanged (Keep Seed)
- Add event type and routing
- Verify reconciliation triggers on informer changes
- Still parallel mode (both seed and reconciler work)

### Step 3: Remove Seed (Big Switch)
- Comment out `go m.seedSelectedResources(ctx)` call
- Run e2e tests
- If successful, delete seed implementation
- Monitor for issues

### Step 4: Cleanup
- Remove unused seed helper methods
- Update tests
- Update documentation

---

## Testing Strategy

### Unit Tests

1. **FolderReconciler Object Fetching**
   - Mock client.Get() calls
   - Verify objects are fetched for CREATE/RECONCILE
   - Verify errors are handled gracefully

2. **InformerStateChanged Event Flow**
   - Mock EventRouter
   - Verify reconciler receives event
   - Verify StartReconciliation() is called

### Integration Tests

1. **Initial Population Without Seed**
   - Create GitDestination
   - Verify cluster resources are written to Git
   - Confirm no seed function was called

2. **Informer Change Triggers Reconciliation**
   - Start with GitDestination and resources
   - Add new WatchRule (adds informer)
   - Verify new resources are detected and written

3. **Deduplication Still Works**
   - Initial reconciliation creates files
   - Live UPDATE event arrives for same resource
   - Verify not duplicated in Git

---

## Risks and Mitigations

### Risk 1: Performance - Many client.Get() Calls

**Impact**: Initial reconciliation might be slow with many resources

**Mitigation**:
- Use controller-runtime cache (reads are fast)
- Consider batching if needed
- Profile and optimize if bottleneck

### Risk 2: Informer Race Condition

**Impact**: Resource updated between GetClusterStateForGitDest() and fetchObject()

**Mitigation**:
- Accept eventual consistency (next reconciliation will fix)
- Live watch events will update Git anyway
- GitDestinationEventStream deduplication prevents issues

### Risk 3: Kind Resolution Failures

**Impact**: Can't fetch object if Kind is wrong

**Mitigation**:
- Use RESTMapper (standard, reliable)
- Add fallback to heuristic if RESTMapper unavailable
- Log warnings for failed resolutions

---

## Success Criteria

- [ ] FolderReconciler can fetch objects from cluster
- [ ] CREATE events include full objects
- [ ] RECONCILE events include full objects
- [ ] InformerStateChanged event type exists
- [ ] WatchManager emits InformerStateChanged on informer changes
- [ ] FolderReconciler re-reconciles on InformerStateChanged
- [ ] Seed function completely removed
- [ ] All tests pass without seed
- [ ] E2E tests verify initial population works
- [ ] Documentation updated

---

## Timeline Estimate

**Total**: 5.5 days

| Phase | Task | Days |
|-------|------|------|
| 1 | Add InformerStateChanged event | 0.5 |
| 2 | WatchManager emits events | 1.0 |
| 3 | EventRouter routes events | 0.5 |
| 4 | FolderReconciler object fetching | 1.5 |
| 5 | Update EventEmitter interfaces | 1.0 |
| 6 | Add Kind resolution | 0.5 |
| 7 | Remove seed function | 0.5 |
| 8 | Testing and cleanup | 1.0 |

---

## Open Questions

1. **RESTMapper**: Add as FolderReconciler dependency or create shared service?
2. **Batch Fetching**: Should we fetch multiple objects in one call for performance?
3. **Cache Strategy**: Use informer cache or direct client.Get()?
4. **Periodic Reconciliation**: Should we add timer-based reconciliation triggers?
5. **BaseFolder**: How does FolderReconciler know which baseFolder to use in events?

---

## Next Steps

1. Review this plan with team
2. Clarify open questions
3. Create feature branch: `feature/remove-seed-enhance-reconciler`
4. Implement phases incrementally with tests
5. Monitor behavior in development environment