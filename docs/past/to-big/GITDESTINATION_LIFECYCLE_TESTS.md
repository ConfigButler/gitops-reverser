# GitDestination Lifecycle Integration Tests

## Overview

This document describes the integration tests for GitDestination lifecycle scenarios using GitDestinationEventStream instances. These tests verify the isolation, duplication, and convergence behavior when multiple GitDestinationEventStream instances share a BranchWorker.

## Test Location

**File:** `internal/reconcile/gitdestination_lifecycle_integration_test.go`

## Test Scenarios

### 1. TestGitDestinationEventStream_MultipleStreamsWithSharedWorker

**Purpose:** Verify that multiple GitDestinationEventStream instances can share a single BranchWorker without interfering with each other.

**Behavior Verified:**
- Each stream maintains its own `baseFolder`
- Events maintain proper isolation by baseFolder
- All events converge at the shared worker

**Example:**
```go
// Create shared mock worker
mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}

// Create first stream for "apps" folder
stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)

// Create second stream for "infra" folder
stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

// Send events to each stream
stream1.OnWatchEvent(createTestEventWithBaseFolder("pod", "app-pod", "CREATE", "apps"))
stream2.OnWatchEvent(createTestEventWithBaseFolder("deployment", "nginx", "CREATE", "infra"))
```

### 2. TestGitDestinationEventStream_DuplicateEventsAcrossStreams

**Purpose:** Verify that the same cluster resource change produces separate events for each GitDestinationEventStream watching it.

**Behavior Verified:**
- Single cluster resource changes generate multiple Git writes (one per stream)
- Events are intentionally duplicated across different baseFolders
- Each stream receives its own copy with the correct baseFolder

**Key Insight:** This represents the expected behavior where multiple GitDestinationEventStreams observe the same cluster state but write to different locations in Git.

**Example:**
```go
// Create two streams for different clusters
streamClusterA := NewGitDestinationEventStream("cluster-a-dest", "default", mockWorker, logger)
streamClusterB := NewGitDestinationEventStream("cluster-b-dest", "default", mockWorker, logger)

// Same resource, different baseFolders
eventForClusterA := createTestEventWithBaseFolder("configmap", "shared-config", "UPDATE", "cluster-a")
eventForClusterB := createTestEventWithBaseFolder("configmap", "shared-config", "UPDATE", "cluster-b")

streamClusterA.OnWatchEvent(eventForClusterA)  // Writes to cluster-a/
streamClusterB.OnWatchEvent(eventForClusterB)  // Writes to cluster-b/
```

### 3. TestGitDestinationEventStream_StreamDeletion

**Purpose:** Verify the behavior when a GitDestinationEventStream is deleted (stops sending events).

**Behavior Verified:**
- Other streams continue to operate normally
- Shared worker continues processing events from remaining streams
- **Files from deleted stream remain in Git** (no automatic cleanup)

**Important Note:** When a GitDestination is deleted, its files remain in the Git repository. This is intentional behavior - the operator does not perform automatic cleanup of Git files.

**Example:**
```go
stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)
stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

// Send events to both
stream1.OnWatchEvent(event1)
stream2.OnWatchEvent(event2)

// Simulate deletion of stream1 (garbage collected)
stream1 = nil

// stream2 continues to work
stream2.OnWatchEvent(event3)  // Still works!
```

### 4. TestGitDestinationEventStream_EventConvergence

**Purpose:** Verify that events from multiple GitDestinationEventStream instances converge at the shared BranchWorker for batched commit processing.

**Behavior Verified:**
- Multiple streams can send events concurrently
- All events converge at the shared worker
- Events from different baseFolders are batched together in commits
- Concurrent event submission is handled safely

**Key Insight:** This demonstrates the core design principle - multiple GitDestinationEventStreams share a single BranchWorker per (repo, branch) combination to ensure serialized commits and prevent merge conflicts.

**Example:**
```go
// Create three streams
streamTeamA := NewGitDestinationEventStream("team-a-dest", "default", mockWorker, logger)
streamTeamB := NewGitDestinationEventStream("team-b-dest", "default", mockWorker, logger)
streamTeamC := NewGitDestinationEventStream("team-c-dest", "default", mockWorker, logger)

// Concurrent event submission
go func() { streamTeamA.OnWatchEvent(eventForTeamA) }()
go func() { streamTeamB.OnWatchEvent(eventForTeamB) }()
go func() { streamTeamC.OnWatchEvent(eventForTeamC) }()

// All events converge at the worker
// Single commit may contain changes to team-a/, team-b/, and team-c/
```

### 5. TestGitDestinationEventStream_DeduplicationPerStream

**Purpose:** Verify that each GitDestinationEventStream performs its own deduplication independently.

**Behavior Verified:**
- Duplicate events within a stream are deduplicated
- Same event sent to different streams is NOT deduplicated (intentional)
- Deduplication is stream-scoped, not global

**Example:**
```go
stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)
stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

// Send same event twice to stream1 (deduplicated within stream)
event1a := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "apps")
event1b := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "apps")
stream1.OnWatchEvent(event1a)
stream1.OnWatchEvent(event1b)  // Ignored - duplicate within stream

// Send same resource to stream2 (NOT deduplicated across streams)
event2 := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "infra")
stream2.OnWatchEvent(event2)  // Processed - different stream

// Result: 2 events total (1 from stream1, 1 from stream2)
```

## Architecture Principles Validated

### 1. Event Isolation by BaseFolder

GitDestinationEventStreams write to separate baseFolders, ensuring logical separation of resources even when sharing the same branch:

```
repo/main/
├── cluster-a/          # GitDestination 1
│   └── namespace/
│       └── pod.yaml
└── cluster-b/          # GitDestination 2
    └── namespace/
        └── pod.yaml
```

### 2. Event Duplication is Intentional

When multiple GitDestinationEventStreams watch the same resources, they intentionally create duplicate writes to different baseFolders. This is the expected behavior for multi-cluster or multi-environment scenarios.

### 3. Deduplication is Stream-Scoped

Each GitDestinationEventStream maintains its own deduplication state. The same resource change:
- Is deduplicated within a single stream (prevents redundant writes)
- Is NOT deduplicated across streams (intentional duplication for multi-destination scenarios)

### 4. Convergence at BranchWorker

All events for a (repo, branch) combination converge at a single BranchWorker, ensuring:
- Serialized commits (no merge conflicts)
- Efficient batching across streams
- Consistent push intervals

### 5. No Automatic Cleanup

When a GitDestinationEventStream is deleted:
- Its files remain in Git (preservation of history)
- Other streams continue operating normally
- Manual cleanup can be performed if desired

## Test Implementation Details

### Mock Worker Pattern

The tests use a `mockEventEnqueuer` to track events sent to the shared worker:

```go
type mockEventEnqueuer struct {
    mu     sync.Mutex
    events []git.Event
}
```

This allows verification of:
- Event count
- Event baseFolders
- Event ordering
- Concurrent event handling
- Cross-stream behavior

### Test Helpers

- `createTestEventWithBaseFolder()`: Creates events with specific baseFolders
- `mockEventEnqueuer`: Captures all events for inspection

## Running the Tests

```bash
# Run all GitDestination lifecycle tests
go test -v ./internal/reconcile -run "TestGitDestinationEventStream_"

# Run specific tests
go test -v ./internal/reconcile -run "TestGitDestinationEventStream_(MultipleStreamsWithSharedWorker|DuplicateEventsAcrossStreams)"
```

## Related Documentation

- [Event Flow Architecture](./EVENT_FLOW_ARCHITECTURE.md)
- [GitDestination Worker Implementation Plan](./GITDESTINATION_WORKER_IMPLEMENTATION_PLAN.md)
- [Branch Tracking Analysis](./BRANCH_TRACKING_ANALYSIS.md)

## Key Takeaways

1. **Multiple GitDestinationEventStreams can coexist** on the same branch by using different baseFolders
2. **Events are duplicated intentionally** when multiple streams watch the same resources
3. **Deduplication is stream-scoped** - each stream maintains its own deduplication state
4. **Event convergence** at the BranchWorker ensures serialized commits
5. **Deletion is safe** - removing a stream doesn't break the worker or other streams
6. **Files persist** - Stream deletion does not clean up Git files automatically