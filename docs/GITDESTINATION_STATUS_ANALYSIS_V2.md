# GitDestination Status and Condition Analysis (Updated with K8s Best Practices)

## Kubernetes Status Best Practices

Based on Kubernetes API conventions and community best practices:

### Core Principles

1. **Conditions are the source of truth** - Status fields should be derived from conditions
2. **Multiple condition types** - Use specific types for different aspects (Ready, Available, Progressing)
3. **Status values have meaning**:
   - `True` - The condition is satisfied
   - `False` - The condition is not satisfied
   - `Unknown` - Cannot determine the condition state
4. **Reason is machine-readable** - PascalCase, stable across versions
5. **Message is human-readable** - Can change, provides context
6. **ObservedGeneration tracks reconciliation** - Detect stale status

### Anti-Patterns to Avoid

❌ **Custom status fields that duplicate condition information**
```go
// BAD: Duplicates what conditions already express
SyncStatus string // "idle", "syncing", "error"
```

❌ **String-based enums without type safety**
```go
// BAD: Easy to typo, no validation
Status: "syncing" // vs "Syncing" vs "SYNCING"?
```

❌ **Mixing operational state with configuration validation**
```go
// BAD: Conflates "is it configured correctly?" with "is it working?"
Ready: False, Reason: "Syncing" // Syncing is operational, not a failure
```

✅ **Use multiple condition types for different concerns**
```go
// GOOD: Separate concerns
Conditions:
  - Type: Ready          // Is configuration valid?
  - Type: Available      // Is it operational?
  - Type: Progressing    // Is work happening?
```

## Current GitDestination Status - Detailed Analysis

### Current Implementation

```go
type GitDestinationStatus struct {
    Conditions         []metav1.Condition
    ObservedGeneration int64
    LastCommitSHA      string      // ⚠️ Operational data
    BranchExists       bool        // ⚠️ Operational data
    LastSyncTime       *metav1.Time // ⚠️ Operational data
    SyncStatus         string      // ❌ ANTI-PATTERN
}
```

### Problems Mapped to Best Practices

#### Problem 1: `SyncStatus` is an Anti-Pattern

**Current Code**:
```go
dest.Status.SyncStatus = "syncing"  // Line 378
dest.Status.SyncStatus = "error"    // Line 384
dest.Status.SyncStatus = "idle"     // Line 408
dest.Status.SyncStatus = ""         // Line 218
```

**Why It's Wrong**:
1. ❌ Duplicates condition information
2. ❌ String-based enum (no type safety)
3. ❌ Mixes configuration state with operational state
4. ❌ Not following K8s conventions

**Best Practice Solution**:
```go
// Use condition types instead
Conditions:
  - Type: Ready
    Status: True
    Reason: Ready
  - Type: Progressing
    Status: True
    Reason: Syncing
    Message: "Syncing changes to Git repository"
```

#### Problem 2: Single `Ready` Condition for Multiple Concerns

**Current Usage**:
- Configuration validation (GitRepoConfig exists, branch allowed)
- Authentication (can we access the repo?)
- Network connectivity (can we reach the server?)
- Operational state (is worker running?)

**Why It's Wrong**:
- ❌ Can't distinguish "misconfigured" from "temporarily unavailable"
- ❌ Users can't tell if they need to fix config or just wait
- ❌ Monitoring/alerting can't be specific

**Best Practice Solution**: Use multiple condition types

#### Problem 3: Operational Data in Status Without Conditions

**Current Fields**:
```go
LastCommitSHA string      // What does this mean if empty?
BranchExists  bool        // What if we can't check?
LastSyncTime  *metav1.Time // What if sync failed?
```

**Why It's Wrong**:
- ❌ No condition to indicate if these values are valid
- ❌ Can't tell if empty means "not checked yet" or "doesn't exist"
- ❌ Stale data when errors occur

## Recommended Status Structure (K8s Best Practices)

### Condition Types

Following Kubernetes conventions, use these condition types:

```go
const (
    // Configuration validity
    TypeReady = "Ready"
    // Indicates: Is the GitDestination properly configured?
    // True: Configuration is valid, can be used
    // False: Configuration error (user must fix)
    // Unknown: Validation in progress
    
    // Operational availability  
    TypeAvailable = "Available"
    // Indicates: Can we currently access the Git repository?
    // True: Repository is accessible, operations can proceed
    // False: Repository unavailable (may be temporary)
    // Unknown: Availability check in progress
    
    // Work in progress
    TypeProgressing = "Progressing"
    // Indicates: Is the worker actively processing events?
    // True: Worker is processing events or pushing changes
    // False: Worker is idle (no events to process)
    // Unknown: Worker state unknown
    
    // Synchronization state
    TypeSynced = "Synced"
    // Indicates: Is Git repository in sync with cluster state?
    // True: All events have been pushed to Git
    // False: Events are queued or push failed
    // Unknown: Sync state unknown
)
```

### Condition Reasons (Machine-Readable)

```go
// Ready condition reasons
const (
    ReasonReady                  = "Ready"
    ReasonValidating             = "Validating"
    ReasonGitRepoConfigNotFound  = "GitRepoConfigNotFound"
    ReasonBranchNotAllowed       = "BranchNotAllowed"
    ReasonConflict               = "Conflict"
    ReasonInvalidConfiguration   = "InvalidConfiguration"
)

// Available condition reasons
const (
    ReasonAvailable              = "Available"
    ReasonAuthenticationFailed   = "AuthenticationFailed"
    ReasonRepositoryNotFound     = "RepositoryNotFound"
    ReasonNetworkError           = "NetworkError"
    ReasonGitOperationFailed     = "GitOperationFailed"
    ReasonChecking               = "Checking"
)

// Progressing condition reasons
const (
    ReasonProcessingEvents       = "ProcessingEvents"
    ReasonIdle                   = "Idle"
    ReasonWorkerNotStarted       = "WorkerNotStarted"
    ReasonWorkerStopped          = "WorkerStopped"
)

// Synced condition reasons
const (
    ReasonSynced                 = "Synced"
    ReasonSyncInProgress         = "SyncInProgress"
    ReasonSyncFailed             = "SyncFailed"
    ReasonEventsQueued           = "EventsQueued"
)
```

### Proposed Status Structure

```go
type GitDestinationStatus struct {
    // Conditions - source of truth for state
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    
    // ObservedGeneration - reconciliation tracking
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    
    // Git metadata - only valid when Available=True
    // +optional
    GitStatus *GitStatus `json:"gitStatus,omitempty"`
    
    // Worker state - only valid when Progressing condition exists
    // +optional
    WorkerStatus *WorkerStatus `json:"workerStatus,omitempty"`
}

// GitStatus contains Git repository metadata
type GitStatus struct {
    // BranchExists indicates if the branch exists on remote
    BranchExists bool `json:"branchExists"`
    
    // LastCommitSHA is the SHA of the latest commit
    // Empty if branch doesn't exist yet
    LastCommitSHA string `json:"lastCommitSHA,omitempty"`
    
    // LastChecked is when we last verified this information
    LastChecked metav1.Time `json:"lastChecked"`
}

// WorkerStatus contains BranchWorker operational state
type WorkerStatus struct {
    // Active indicates if the worker is running
    Active bool `json:"active"`
    
    // QueuedEvents is the number of events waiting
    // +optional
    QueuedEvents int `json:"queuedEvents,omitempty"`
    
    // LastPushTime is when we last pushed to Git
    // +optional
    LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`
    
    // LastPushStatus indicates the result of the last push
    // Values: "Success", "Failed", "Pending"
    // +optional
    LastPushStatus string `json:"lastPushStatus,omitempty"`
}
```

## Example Status Scenarios

### Scenario 1: Healthy GitDestination

```yaml
status:
  observedGeneration: 5
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
    message: "GitDestination configuration is valid"
    lastTransitionTime: "2025-11-12T10:00:00Z"
  - type: Available
    status: "True"
    reason: Available
    message: "Git repository is accessible"
    lastTransitionTime: "2025-11-12T10:00:05Z"
  - type: Progressing
    status: "False"
    reason: Idle
    message: "No events to process"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  - type: Synced
    status: "True"
    reason: Synced
    message: "All changes pushed to Git"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  gitStatus:
    branchExists: true
    lastCommitSHA: "abc123..."
    lastChecked: "2025-11-12T10:05:00Z"
  workerStatus:
    active: true
    queuedEvents: 0
    lastPushTime: "2025-11-12T10:04:55Z"
    lastPushStatus: "Success"
```

### Scenario 2: Configuration Error (User Must Fix)

```yaml
status:
  observedGeneration: 3
  conditions:
  - type: Ready
    status: "False"
    reason: BranchNotAllowed
    message: "Branch 'prod' not in allowedBranches list"
    lastTransitionTime: "2025-11-12T10:00:00Z"
  - type: Available
    status: "Unknown"
    reason: Checking
    message: "Waiting for valid configuration"
  - type: Progressing
    status: "False"
    reason: WorkerNotStarted
    message: "Worker not started due to configuration error"
  - type: Synced
    status: "Unknown"
    reason: Synced
    message: "Cannot sync with invalid configuration"
  # No gitStatus or workerStatus - not applicable
```

### Scenario 3: Temporary Network Issue (Will Retry)

```yaml
status:
  observedGeneration: 5
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
    message: "GitDestination configuration is valid"
    lastTransitionTime: "2025-11-12T10:00:00Z"
  - type: Available
    status: "False"
    reason: NetworkError
    message: "Cannot reach git.example.com: connection timeout"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  - type: Progressing
    status: "True"
    reason: ProcessingEvents
    message: "Worker is retrying failed operations"
    lastTransitionTime: "2025-11-12T10:05:01Z"
  - type: Synced
    status: "False"
    reason: SyncFailed
    message: "Last push failed due to network error"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  gitStatus:
    branchExists: true
    lastCommitSHA: "abc123..."
    lastChecked: "2025-11-12T10:04:00Z"  # Stale but still shown
  workerStatus:
    active: true
    queuedEvents: 5
    lastPushTime: "2025-11-12T10:04:00Z"
    lastPushStatus: "Failed"
```

### Scenario 4: Active Syncing

```yaml
status:
  observedGeneration: 5
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
  - type: Available
    status: "True"
    reason: Available
  - type: Progressing
    status: "True"
    reason: ProcessingEvents
    message: "Processing 3 events"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  - type: Synced
    status: "False"
    reason: EventsQueued
    message: "3 events queued for next push"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  gitStatus:
    branchExists: true
    lastCommitSHA: "abc123..."
    lastChecked: "2025-11-12T10:04:55Z"
  workerStatus:
    active: true
    queuedEvents: 3
    lastPushTime: "2025-11-12T10:04:50Z"
    lastPushStatus: "Success"
```

## Implementation Guidelines

### 1. Condition Management

```go
// Helper to set multiple conditions atomically
func (r *GitDestinationReconciler) setConditions(
    dest *configbutleraiv1alpha1.GitDestination,
    conditions ...metav1.Condition,
) {
    for _, condition := range conditions {
        meta.SetStatusCondition(&dest.Status.Conditions, condition)
    }
}

// Example usage
r.setConditions(dest,
    metav1.Condition{
        Type:    TypeReady,
        Status:  metav1.ConditionTrue,
        Reason:  ReasonReady,
        Message: "Configuration is valid",
    },
    metav1.Condition{
        Type:    TypeAvailable,
        Status:  metav1.ConditionTrue,
        Reason:  ReasonAvailable,
        Message: "Repository is accessible",
    },
)
```

### 2. Status Field Management

```go
// Only set gitStatus when Available=True
if meta.IsStatusConditionTrue(dest.Status.Conditions, TypeAvailable) {
    dest.Status.GitStatus = &configbutleraiv1alpha1.GitStatus{
        BranchExists:  branchExists,
        LastCommitSHA: sha,
        LastChecked:   metav1.Now(),
    }
} else {
    // Keep last known state but mark as potentially stale
    // OR clear it entirely - depends on use case
    dest.Status.GitStatus = nil
}
```

### 3. Condition Transitions

```go
// When configuration becomes invalid
r.setConditions(dest,
    metav1.Condition{
        Type:    TypeReady,
        Status:  metav1.ConditionFalse,
        Reason:  ReasonBranchNotAllowed,
        Message: "Branch not in allowedBranches",
    },
    metav1.Condition{
        Type:    TypeAvailable,
        Status:  metav1.ConditionUnknown,
        Reason:  ReasonChecking,
        Message: "Waiting for valid configuration",
    },
)
// Clear operational status
dest.Status.GitStatus = nil
dest.Status.WorkerStatus = nil
```

## Migration Strategy

### Phase 1: Add New Condition Types (Non-Breaking)

1. Add `Available`, `Progressing`, `Synced` condition types
2. Keep existing `Ready` condition
3. Keep existing status fields
4. Controllers set both old and new patterns

### Phase 2: Deprecate `SyncStatus` (Breaking)

1. Remove `SyncStatus` field from API
2. Update controllers to only use conditions
3. Update documentation

### Phase 3: Restructure Status Fields (Breaking)

1. Move `LastCommitSHA`, `BranchExists`, `LastSyncTime` into `GitStatus` struct
2. Add `WorkerStatus` struct
3. Update all controllers

## Testing Requirements

### Unit Tests

```go
func TestConditionTransitions(t *testing.T) {
    tests := []struct{
        name string
        initial []metav1.Condition
        event string
        expected []metav1.Condition
    }{
        {
            name: "ready to unavailable",
            initial: []metav1.Condition{
                {Type: TypeReady, Status: metav1.ConditionTrue},
                {Type: TypeAvailable, Status: metav1.ConditionTrue},
            },
            event: "network_error",
            expected: []metav1.Condition{
                {Type: TypeReady, Status: metav1.ConditionTrue},
                {Type: TypeAvailable, Status: metav1.ConditionFalse, Reason: ReasonNetworkError},
            },
        },
        // ... more test cases
    }
}
```

### Integration Tests

- Verify condition transitions during reconciliation
- Verify status fields are set/cleared correctly
- Verify ObservedGeneration tracking

### E2E Tests

- Verify kubectl output shows correct conditions
- Verify monitoring can distinguish error types
- Verify status updates in real-time

## Summary

### Key Changes

1. ❌ **Remove** `SyncStatus` field - use conditions instead
2. ✅ **Add** multiple condition types (Ready, Available, Progressing, Synced)
3. ✅ **Restructure** status fields into `GitStatus` and `WorkerStatus`
4. ✅ **Use** specific Reason values for better debugging
5. ✅ **Follow** Kubernetes API conventions

### Benefits

- **Clearer semantics**: Each condition type has a specific meaning
- **Better UX**: Users can distinguish config errors from operational issues
- **Easier monitoring**: Specific conditions for specific alerts
- **More idiomatic**: Follows Kubernetes best practices
- **Better debugging**: Rich condition history and reasons

### Timeline

- Phase 1 (Add new conditions): 2-3 hours
- Phase 2 (Remove SyncStatus): 1 hour
- Phase 3 (Restructure fields): 2-3 hours
- Testing: 3-4 hours

**Total**: ~10 hours (can overlap with branch tracking refactor)