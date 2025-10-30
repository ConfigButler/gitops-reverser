# Event Flow Architecture: Understanding the New Sync Mechanism

## Overview

This document explains the new event-based architecture implemented in the GitOps Reverser sync refactor. The key insight is the separation between **real-time cluster events** (what changed) and **reconciliation events** (what should be done to make Git match cluster state).

## The "Upsert" Strategy: Why UPDATE Events for Everything

### Traditional Approach (Before)
```
Cluster Event → Specific Operation Type
├── Resource Created → CREATE event
├── Resource Updated → UPDATE event
└── Resource Deleted → DELETE event
```

### New Approach (After): "Upsert" Strategy
```
Any Cluster Change → UPDATE Event
├── Resource Created → UPDATE event (new resource appeared)
├── Resource Updated → UPDATE event (existing resource changed)
└── Resource Deleted → UPDATE event (resource disappeared)
```

### Why This Works Better

1. **Simplified Event Types**: One event type handles all cluster state changes
2. **Consistent Processing**: All cluster events follow the same routing and processing path
3. **Clear Separation**: Real-time events vs reconciliation events are distinctly different
4. **Race Condition Safety**: Time-based filtering prevents conflicts between real-time and reconciliation events

## Event Types and Their Purposes

### Real-Time Events (From Cluster Changes)

#### `UPDATE` Event
- **Source**: Kubernetes watch events (create, update, delete)
- **Purpose**: "Something changed in the cluster - react immediately"
- **Routing**: WatchManager → EventRouter → BranchWorker
- **Action**: Immediate Git commit with current cluster state
- **Example**: Pod created → UPDATE event → BranchWorker commits pod YAML to Git

### Reconciliation Events (From State Comparison)

#### `CREATE` Event
- **Source**: BaseFolderReconciler reconciliation
- **Purpose**: "This resource exists in cluster but not in Git - add it"
- **Routing**: BaseFolderReconciler → WatchManager (with time filtering) → EventRouter → BranchWorker
- **Action**: Add missing file to Git repository

#### `DELETE` Event
- **Source**: BaseFolderReconciler reconciliation
- **Purpose**: "This resource exists in Git but not in cluster - remove it"
- **Routing**: BaseFolderReconciler → WatchManager (with time filtering) → EventRouter → BranchWorker
- **Action**: Remove orphaned file from Git repository

#### `RECONCILE_RESOURCE` Event
- **Source**: BaseFolderReconciler reconciliation
- **Purpose**: "This resource exists in both - check if Git version matches cluster version"
- **Routing**: BaseFolderReconciler → WatchManager → EventRouter → BranchWorker
- **Action**: BranchWorker can ignore if resource hasn't actually changed

## Complete Event Flow Examples

### Scenario 1: New Resource Created in Cluster

```
1. User creates ConfigMap in Kubernetes
2. WatchManager detects creation via informer
3. WatchManager creates UPDATE event (upsert strategy)
4. EventRouter routes to appropriate BranchWorker
5. BranchWorker commits ConfigMap YAML to Git
   → Commit message: "[UPDATE] v1/configmaps/my-config by user/admin"
```

### Scenario 2: Resource Deleted from Cluster (Real-Time)

```
1. User deletes ConfigMap from Kubernetes
2. WatchManager detects deletion via informer
3. WatchManager creates UPDATE event (upsert strategy)
4. EventRouter routes to appropriate BranchWorker
5. BranchWorker commits ConfigMap deletion from Git
   → Commit message: "[UPDATE] v1/configmaps/my-config by user/admin"
```

### Scenario 3: Reconciliation Discovers Missing Git File

```
1. BaseFolderReconciler periodically compares cluster vs Git state
2. Discovers: ConfigMap exists in cluster but YAML file missing from Git
3. BaseFolderReconciler emits CREATE event
4. WatchManager applies time filtering (prevents conflicts with real-time events)
5. EventRouter routes filtered CREATE event to BranchWorker
6. BranchWorker adds ConfigMap YAML file to Git
   → Commit message: "[CREATE] v1/configmaps/my-config by system/reconciler"
```

### Scenario 4: Reconciliation Discovers Orphaned Git File

```
1. BaseFolderReconciler periodically compares cluster vs Git state
2. Discovers: YAML file exists in Git but ConfigMap deleted from cluster
3. BaseFolderReconciler emits DELETE event
4. WatchManager applies time filtering (prevents conflicts with real-time events)
5. EventRouter routes filtered DELETE event to BranchWorker
6. BranchWorker removes orphaned YAML file from Git
   → Commit message: "[DELETE] v1/configmaps/old-config by system/reconciler"
```

## How to Distinguish Operations from Git's Perspective

### Method 1: Commit Message Analysis

The commit message clearly indicates the operation type:
- `"[UPDATE] v1/configmaps/my-config by user/admin"` → Real-time cluster change
- `"[CREATE] v1/configmaps/my-config by system/reconciler"` → Reconciliation addition
- `"[DELETE] v1/configmaps/my-config by system/reconciler"` → Reconciliation removal

### Method 2: Event Metadata

Each event contains:
- `Operation`: The type (UPDATE, CREATE, DELETE, RECONCILE_RESOURCE)
- `UserInfo.Username`: Who triggered the change
- `Timestamp`: When the event occurred

### Method 3: Git History Analysis

By examining the Git history:
- **File added to Git**: CREATE operation (file didn't exist before)
- **File modified in Git**: UPDATE operation (file existed, content changed)
- **File removed from Git**: DELETE operation (file existed, now gone)

### Method 4: Reconciliation Logic Inspection

The BaseFolderReconciler's `findDifferences()` method provides clear logic:
```go
func (r *BaseFolderReconciler) findDifferences(cluster, git []ResourceIdentifier) (toCreate, toDelete, existing []ResourceIdentifier) {
    // toCreate: In cluster but not in Git → CREATE event
    // toDelete: In Git but not in cluster → DELETE event
    // existing: In both → RECONCILE_RESOURCE event
}
```

## Benefits of the Upsert Strategy

### 1. Simplified Event Processing
- Single event type for all cluster changes
- Consistent routing and handling logic
- Reduced complexity in event processing

### 2. Clear Separation of Concerns
- **Real-time events**: Immediate reaction to cluster changes
- **Reconciliation events**: Periodic state synchronization
- **No overlap or confusion** between the two paths

### 3. Race Condition Prevention
- Time-based filtering ensures reconciliation doesn't conflict with real-time events
- Reconciliation events are delayed if recent real-time events exist for the same resource

### 4. Better Observability
- Event audit trail shows both real-time and reconciliation activities
- Commit messages clearly distinguish between user actions and system reconciliation

### 5. Scalability
- Real-time and reconciliation can be scaled independently
- Event-based communication allows for distributed processing

## Migration from Old Architecture

### Before (Mixed Responsibilities)
```
BranchWorker: Process events + List files + Compute orphans + Generate deletes
WatchManager: Delegate orphan detection + Handle time filtering
```

### After (Clean Separation)
```
WatchManager: Process real-time events + Time filtering for reconciliation
BranchWorker: Process events + Report Git state when requested
BaseFolderReconciler: Compare cluster vs Git state + Emit reconciliation events
EventRouter: Route all events between components
```

## Implementation Details

### Event Routing Architecture

```
WatchManager (Real-time Events)
├── Processes Kubernetes watch events
├── Creates UPDATE events for all changes
├── Applies time filtering to reconciliation events
└── Routes events via EventRouter

EventRouter (Central Communication)
├── Routes UPDATE events to BranchWorkers
├── Routes reconciliation events to BranchWorkers
├── Routes state request/response events
└── Manages per-(repo,branch,baseFolder) routing

BranchWorker (Git Operations)
├── Receives UPDATE events → Immediate Git commits
├── Receives reconciliation events → Git operations
├── Responds to state requests with RepoStateEvents
└── Handles all Git repository interactions

BaseFolderReconciler (State Comparison)
├── Requests cluster state from WatchManager
├── Requests Git state from BranchWorker
├── Compares states and emits reconciliation events
└── Pure logic, no time concerns
```

### Time-Based Safety

The WatchManager maintains `lastEventTimes` to prevent conflicts:
- When a real-time UPDATE event is processed, timestamp is recorded
- Reconciliation CREATE/DELETE events are delayed if recent real-time events exist
- This ensures reconciliation doesn't overwrite recent user changes

## Conclusion

The "upsert" strategy using UPDATE events for all real-time cluster changes provides a clean separation between immediate reaction and periodic reconciliation. While it may seem counterintuitive at first, this approach:

1. **Simplifies the architecture** by reducing event types
2. **Prevents race conditions** through time-based filtering
3. **Maintains clear audit trails** through commit messages and event metadata
4. **Enables independent scaling** of real-time and reconciliation components
5. **Provides better observability** of both user actions and system reconciliation

The operation type is still clearly distinguishable through commit messages, event metadata, and Git history analysis, ensuring full traceability of changes from both real-time and reconciliation sources.