# Implementation Plan: GitDestination-Based Event Queueing

## Executive Summary

**Objective**: Refactor event queueing from GitRepoConfig-based to GitDestination-based to ensure proper branch isolation and eliminate "invalid reference name" errors.

**Impact**: Medium-High (affects event flow, worker dispatch, rule storage, and seed sync)

**Risk**: Medium (core architecture change, but additive to event structure)

**Estimated Effort**: 2-3 hours implementation + comprehensive testing

---

## Problem Statement

Currently, events are batched by `GitRepoConfigRef`, but a single GitRepoConfig can be referenced by multiple GitDestinations with different branches. This causes:

1. **Branch mixing**: Events for different branches end up in same queue
2. **Missing branch context**: SEED_SYNC and orphan DELETE events lack branch information
3. **Git checkout failures**: "invalid reference name" when branch is empty

**Root cause**: GitDestination (not GitRepoConfig) is the true "deployment target" that should own a queue.

---

## Architectural Change

### Current: Queue per GitRepoConfig
```
Queue Key: "namespace/gitrepoconfig-name"

GitRepoConfig "shared-repo"
  └─ Queue: "default/shared-repo"
      ├─ Event from GitDestination "prod" (branch: main)
      ├─ Event from GitDestination "dev"  (branch: dev)
      └─ SEED_SYNC (?? which branch ??)
```

### Target: Queue per GitDestination
```
Queue Key: "namespace/gitdestination-name"

GitDestination "prod" (repo: shared-repo, branch: main)
  └─ Queue: "default/prod"
      ├─ All events: branch=main, baseFolder=clusters/prod
      └─ SEED_SYNC: knows branch=main implicitly

GitDestination "dev" (repo: shared-repo, branch: dev)
  └─ Queue: "default/dev"
      ├─ All events: branch=dev, baseFolder=clusters/dev
      └─ SEED_SYNC: knows branch=dev implicitly
```

---

## Implementation Steps

### Phase 1: Data Structure Changes

#### Step 1.1: Extend Event Structure
**File**: [`internal/eventqueue/queue.go`](../../internal/eventqueue/queue.go:30-56)

**Change**: Add GitDestination tracking fields to Event struct

```go
type Event struct {
    // Existing fields
    Object                 *unstructured.Unstructured
    Identifier             types.ResourceIdentifier
    Operation              string
    UserInfo               UserInfo
    
    // NEW: Primary reference (GitDestination = deployment target)
    GitDestinationRef       string
    GitDestinationNamespace string
    
    // KEEP: Secondary reference (for direct repo access when needed)
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    
    // Derived from GitDestination (redundant but convenient)
    Branch     string
    BaseFolder string
}
```

**Testing**: No behavior change yet, just structure extension

---

#### Step 1.2: Extend CompiledRule Structure
**File**: [`internal/rulestore/store.go`](../../internal/rulestore/store.go:35-52)

**Change**: Track GitDestination source in compiled rules

```go
type CompiledRule struct {
    Source types.NamespacedName  // WatchRule name
    
    // NEW: Track GitDestination that provided these values
    GitDestinationRef       string
    GitDestinationNamespace string
    
    // KEEP: Resolved values (from GitDestination spec)
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    BaseFolder             string
    IsClusterScoped        bool
    ResourceRules          []CompiledResourceRule
}

// Same changes for CompiledClusterRule
type CompiledClusterRule struct {
    Source types.NamespacedName
    
    // NEW: Track GitDestination
    GitDestinationRef       string
    GitDestinationNamespace string
    
    // KEEP: Resolved values
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    BaseFolder             string
    Rules                  []CompiledClusterResourceRule
}
```

**Testing**: Update unit tests in `store_test.go` to verify new fields

---

### Phase 2: Rule Storage Updates

#### Step 2.1: Update WatchRule Controller
**File**: [`internal/controller/watchrule_controller.go`](../../internal/controller/watchrule_controller.go:190)

**Change**: Pass GitDestination reference to rule store

```go
// Line 190 - Current:
r.RuleStore.AddOrUpdateWatchRule(*watchRule, grc.Name, grcNS, dest.Spec.Branch, dest.Spec.BaseFolder)

// NEW signature:
r.RuleStore.AddOrUpdateWatchRule(
    *watchRule,
    dest.Name, destNS,              // GitDestination ref
    grc.Name, grcNS,                // GitRepoConfig ref
    dest.Spec.Branch,
    dest.Spec.BaseFolder,
)
```

**Dependencies**: Requires Step 1.2 completed

**Testing**: Verify WatchRule reconciliation still works, check rule store contents

---

#### Step 2.2: Update ClusterWatchRule Controller
**File**: [`internal/controller/clusterwatchrule_controller.go`](../../internal/controller/clusterwatchrule_controller.go)

**Change**: Same as Step 2.1 but for ClusterWatchRule

**Dependencies**: Requires Step 1.2 completed

**Testing**: Verify ClusterWatchRule reconciliation, check rule store

---

#### Step 2.3: Update RuleStore Methods
**File**: [`internal/rulestore/store.go`](../../internal/rulestore/store.go:112-155)

**Change**: Update method signatures to accept GitDestination refs

```go
// Line 112-126
func (s *RuleStore) AddOrUpdateWatchRule(
    rule configv1alpha1.WatchRule,
    gitDestinationName string,      // NEW parameter (before gitRepoConfigName)
    gitDestinationNamespace string, // NEW parameter
    gitRepoConfigName string,
    gitRepoConfigNamespace string,
    branch string,
    baseFolder string,
) {
    // ... populate GitDestinationRef/Namespace in CompiledRule ...
}

// Same for AddOrUpdateClusterWatchRule
```

**Dependencies**: Steps 2.1 and 2.2 must call with new parameters

**Testing**: Unit tests in `store_test.go` - verify GitDestination refs stored correctly

---

### Phase 3: Event Creation Updates

#### Step 3.1: Update Watch Event Creation
**File**: [`internal/watch/informers.go`](../../internal/watch/informers.go:108-135)

**Change**: Populate GitDestination fields when creating events

```go
// Line 108-119 - WatchRule matches
for _, rule := range wrRules {
    ev := eventqueue.Event{
        Object:                 sanitized.DeepCopy(),
        Identifier:             id,
        Operation:              string(op),
        UserInfo:               userInfo,
        
        // NEW: Primary reference
        GitDestinationRef:       rule.GitDestinationRef,
        GitDestinationNamespace: rule.GitDestinationNamespace,
        
        // KEEP: Secondary reference + derived values
        GitRepoConfigRef:       rule.GitRepoConfigRef,
        GitRepoConfigNamespace: rule.GitRepoConfigNamespace,
        Branch:                 rule.Branch,
        BaseFolder:             rule.BaseFolder,
    }
    m.EventQueue.Enqueue(ev)
}

// Line 123-135 - ClusterWatchRule matches (same pattern)
```

**Dependencies**: Requires Phase 2 (rules have GitDestination refs)

**Testing**: Verify events have GitDestination refs, check queue contents

---

#### Step 3.2: Update Seed Event Creation
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:204-254)

**Change**: Same as Step 3.1 for `enqueueMatches()` method

**Dependencies**: Requires Phase 2 completed

**Testing**: Verify seed events have complete context

---

### Phase 4: Seed Sync Refactoring

#### Step 4.1: Track Destinations During Seed
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:340-366)

**Change**: Track GitDestination keys instead of GitRepoConfig keys

```go
// Line 356-357 - Current:
repoKeys := make(map[k8stypes.NamespacedName]struct{})

// NEW:
type DestinationContext struct {
    GitDestinationRef       string
    GitDestinationNamespace string
    GitRepoConfigRef        string
    GitRepoConfigNamespace  string
    Branch                  string
    BaseFolder              string
}
destKeys := make(map[k8stypes.NamespacedName]DestinationContext)
```

**Dependencies**: Requires Phase 3 (events have destination refs)

**Testing**: Verify correct destinations tracked during seed

---

#### Step 4.2: Update processListedObject
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:565-601)

**Change**: Track GitDestination keys from matching rules

```go
// Line 586-591 - Current: Track GitRepoConfig
for _, r := range wrRules {
    repoKeys[k8stypes.NamespacedName{Name: r.GitRepoConfigRef, Namespace: r.Source.Namespace}] = struct{}{}
}

// NEW: Track GitDestination with full context
for _, r := range wrRules {
    destKey := k8stypes.NamespacedName{
        Name:      r.GitDestinationRef,
        Namespace: r.GitDestinationNamespace,
    }
    destKeys[destKey] = DestinationContext{
        GitDestinationRef:       r.GitDestinationRef,
        GitDestinationNamespace: r.GitDestinationNamespace,
        GitRepoConfigRef:        r.GitRepoConfigRef,
        GitRepoConfigNamespace:  r.GitRepoConfigNamespace,
        Branch:                  r.Branch,
        BaseFolder:              r.BaseFolder,
    }
}
// Same for ClusterWatchRules
```

**Dependencies**: Step 4.1

**Testing**: Verify destination tracking, check destKeys map contents

---

#### Step 4.3: Emit SEED_SYNC per Destination
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:603-615)

**Change**: Create SEED_SYNC with complete GitDestination context

```go
// Line 604-614 - Current:
func (m *Manager) emitSeedSyncControls(repoKeys map[k8stypes.NamespacedName]struct{}) {
    for key := range repoKeys {
        m.EventQueue.Enqueue(eventqueue.Event{
            Operation:              "SEED_SYNC",
            GitRepoConfigRef:       key.Name,
            GitRepoConfigNamespace: key.Namespace,
            // Missing: Branch, GitDestinationRef
        })
    }
}

// NEW:
func (m *Manager) emitSeedSyncControls(destKeys map[k8stypes.NamespacedName]DestinationContext) {
    for destKey, ctx := range destKeys {
        m.EventQueue.Enqueue(eventqueue.Event{
            Operation:               "SEED_SYNC",
            GitDestinationRef:       ctx.GitDestinationRef,
            GitDestinationNamespace: ctx.GitDestinationNamespace,
            GitRepoConfigRef:        ctx.GitRepoConfigRef,
            GitRepoConfigNamespace:  ctx.GitRepoConfigNamespace,
            Branch:                  ctx.Branch,
            BaseFolder:              ctx.BaseFolder,
        })
    }
}
```

**Dependencies**: Step 4.2

**Testing**: Verify SEED_SYNC events have Branch field, check worker logs

---

### Phase 5: Worker Dispatch Changes

#### Step 5.1: Change Queue Key to GitDestination
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:169)

**Change**: Dispatch by GitDestination instead of GitRepoConfig

```go
// Line 169 - Current:
queueKey := event.GitRepoConfigNamespace + "/" + event.GitRepoConfigRef

// NEW:
queueKey := event.GitDestinationNamespace + "/" + event.GitDestinationRef
```

**Dependencies**: Requires Phase 3 (all events have GitDestinationRef)

**Testing**: 
- Verify queue keys match GitDestination names
- Check that each GitDestination gets its own queue
- Ensure no queue collisions

---

#### Step 5.2: Remove Branch Extraction Logic
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:484-498)

**Change**: Simplify commitAndPush - branch always present in events

```go
// Current (after quick fix):
branch := ""
for _, event := range events {
    if event.Branch != "" {
        branch = event.Branch
        break
    }
}
if branch == "" && len(repoConfig.Spec.AllowedBranches) > 0 {
    branch = repoConfig.Spec.AllowedBranches[0]
}

// NEW (simplified - all events in queue have same branch):
branch := ""
if len(events) > 0 {
    branch = events[0].Branch
}
// No fallback needed - GitDestination ensures branch is always set
if branch == "" {
    log.Error(nil, "CRITICAL: Event batch has no branch - this should never happen with GitDestination queuing")
    return
}
```

**Dependencies**: All of Phase 4 completed

**Testing**: 
- Verify branch is always present
- Add assertion that branch is non-empty
- Test error handling if somehow empty

---

### Phase 6: Orphan Detection Updates

#### Step 6.1: Pass Branch Context to computeOrphanDeletes
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:377-393)

**Change**: Extract branch from SEED_SYNC event, pass to orphan detection

```go
// Line 378-384 - Current:
if strings.EqualFold(event.Operation, "SEED_SYNC") {
    deletes := w.computeOrphanDeletes(ctx, log, repoConfig, sLivePaths)
    // ...
}

// NEW:
if strings.EqualFold(event.Operation, "SEED_SYNC") {
    // SEED_SYNC now has branch from GitDestination
    deletes := w.computeOrphanDeletes(ctx, log, repoConfig, event.Branch, event.BaseFolder, sLivePaths)
    // ...
}
```

**Dependencies**: Phase 4 (SEED_SYNC has branch)

**Testing**: Verify branch passed correctly to orphan detection

---

#### Step 6.2: Update computeOrphanDeletes Signature
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:583-632)

**Change**: Accept branch/baseFolder parameters, use them for DELETE events

```go
// Line 585-590 - Current signature:
func (w *Worker) computeOrphanDeletes(
    ctx context.Context,
    log logr.Logger,
    repoConfig v1alpha1.GitRepoConfig,
    sLive map[string]struct{},
) []eventqueue.Event {

// NEW signature:
func (w *Worker) computeOrphanDeletes(
    ctx context.Context,
    log logr.Logger,
    repoConfig v1alpha1.GitRepoConfig,
    branch string,      // NEW: From SEED_SYNC event
    baseFolder string,  // NEW: From SEED_SYNC event
    sLive map[string]struct{},
) []eventqueue.Event {
    // ... implementation uses branch parameter ...
    
    // Line 619-627 - When creating DELETE events:
    evs = append(evs, eventqueue.Event{
        // ... existing fields ...
        Branch:     branch,      // From parameter
        BaseFolder: baseFolder,  // From parameter
    })
}
```

**Dependencies**: Step 6.1

**Testing**: Verify orphan DELETEs have correct branch/baseFolder

---

#### Step 6.3: Update listRepoYAMLPaths
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:634-684)

**Change**: Accept branch parameter instead of guessing

```go
// Line 635-648 - Current:
func (w *Worker) listRepoYAMLPaths(ctx context.Context, repoConfig v1alpha1.GitRepoConfig) ([]string, error) {
    // ...
    branch := ""
    if len(repoConfig.Spec.AllowedBranches) > 0 {
        branch = repoConfig.Spec.AllowedBranches[0]  // ❌ GUESSING
    }
    // ...
}

// NEW:
func (w *Worker) listRepoYAMLPaths(
    ctx context.Context,
    repoConfig v1alpha1.GitRepoConfig,
    branch string,  // NEW: Explicit branch from caller
) ([]string, error) {
    // Use the provided branch directly
    if err := repo.Checkout(branch); err != nil {
        return nil, err
    }
    // ...
}
```

**Dependencies**: Step 6.2 must call with branch parameter

**Testing**: Verify correct branch used for repo listing

---

### Phase 7: Integration & Testing

#### Step 7.1: Update Unit Tests

**Files to update**:
- `internal/eventqueue/queue_test.go` - Test new Event fields
- `internal/rulestore/store_test.go` - Test new CompiledRule fields
- `internal/git/worker_test.go` - Test GitDestination-based queuing
- `internal/watch/manager_test.go` - Test destination tracking during seed

**Focus areas**:
- Event creation with all fields populated
- Queue key generation from GitDestination
- SEED_SYNC with branch context
- Orphan detection with explicit branch

---

#### Step 7.2: Update Integration Tests

**File**: `internal/correlation/integration_test.go`

**Change**: Verify events have GitDestination refs in correlation scenarios

---

#### Step 7.3: E2E Test Validation

**File**: `test/e2e/e2e_test.go`

**Validation points**:
1. Multiple GitDestinations sharing GitRepoConfig work correctly
2. Each destination's queue is independent
3. No "invalid reference name" errors
4. SEED_SYNC operates on correct branch
5. Orphan detection respects branch/baseFolder boundaries

**New test case**: Create 2 GitDestinations pointing to same GitRepoConfig but different branches

---

## Implementation Order & Dependencies

```
Phase 1: Data Structures (parallel-safe)
  ├─ 1.1: Event structure
  └─ 1.2: CompiledRule structure

Phase 2: Rule Storage (depends on Phase 1)
  ├─ 2.3: RuleStore methods (update signatures)
  ├─ 2.1: WatchRule controller (calls new signatures)
  └─ 2.2: ClusterWatchRule controller (calls new signatures)

Phase 3: Event Creation (depends on Phase 2)
  ├─ 3.1: Watch event creation
  └─ 3.2: Seed event creation

Phase 4: Seed Sync (depends on Phase 3)
  ├─ 4.1: Track destinations
  ├─ 4.2: Update processListedObject
  └─ 4.3: Emit SEED_SYNC with context

Phase 5: Worker Dispatch (depends on Phase 4)
  ├─ 5.1: Change queue key
  └─ 5.2: Simplify branch extraction

Phase 6: Orphan Detection (depends on Phase 5)
  ├─ 6.1: Pass branch to computeOrphanDeletes
  ├─ 6.2: Update computeOrphanDeletes signature
  └─ 6.3: Update listRepoYAMLPaths

Phase 7: Testing (depends on Phase 6)
  ├─ 7.1: Unit tests
  ├─ 7.2: Integration tests
  └─ 7.3: E2E validation
```

---

## Validation Checklist

Before considering implementation complete:

- [ ] All events have GitDestinationRef populated
- [ ] Queue keys use GitDestination (not GitRepoConfig)
- [ ] SEED_SYNC events have Branch field
- [ ] Orphan DELETE events have correct Branch
- [ ] No "invalid reference name" errors in e2e tests
- [ ] Multiple GitDestinations sharing one GitRepoConfig work correctly
- [ ] Different branches properly isolated in separate queues
- [ ] All unit tests pass with >90% coverage
- [ ] All integration tests pass
- [ ] All e2e tests pass
- [ ] `make lint` passes without errors
- [ ] Metrics still track queue depths correctly

---

## Rollback Strategy

If implementation fails:

1. **Revert order**: Reverse of implementation order (Phase 7 → 6 → ... → 1)
2. **Safe points**: Each phase can be reverted independently
3. **Quick fix remains**: Current branch extraction logic provides fallback behavior
4. **Git history**: Use git to revert specific commits by phase

---

## Risk Mitigation

1. **Additive changes**: New Event fields don't break existing code until used
2. **Backward compatibility**: Keep GitRepoConfig refs in events during transition
3. **Incremental testing**: Test after each phase completion
4. **Metrics validation**: Ensure queue depth metrics still accurate
5. **Performance**: GitDestination-based queuing may create more queues but better isolation

---

## Success Criteria

1. ✅ No "invalid reference name" errors in any test scenario
2. ✅ SEED_SYNC events have explicit branch (not inferred)
3. ✅ Orphan deletions use correct branch (not guessed)
4. ✅ Multiple GitDestinations can share GitRepoConfig without conflicts
5. ✅ Event batches contain only events for single branch
6. ✅ All tests pass (unit, integration, e2e)
7. ✅ Code passes all linting and quality checks

---

## Post-Implementation

After successful implementation:

1. Update [`IMPLEMENTATION_SUMMARY.md`](IMPLEMENTATION_SUMMARY.md) with architecture change
2. Add section to [`CONTRIBUTING.md`](../CONTRIBUTING.md) about GitDestination queueing
3. Update metrics documentation if queue tracking changes
4. Consider removing temporary quick-fix code once fully validated