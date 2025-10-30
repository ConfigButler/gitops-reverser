# Sync Architecture Refactor: Event-Based Orphan Detection - CORRECTED PLAN

## Executive Summary

### What We Have Now (Accomplishments)

✅ **Phase 1-4 COMPLETED**: We have successfully implemented the core components:
- `BaseFolderReconciler` with pure reconciliation logic
- `GitDestinationEventStream` with state machine and event buffering
- `ReconcilerManager` for lifecycle management
- Enhanced `BranchWorker` with `EmitRepoState()` method
- Event types: `ClusterStateEvent`, `RepoStateEvent`, control events
- Comprehensive unit tests with >90% coverage

✅ **What We're Proud Of**:
- **Clean Architecture**: Single responsibility per component
- **Testability**: Isolated unit testing of reconciliation logic
- **Event-Based Design**: Proper separation of concerns with events
- **State Machine**: Deterministic behavior with STARTUP_RECONCILE → LIVE_PROCESSING
- **Event Deduplication**: Hash-based duplicate prevention
- **Base Folder Scoping**: Per-baseFolder reconciliation safety

### Why We Need This Change (Problems with Current Implementation)

❌ **Current Architecture Issues**:
- **WatchManager routes directly to BranchWorkers** (bypasses GitDestinationEventStream)
- **No reconciliation event flow** (BaseFolderReconciler exists but unused)
- **Mixed responsibilities** still exist (BranchWorker handles both live and reconciliation events)
- **No event buffering/deduplication** during reconciliation
- **Race conditions** possible between live events and reconciliation

❌ **Missing Critical Features**:
- GitDestinationEventStream not integrated into event flow
- No REQUEST_CLUSTER_STATE / REQUEST_REPO_STATE control flow
- No time-based safety for reconciliation events
- Reconciliation events not properly separated from live events

## Corrected Implementation Plan

### Phase 5: Fix EventRouter Integration
**Goal**: Route events to GitDestinationEventStreams instead of directly to BranchWorkers

**Tasks**:
- Modify `WatchManager.enqueueMatches()` to route to GitDestinationEventStreams
- Update `EventRouter` to support routing to GitDestinationEventStreams
- Implement `EventRouter.RouteToGitDestinationEventStream()` method
- Update GitDestination controller to register GitDestinationEventStreams with EventRouter

**Code Changes**:
```go
// In WatchManager
func (m *Manager) enqueueMatches(...) {
    // Instead of: m.EventRouter.RouteEvent(...)
    // Use: m.EventRouter.RouteToGitDestinationEventStream(event, gitDestName)
}

// In EventRouter
func (r *EventRouter) RouteToGitDestinationEventStream(event git.Event, gitDestName string) error {
    // Route to specific GitDestinationEventStream
    return r.routeToEventStream(event, gitDestName)
}
```

### Phase 6: Implement Reconciliation Control Flow
**Goal**: Enable BaseFolderReconciler to request and receive state snapshots

**Tasks**:
- Implement `REQUEST_CLUSTER_STATE` control event handling in WatchManager
- Implement `REQUEST_REPO_STATE` control event handling in BranchWorker
- Wire BaseFolderReconciler to emit control events via EventRouter
- Implement state snapshot emission (ClusterStateEvent, RepoStateEvent)

**Code Changes**:
```go
// BaseFolderReconciler triggers reconciliation
func (r *BaseFolderReconciler) startReconciliation() {
    // Emit REQUEST_CLUSTER_STATE
    r.eventEmitter.EmitControlEvent(events.ControlEvent{
        Type:       events.RequestClusterState,
        RepoName:   r.repoName,
        Branch:     r.branch,
        BaseFolder: r.baseFolder,
    })

    // Emit REQUEST_REPO_STATE
    r.eventEmitter.EmitControlEvent(events.ControlEvent{
        Type:       events.RequestRepoState,
        RepoName:   r.repoName,
        Branch:     r.branch,
        BaseFolder: r.baseFolder,
    })
}
```

### Phase 7: Wire Reconciliation Events to GitDestinationEventStream
**Goal**: BaseFolderReconciler emits CREATE/DELETE events that flow through GitDestinationEventStream

**Tasks**:
- Implement `ReconcileEventEmitter` interface in GitDestinationEventStream
- Wire BaseFolderReconciler to emit events to GitDestinationEventStream
- Ensure reconciliation events are buffered during STARTUP_RECONCILE phase
- Implement event forwarding from GitDestinationEventStream to BranchWorker

**Code Changes**:
```go
// GitDestinationEventStream implements ReconcileEventEmitter
func (s *GitDestinationEventStream) EmitCreateEvent(resource types.ResourceIdentifier) error {
    event := git.Event{Operation: "CREATE", Identifier: resource, /* ... */}
    return s.OnWatchEvent(event) // Process through state machine
}
```

### Phase 8: Enhanced Testing Strategy
**Goal**: Comprehensive testing to verify correct behavior

**Unit Tests**:
```go
func TestGitDestinationEventStream_StateMachine(t *testing.T) {
    stream := NewGitDestinationEventStream("dest", "ns", mockBranchWorker, logr.Discard())

    // Test STARTUP_RECONCILE: events buffered
    event := git.Event{Operation: "UPDATE", Identifier: testResource}
    stream.OnWatchEvent(event)
    assert.Equal(t, EventStreamStateStartupReconcile, stream.GetState())
    assert.Equal(t, 1, stream.GetBufferedEventCount())

    // Test transition to LIVE_PROCESSING
    stream.OnReconciliationComplete()
    assert.Equal(t, EventStreamStateLiveProcessing, stream.GetState())

    // Test buffered events processed
    // (Verify BranchWorker.Enqueue called for buffered events)
}

func TestGitDestinationEventStream_EventDeduplication(t *testing.T) {
    stream := NewGitDestinationEventStream("dest", "ns", mockBranchWorker, logr.Discard())
    stream.OnReconciliationComplete() // Enter LIVE_PROCESSING

    // Send same event twice
    event := git.Event{Operation: "UPDATE", Identifier: testResource, Object: testObject}
    stream.OnWatchEvent(event)
    stream.OnWatchEvent(event) // Duplicate

    // Verify only processed once
    assert.Equal(t, 1, mockBranchWorker.EnqueueCallCount)
}

func TestBaseFolderReconciler_ReconciliationFlow(t *testing.T) {
    reconciler := NewBaseFolderReconciler("repo", "branch", "apps", mockEventEmitter, logr.Discard())

    // Simulate receiving both states
    reconciler.OnClusterState(ClusterStateEvent{Resources: clusterResources})
    reconciler.OnRepoState(RepoStateEvent{Resources: gitResources})

    // Verify reconciliation events emitted
    assert.Equal(t, 1, mockEventEmitter.CreateEventCount)  // Missing in Git
    assert.Equal(t, 1, mockEventEmitter.DeleteEventCount)  // Orphan in Git
    assert.Equal(t, 1, mockEventEmitter.ReconcileEventCount) // Exists in both
}

func TestEventRouter_GitDestinationEventStreamRouting(t *testing.T) {
    router := NewEventRouter()
    stream := NewGitDestinationEventStream("dest", "ns", mockBranchWorker, logr.Discard())

    // Register stream with router
    router.RegisterGitDestinationEventStream("dest", "ns", stream)

    // Route event to stream
    event := git.Event{Operation: "UPDATE", Identifier: testResource}
    err := router.RouteToGitDestinationEventStream(event, "dest", "ns")

    // Verify event reached stream
    assert.NoError(t, err)
    assert.Equal(t, 1, stream.GetProcessedEventCount())
}
```

**Integration Tests**:
```go
func TestEndToEnd_EventFlow(t *testing.T) {
    // Setup: WatchManager + EventRouter + GitDestinationEventStream + BranchWorker

    // Simulate cluster change
    watchManager.SimulateResourceCreation(testResource)

    // Verify event flow:
    // 1. WatchManager routes to GitDestinationEventStream
    // 2. GitDestinationEventStream buffers/forwards to BranchWorker
    // 3. BranchWorker commits to Git

    assert.True(t, gitRepository.ContainsResource(testResource))
}

func TestReconciliation_EventFlow(t *testing.T) {
    // Setup: BaseFolderReconciler + GitDestinationEventStream + BranchWorker

    // Trigger reconciliation
    reconciler.StartReconciliation()

    // Verify control events emitted
    assert.Equal(t, 1, eventRouter.RequestClusterStateCount)
    assert.Equal(t, 1, eventRouter.RequestRepoStateCount)

    // Simulate state responses
    reconciler.OnClusterState(clusterState)
    reconciler.OnRepoState(repoState)

    // Verify reconciliation events through GitDestinationEventStream
    assert.Equal(t, expectedCreateEvents, gitDestinationEventStream.CreateEventsProcessed)
    assert.Equal(t, expectedDeleteEvents, gitDestinationEventStream.DeleteEventsProcessed)
}
```

### Phase 9: Performance and Safety Validation
**Goal**: Ensure the new architecture performs well and prevents race conditions

**Performance Tests**:
- Measure event throughput with multiple GitDestinations
- Test reconciliation time with large numbers of resources
- Validate event buffering doesn't cause memory issues

**Safety Tests**:
- Test race condition prevention between live and reconciliation events
- Verify event deduplication works correctly
- Test state machine transitions under load

### Phase 10: Migration and Cleanup
**Goal**: Remove legacy code and complete the refactor

**Tasks**:
- Remove direct WatchManager → BranchWorker routing
- Remove old SEED_SYNC handling
- Update documentation
- Final integration testing

## Expected Behavior After Implementation

### Live Event Processing:
```
Cluster Change → WatchManager → EventRouter → GitDestinationEventStream → BranchWorker → Git Commit
```

### Reconciliation Processing:
```
BaseFolderReconciler → Control Events → State Snapshots → Reconciliation Logic → Reconciliation Events → GitDestinationEventStream → BranchWorker → Git Commit
```

### Key Benefits:
- **Event Buffering**: Live events buffered during reconciliation startup
- **Deduplication**: Hash-based duplicate prevention
- **State Machine**: Deterministic STARTUP_RECONCILE → LIVE_PROCESSING transition
- **Separation**: Live events vs reconciliation events clearly separated
- **Safety**: No race conditions between live and reconciliation processing

## Risk Assessment

### Low Risk:
- All components already implemented and tested
- EventRouter changes are additive
- Can rollback by keeping direct BranchWorker routing

### Medium Risk:
- Event routing changes could affect performance
- State machine complexity in GitDestinationEventStream

### Mitigation:
- Comprehensive unit and integration tests
- Gradual rollout with feature flags
- Performance benchmarking before/after

## Success Criteria

✅ **All unit tests pass** (>90% coverage)
✅ **Integration tests verify event flows**
✅ **Performance tests show no regression**
✅ **E2E tests pass with new architecture**
✅ **No race conditions between live and reconciliation events**
✅ **Event deduplication works correctly**
✅ **State machine transitions are deterministic**

This corrected plan addresses the architectural gaps and ensures we implement the sophisticated event-based architecture as originally designed.