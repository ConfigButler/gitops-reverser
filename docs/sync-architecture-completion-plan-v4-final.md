# Sync Architecture Completion Plan v4 (FINAL - Corrected)

## Addendum: Rename to FolderReconciler ✅ DONE

**Date**: 2025-10-30  
**Status**: Rename completed via IDE refactor (F2)

### Files Updated
- ✅ `internal/reconcile/base_folder_reconciler.go` - Type renamed to `FolderReconciler`
- ✅ `internal/reconcile/reconciler_manager.go` - Updated all references
- ✅ `internal/reconcile/base_folder_reconciler_test.go` - Updated test cases

### Why FolderReconciler?
1. **Avoids conflict** - `GitDestinationReconciler` already exists in controller
2. **Describes purpose** - Reconciles folder contents (file identifiers), not file contents
3. **Clearer separation** - Controller manages CRD lifecycle, FolderReconciler manages folder state

### Remaining Work
The rename is complete, but the structure still uses the OLD design:
- ❌ Still stores `repoName`, `branch`, `baseFolder` separately
- ❌ Events still have `RepoName`, `Branch`, `BaseFolder` fields
- ❌ **ReconcilerKey still exists** → **WILL BE REMOVED**
- ❌ No reusable type for GitDestination references

---

## Critical Design Corrections

### GitDestination = Repo + Branch + ONE BaseFolder

From the CRD (`config/crd/bases/configbutler.ai_gitdestinations.yaml` line 62-70):
- `baseFolder` is a **single string field**, not an array
- GitDestination uniqueness: `(resolved_repo_url, branch, baseFolder)`

This means:
- ✅ One GitDestination = One baseFolder = One reconciler
- ✅ GitDestination reference alone is sufficient!
- ✅ No need to pass repo/branch/baseFolder separately

### Use ResourceReference Type (Clean Pattern)

Instead of passing separate name/namespace fields, use a clean reference type:

```go
type ResourceReference struct {
    Name      string
    Namespace string
}
```

Benefits:
- Reusable across codebase
- Type-safe
- Clean API
- Follows Kubernetes conventions

---

## New Reusable Type: ResourceReference

### Create internal/types/reference.go (NEW FILE)

```go
/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
*/

package types

import "fmt"

// ResourceReference references a Kubernetes resource by name and namespace.
// Provides a clean, reusable type for referencing GitDestinations and other resources.
type ResourceReference struct {
    Name      string
    Namespace string
}

// NewResourceReference creates a new resource reference.
func NewResourceReference(name, namespace string) ResourceReference {
    return ResourceReference{
        Name:      name,
        Namespace: namespace,
    }
}

// String returns "namespace/name" format.
func (r ResourceReference) String() string {
    return fmt.Sprintf("%s/%s", r.Namespace, r.Name)
}

// Key returns a string key suitable for map lookups.
func (r ResourceReference) Key() string {
    return r.String()
}

// Equal checks if two references are equal.
func (r ResourceReference) Equal(other ResourceReference) bool {
    return r.Name == other.Name && r.Namespace == other.Namespace
}

// IsZero returns true if this is an empty reference.
func (r ResourceReference) IsZero() bool {
    return r.Name == "" && r.Namespace == ""
}
```

---

## Simplified Event Structures

### Control Events
```go
// In internal/events/events.go

type ControlEvent struct {
    Type    ControlEventType
    GitDest types.ResourceReference  // Clean GitDestination reference!
    
    // Optional resource context for ReconcileResource events
    Resource *types.ResourceIdentifier
}

// ControlEventEmitter emits control events
type ControlEventEmitter interface {
    EmitControlEvent(event ControlEvent) error
}
```

### State Events  
```go
type ClusterStateEvent struct {
    GitDest   types.ResourceReference
    Resources []types.ResourceIdentifier
}

type RepoStateEvent struct {
    GitDest   types.ResourceReference
    Resources []types.ResourceIdentifier
}
```

---

## Updated Component Implementations

### FolderReconciler

```go
// In internal/reconcile/base_folder_reconciler.go

type FolderReconciler struct {
    gitDest types.ResourceReference  // Just the GitDest reference!
    
    clusterResources []types.ResourceIdentifier
    gitResources     []types.ResourceIdentifier
    eventEmitter     EventEmitter
    controlEmitter   ControlEventEmitter
    logger           logr.Logger
}

func NewFolderReconciler(
    gitDest types.ResourceReference,
    eventEmitter EventEmitter,
    controlEmitter ControlEventEmitter,
    logger logr.Logger,
) *FolderReconciler {
    return &FolderReconciler{
        gitDest:        gitDest,
        eventEmitter:   eventEmitter,
        controlEmitter: controlEmitter,
        logger:         logger.WithValues("gitDest", gitDest.String()),
    }
}

func (r *FolderReconciler) StartReconciliation(ctx context.Context) error {
    r.logger.Info("Starting reconciliation")
    
    // Emit control events - just pass GitDest!
    if err := r.controlEmitter.EmitControlEvent(events.ControlEvent{
        Type:    events.RequestClusterState,
        GitDest: r.gitDest,
    }); err != nil {
        return err
    }
    
    if err := r.controlEmitter.EmitControlEvent(events.ControlEvent{
        Type:    events.RequestRepoState,
        GitDest: r.gitDest,
    }); err != nil {
        return err
    }
    
    return nil
}

func (r *FolderReconciler) OnClusterState(event events.ClusterStateEvent) {
    if !event.GitDest.Equal(r.gitDest) {
        return
    }
    r.clusterResources = event.Resources
    r.logger.V(1).Info("Received cluster state", "resourceCount", len(event.Resources))
    r.reconcile()
}

func (r *FolderReconciler) OnRepoState(event events.RepoStateEvent) {
    if !event.GitDest.Equal(r.gitDest) {
        return
    }
    r.gitResources = event.Resources
    r.logger.V(1).Info("Received repo state", "resourceCount", len(event.Resources))
    r.reconcile()
}

func (r *FolderReconciler) GetGitDest() types.ResourceReference {
    return r.gitDest
}

func (r *FolderReconciler) String() string {
    return fmt.Sprintf("FolderReconciler(gitDest=%s)", r.gitDest.String())
}
```

### ReconcilerManager (ReconcilerKey REMOVED!)

```go
// In internal/reconcile/reconciler_manager.go

// ReconcilerKey REMOVED - Using ResourceReference.Key() for map keys!

type ReconcilerManager struct {
    reconcilers map[string]*FolderReconciler  // key = gitDest.Key() = "namespace/name"
    eventRouter interface {
        ProcessControlEvent(ctx context.Context, event events.ControlEvent) error
    }
    logger logr.Logger
}

func NewReconcilerManager(
    eventRouter interface {
        ProcessControlEvent(ctx context.Context, event events.ControlEvent) error
    },
    logger logr.Logger,
) *ReconcilerManager {
    return &ReconcilerManager{
        reconcilers: make(map[string]*FolderReconciler),
        eventRouter: eventRouter,
        logger:      logger,
    }
}

func (m *ReconcilerManager) CreateReconciler(
    gitDest types.ResourceReference,
    eventEmitter EventEmitter,
) *FolderReconciler {
    key := gitDest.Key()
    
    if reconciler, exists := m.reconcilers[key]; exists {
        m.logger.V(1).Info("Reconciler already exists", "gitDest", gitDest.String())
        return reconciler
    }
    
    reconciler := NewFolderReconciler(gitDest, eventEmitter, m, m.logger)
    m.reconcilers[key] = reconciler
    m.logger.Info("Created new FolderReconciler", "gitDest", gitDest.String())
    return reconciler
}

func (m *ReconcilerManager) GetReconciler(gitDest types.ResourceReference) (*FolderReconciler, bool) {
    reconciler, exists := m.reconcilers[gitDest.Key()]
    return reconciler, exists
}

func (m *ReconcilerManager) DeleteReconciler(gitDest types.ResourceReference) bool {
    key := gitDest.Key()
    if _, exists := m.reconcilers[key]; !exists {
        m.logger.V(1).Info("Reconciler not found", "gitDest", gitDest.String())
        return false
    }
    delete(m.reconcilers, key)
    m.logger.Info("Deleted FolderReconciler", "gitDest", gitDest.String())
    return true
}

func (m *ReconcilerManager) EmitControlEvent(event events.ControlEvent) error {
    if m.eventRouter == nil {
        return fmt.Errorf("eventRouter not set")
    }
    return m.eventRouter.ProcessControlEvent(context.Background(), event)
}
```

### EventRouter (Orchestrator)

```go
// In internal/watch/event_router.go

type EventRouter struct {
    WorkerManager     *git.WorkerManager
    ReconcilerManager *reconcile.ReconcilerManager
    WatchManager      *Manager
    Client            client.Client
    Log               logr.Logger
    gitDestStreams    map[string]*reconcile.GitDestinationEventStream
}

func (r *EventRouter) ProcessControlEvent(ctx context.Context, event events.ControlEvent) error {
    r.Log.V(1).Info("Processing control event", "type", event.Type, "gitDest", event.GitDest.String())
    
    switch event.Type {
    case events.RequestClusterState:
        return r.handleRequestClusterState(ctx, event)
    case events.RequestRepoState:
        return r.handleRequestRepoState(ctx, event)
    default:
        return fmt.Errorf("unknown control event type: %s", event.Type)
    }
}

func (r *EventRouter) handleRequestClusterState(ctx context.Context, event events.ControlEvent) error {
    // Call WatchManager service (synchronous)
    resources, err := r.WatchManager.GetClusterStateForGitDest(ctx, event.GitDest)
    if err != nil {
        return fmt.Errorf("failed to get cluster state: %w", err)
    }
    
    // Wrap in event and route
    return r.RouteClusterStateEvent(events.ClusterStateEvent{
        GitDest:   event.GitDest,
        Resources: resources,
    })
}

func (r *EventRouter) handleRequestRepoState(ctx context.Context, event events.ControlEvent) error {
    // Look up GitDestination
    var gitDest configv1alpha1.GitDestination
    if err := r.Client.Get(ctx, client.ObjectKey{
        Name:      event.GitDest.Name,
        Namespace: event.GitDest.Namespace,
    }, &gitDest); err != nil {
        return fmt.Errorf("failed to get GitDestination: %w", err)
    }
    
    // Get BranchWorker
    worker, exists := r.WorkerManager.GetWorkerForDestination(
        gitDest.Spec.RepoRef.Name,
        gitDest.Spec.RepoRef.Namespace,
        gitDest.Spec.Branch,
    )
    if !exists {
        return fmt.Errorf("no worker for %s", event.GitDest.String())
    }
    
    // Call BranchWorker service (synchronous)
    resources, err := worker.ListResourcesInBaseFolder(gitDest.Spec.BaseFolder)
    if err != nil {
        return fmt.Errorf("failed to list resources: %w", err)
    }
    
    // Wrap in event and route
    return r.RouteRepoStateEvent(events.RepoStateEvent{
        GitDest:   event.GitDest,
        Resources: resources,
    })
}

func (r *EventRouter) RouteRepoStateEvent(event events.RepoStateEvent) error {
    reconciler, exists := r.ReconcilerManager.GetReconciler(event.GitDest)
    if !exists {
        r.Log.V(1).Info("No reconciler found", "gitDest", event.GitDest.String())
        return nil
    }
    reconciler.OnRepoState(event)
    return nil
}

func (r *EventRouter) RouteClusterStateEvent(event events.ClusterStateEvent) error {
    reconciler, exists := r.ReconcilerManager.GetReconciler(event.GitDest)
    if !exists {
        r.Log.V(1).Info("No reconciler found", "gitDest", event.GitDest.String())
        return nil
    }
    reconciler.OnClusterState(event)
    return nil
}
```

### BranchWorker (Pure Git Service)

```go
// In internal/git/branch_worker.go

type BranchWorker struct {
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    Client                 client.Client
    Log                    logr.Logger
    // ... other fields ...
    
    // NO EventRouter - BranchWorker doesn't know about events!
}

// ListResourcesInBaseFolder returns resource identifiers found in a Git folder
// This is a SYNCHRONOUS service method - no event emission!
func (w *BranchWorker) ListResourcesInBaseFolder(baseFolder string) ([]types.ResourceIdentifier, error) {
    repoConfig, err := w.getGitRepoConfig(w.ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to get GitRepoConfig: %w", err)
    }
    
    auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
    if err != nil {
        return nil, fmt.Errorf("failed to get auth: %w", err)
    }
    
    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)
    
    repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
    if err != nil {
        return nil, fmt.Errorf("failed to clone repository: %w", err)
    }
    
    if err := repo.Checkout(w.Branch); err != nil {
        return nil, fmt.Errorf("failed to checkout branch: %w", err)
    }
    
    return w.listResourceIdentifiersInBaseFolder(repoPath, baseFolder)
}

func (w *BranchWorker) listResourceIdentifiersInBaseFolder(
    repoPath, baseFolder string,
) ([]types.ResourceIdentifier, error) {
    var resources []types.ResourceIdentifier
    
    basePath := repoPath
    if baseFolder != "" {
        basePath = filepath.Join(repoPath, baseFolder)
    }
    
    err := filepath.Walk(basePath, func(path string, info os.FileInfo, walkErr error) error {
        if walkErr != nil {
            return walkErr
        }
        if info.IsDir() {
            return nil
        }
        
        relPath, relErr := filepath.Rel(repoPath, path)
        if relErr != nil {
            return relErr
        }
        
        // Skip marker files
        if strings.Contains(relPath, ".configbutler") {
            return nil
        }
        
        // Process YAML files
        ext := filepath.Ext(relPath)
        if strings.EqualFold(ext, ".yaml") || strings.EqualFold(ext, ".yml") {
            if id, ok := parseIdentifierFromPath(relPath); ok {
                resources = append(resources, id)
            }
        }
        
        return nil
    })
    
    if err != nil && !os.IsNotExist(err) {
        return nil, err
    }
    
    return resources, nil
}

// REMOVE EmitRepoState() - no longer needed
```

### WatchManager (Pure Service)

```go
// In internal/watch/manager.go

// GetClusterStateForGitDest returns cluster resources for a GitDestination
// SYNCHRONOUS service method - no event handling!
func (m *Manager) GetClusterStateForGitDest(
    ctx context.Context,
    gitDest types.ResourceReference,
) ([]types.ResourceIdentifier, error) {
    log := m.Log.WithValues("gitDest", gitDest.String())
    
    // Look up GitDestination to get baseFolder
    var gitDestObj configv1alpha1.GitDestination
    if err := m.Client.Get(ctx, client.ObjectKey{
        Name:      gitDest.Name,
        Namespace: gitDest.Namespace,
    }, &gitDestObj); err != nil {
        return nil, fmt.Errorf("failed to get GitDestination: %w", err)
    }
    
    baseFolder := gitDestObj.Spec.BaseFolder
    log = log.WithValues("baseFolder", baseFolder)
    
    // Get matching rules
    wrRules := m.RuleStore.SnapshotWatchRules()
    cwrRules := m.RuleStore.SnapshotClusterWatchRules()
    
    // Build GVR set
    gvrSet := make(map[GVR]struct{})
    
    for _, rule := range wrRules {
        if rule.GitDestinationRef == gitDestObj.Name &&
            rule.GitDestinationNamespace == gitDestObj.Namespace {
            for _, rr := range rule.ResourceRules {
                m.addGVRsFromResourceRule(rr, gvrSet)
            }
        }
    }
    
    for _, cwrRule := range cwrRules {
        if cwrRule.GitDestinationRef == gitDestObj.Name &&
            cwrRule.GitDestinationNamespace == gitDestObj.Namespace {
            for _, rr := range cwrRule.Rules {
                m.addGVRsFromClusterResourceRule(rr, gvrSet)
            }
        }
    }
    
    // Query cluster
    dc := m.dynamicClientFromConfig(m.Log)
    if dc == nil {
        return nil, fmt.Errorf("no dynamic client")
    }
    
    var resources []types.ResourceIdentifier
    for gvr := range gvrSet {
        gvrResources, err := m.listResourcesForGVR(ctx, dc, gvr, &gitDestObj)
        if err != nil {
            log.Error(err, "Failed to list GVR", "gvr", gvr)
            continue
        }
        resources = append(resources, gvrResources...)
    }
    
    log.Info("Retrieved cluster state", "resourceCount", len(resources))
    return resources, nil
}
```

---

## Implementation Phases

### ✅ Phase 0: Rename to FolderReconciler (DONE)
- Already completed via IDE refactor

### Phase 1: Create ResourceReference Type
**Files**: `internal/types/reference.go` (new), `internal/types/reference_test.go` (new)

**Tasks**:
- [ ] Create `ResourceReference` struct
- [ ] Add methods: `String()`, `Key()`, `Equal()`, `IsZero()`
- [ ] Add comprehensive unit tests
- [ ] Verify tests pass

**Estimated**: 0.5 days

### Phase 2: Update Event Structures
**File**: `internal/events/events.go`

**Tasks**:
- [ ] Update `ControlEvent` to use `GitDest types.ResourceReference`
- [ ] Update `ClusterStateEvent` to use `GitDest types.ResourceReference`
- [ ] Update `RepoStateEvent` to use `GitDest types.ResourceReference`
- [ ] Add `ControlEventEmitter` interface
- [ ] Update all event-related tests

**Estimated**: 0.5 days

### Phase 3: Update FolderReconciler
**File**: `internal/reconcile/base_folder_reconciler.go`

**Tasks**:
- [ ] Replace `repoName`, `branch`, `baseFolder` fields with `gitDest types.ResourceReference`
- [ ] Update `NewFolderReconciler()` signature to take `ResourceReference`
- [ ] Update `StartReconciliation()` to use simplified events
- [ ] Update `OnClusterState()` to use `Equal()` method
- [ ] Update `OnRepoState()` to use `Equal()` method
- [ ] Remove `GetRepoName()`, `GetBranch()`, `GetBaseFolder()` methods
- [ ] Add `GetGitDest()` method
- [ ] Update `String()` method
- [ ] Update all tests in `base_folder_reconciler_test.go`

**Estimated**: 0.5 days

### Phase 4: Update ReconcilerManager (REMOVE ReconcilerKey!)
**File**: `internal/reconcile/reconciler_manager.go`

**Tasks**:
- [ ] **DELETE ReconcilerKey struct entirely**
- [ ] Change map from `map[ReconcilerKey]*FolderReconciler` to `map[string]*FolderReconciler`
- [ ] Update `CreateReconciler()` to take `ResourceReference` and use `gitDest.Key()`
- [ ] Update `GetReconciler()` to take `ResourceReference`
- [ ] Update `DeleteReconciler()` to take `ResourceReference`
- [ ] Remove all ReconcilerKey usage
- [ ] Update all tests

**Estimated**: 0.5 days

### Phase 5: Update EventRouter
**File**: `internal/watch/event_router.go`

**Tasks**:
- [ ] Add fields: `ReconcilerManager`, `WatchManager`, `Client`
- [ ] Implement `ProcessControlEvent(ctx, event)`
- [ ] Implement `handleRequestClusterState()` - calls WatchManager service
- [ ] Implement `handleRequestRepoState()` - calls BranchWorker service
- [ ] Update `RouteClusterStateEvent()` to use `ResourceReference`
- [ ] Update `RouteRepoStateEvent()` to use `ResourceReference`
- [ ] Add unit tests for orchestration logic

**Estimated**: 1 day

### Phase 6: Update WatchManager Service
**File**: `internal/watch/manager.go`

**Tasks**:
- [ ] Implement `GetClusterStateForGitDest(ctx, gitDest ResourceReference)`
- [ ] Add helper: `addGVRsFromResourceRule()`
- [ ] Add helper: `addGVRsFromClusterResourceRule()`
- [ ] Add helper: `listResourcesForGVR()`
- [ ] Add helper: `objectMatchesGitDest()`
- [ ] Remove `getCurrentClusterState()` stub (line 690)
- [ ] Remove `EmitClusterStateSnapshot()` if not used
- [ ] Add unit tests for service method

**Estimated**: 1.5 days

### Phase 7: Update BranchWorker Service
**File**: `internal/git/branch_worker.go`

**Tasks**:
- [ ] Implement `ListResourcesInBaseFolder(baseFolder string)`
- [ ] Implement helper: `listResourceIdentifiersInBaseFolder()`
- [ ] Remove `EmitRepoState()` method (lines 165-207)
- [ ] Remove EventRouter field/dependency
- [ ] Update tests to use synchronous API

**Estimated**: 0.5 days

### Phase 8: Wire GitDestination Controller
**File**: `internal/controller/gitdestination_controller.go`

**Tasks**:
- [ ] Add `ReconcilerManager` field to controller struct
- [ ] Create `GitDestinationEventStream` in `Reconcile()`
- [ ] Register stream with EventRouter
- [ ] Create ONE `FolderReconciler` per GitDestination using `ResourceReference`
- [ ] Call `StartReconciliation()`
- [ ] Handle reconciliation completion signaling
- [ ] Add integration tests

**Estimated**: 1 day

### Phase 9: Update Main Wiring
**File**: `cmd/main.go`

**Tasks**:
- [ ] Create `ReconcilerManager`
- [ ] Wire EventRouter with all dependencies (ReconcilerManager, WatchManager, Client)
- [ ] Update GitDestination controller setup with ReconcilerManager

**Estimated**: 0.5 days

### Phase 10: Update All Tests
**Files**: All test files

**Tasks**:
- [ ] Update all event tests to use `ResourceReference`
- [ ] Remove all `ReconcilerKey` usage in tests
- [ ] Update FolderReconciler tests
- [ ] Update ReconcilerManager tests
- [ ] Add ResourceReference unit tests
- [ ] Verify >90% coverage maintained
- [ ] Run `make test` and fix any failures

**Estimated**: 1 day

### Phase 11: Integration Testing
**Files**: New integration test files

**Tasks**:
- [ ] Add control event flow integration test
- [ ] Add state event routing integration test
- [ ] Add end-to-end reconciliation flow test
- [ ] Verify event deduplication works
- [ ] Verify state machine transitions correctly
- [ ] Run `make test-e2e` and verify passes

**Estimated**: 1 day

### Phase 12: Documentation
**Files**: README.md, architecture docs

**Tasks**:
- [ ] Update README with new architecture
- [ ] Add sequence diagram for reconciliation flow
- [ ] Document `ResourceReference` pattern
- [ ] Update component descriptions
- [ ] Add migration guide if needed

**Estimated**: 0.5 days

---

## Total Timeline: 8.5 days

---

## Benefits Summary

| Aspect | Before | After | Improvement |
|--------|--------|-------|-------------|
| **Event Fields** | `RepoName`, `Branch`, `BaseFolder` (3 fields) | `GitDest ResourceReference` (1 field) | 66% reduction |
| **ReconcilerKey** | Custom struct with 3 fields | **REMOVED** - use `ResourceReference.Key()` | Eliminated custom type |
| **FolderReconciler Fields** | `repoName`, `branch`, `baseFolder` (3 fields) | `gitDest ResourceReference` (1 field) | 66% reduction |
| **Map Keys** | Custom `ReconcilerKey` type | Simple `string` from `gitDest.Key()` | Standard Go pattern |
| **BranchWorker API** | Event-based `EmitRepoState()` | Synchronous `ListResourcesInBaseFolder()` | Clean separation |
| **WatchManager API** | Event handler | Service method | Clear intent |
| **Reusability** | No reusable reference type | `ResourceReference` reusable everywhere | Better abstraction |

---

## Architecture Diagram

```
GitDestination CRD (repo+branch+baseFolder)
  │
  ├─ Managed by: GitDestinationReconciler (controller/gitdestination_controller.go)
  │   │
  │   ├─ Creates: GitDestinationEventStream (one per GitDest)
  │   │   └─ Buffers/deduplicates events → BranchWorker
  │   │
  │   └─ Creates: FolderReconciler (one per GitDest)
  │       └─ Stores: gitDest ResourceReference
  │           │
  │           ├─ Emits: ControlEvents(GitDest)
  │           │   ↓
  │           EventRouter (orchestrator)
  │           │   ├─ Calls: WatchManager.GetClusterStateForGitDest(GitDest)
  │           │   ├─ Calls: BranchWorker.ListResourcesInBaseFolder(baseFolder)
  │           │   └─ Emits: StateEvents(GitDest) → FolderReconciler
  │           │
  │           └─ Receives: StateEvents
  │               └─ reconcile() → Emits resource events
  │                   └─ GitDestinationEventStream → BranchWorker
```

---

## Key Design Principles

1. **GitDestination is the unit of work** - Everything references GitDest, not individual fields
2. **ResourceReference for clean APIs** - Reusable, type-safe, Kubernetes-style
3. **BranchWorker is pure Git service** - No event knowledge, synchronous API
4. **WatchManager is pure cluster service** - No event handling, synchronous API  
5. **EventRouter orchestrates** - Calls services, wraps results in events
6. **Events for async coordination** - Control flow and state propagation
7. **Method calls for data retrieval** - Synchronous, testable, clear

---

## Success Criteria

- [ ] `ResourceReference` type exists and is well-tested
- [ ] **`ReconcilerKey` is completely removed**
- [ ] All events use `ResourceReference` instead of individual fields
- [ ] All components use `ResourceReference` for GitDest references
- [ ] BranchWorker has no event knowledge (pure Git service)
- [ ] WatchManager provides synchronous service method
- [ ] EventRouter orchestrates with service calls
- [ ] Control events flow end-to-end
- [ ] State events route correctly to reconcilers
- [ ] All unit tests pass (>90% coverage)
- [ ] Integration tests verify full flow
- [ ] E2E tests pass
- [ ] Documentation updated

---

## Migration Notes

### Breaking Changes
1. Event structures change - affects any code using events
2. ReconcilerKey removed - affects ReconcilerManager API
3. BranchWorker API changes - affects anything calling BranchWorker

### Migration Strategy
1. Implement all phases in order
2. Update tests incrementally
3. Verify each phase before proceeding
4. Keep git history clean with one commit per phase

---

## Next Steps

1. **Phase 1**: Create `ResourceReference` type and tests ✅
2. **Phase 2**: Update event structures (breaking change - atomic) ✅
3. **Phase 3**: Update FolderReconciler ✅
4. **Phase 4**: Update ReconcilerManager and **REMOVE ReconcilerKey** ✅
5. **Phase 5-7**: Update coordinators and services
6. **Phase 8**: Wire in controller
7. **Phase 9-12**: Integration, testing, documentation

The plan is now fully updated with `ResourceReference` and `ReconcilerKey` removal!