# GitDestination Status: Final Recommendations

**Based on**: Kubernetes best practices + Superorbital article analysis + Current implementation review

## Executive Summary

After analyzing the Superorbital article "Status and Conditions: Explained!" and comparing it with the architect's initial recommendations, we've identified one critical issue and several refinements needed for the GitDestination status design.

**Critical Issue**: The `Progressing` condition name violates Kubernetes best practice by describing a transition rather than a state.

**Overall Assessment**: The architect's analysis was 85% correct. The core architecture is sound and aligns well with Kubernetes patterns.

## Revised Condition Design

### Condition Types

```go
const (
    // TypeReady is the summary condition - check this first for overall health
    // This is the all-encompassing condition that considers all other conditions
    TypeReady = "Ready"
    // True: GitDestination is properly configured and operational
    // False: Configuration error or critical failure preventing operation
    // Unknown: Initial validation in progress
    
    // TypeAvailable indicates repository accessibility
    TypeAvailable = "Available"
    // True: Git repository is accessible and operations can proceed
    // False: Cannot access repository (authentication, network, or Git errors)
    // Unknown: Availability check in progress or not yet performed
    
    // TypeActive indicates worker operational state
    // RENAMED from "Progressing" to follow best practice of describing state, not transition
    TypeActive = "Active"
    // True: BranchWorker is running and can process events
    // False: BranchWorker is stopped or not started
    // Unknown: Worker state cannot be determined
    
    // TypeSynced indicates synchronization state with Git
    TypeSynced = "Synced"
    // True: All events have been successfully pushed to Git
    // False: Events are queued or last push failed
    // Unknown: Sync state cannot be determined
)
```

### Condition Reasons

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

// Available condition reasons (more specific than old "RepositoryUnavailable")
const (
    ReasonAvailable              = "Available"
    ReasonAuthenticationFailed   = "AuthenticationFailed"  // Fix credentials
    ReasonRepositoryNotFound     = "RepositoryNotFound"    // Check URL
    ReasonNetworkError           = "NetworkError"          // Check connectivity
    ReasonGitOperationFailed     = "GitOperationFailed"    // Check Git server
    ReasonChecking               = "Checking"
)

// Active condition reasons (renamed from Progressing reasons)
const (
    ReasonActive                 = "Active"           // Worker running with events
    ReasonIdle                   = "Idle"             // Worker running, no events
    ReasonWorkerNotStarted       = "WorkerNotStarted" // Worker hasn't started yet
    ReasonWorkerStopped          = "WorkerStopped"    // Worker was stopped
)

// Synced condition reasons
const (
    ReasonSynced                 = "Synced"
    ReasonSyncInProgress         = "SyncInProgress"
    ReasonSyncFailed             = "SyncFailed"
    ReasonEventsQueued           = "EventsQueued"
)
```

## Status Structure

```go
type GitDestinationStatus struct {
    // Conditions - source of truth for state (follows Kubernetes patterns)
    // Types: Ready (summary), Available, Active, Synced
    // All conditions use positive polarity (True = good state)
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    
    // ObservedGeneration tracks which spec generation was last reconciled
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    
    // GitStatus contains Git repository metadata
    // Only populated when Available=True
    // +optional
    GitStatus *GitStatus `json:"gitStatus,omitempty"`
    
    // WorkerStatus contains BranchWorker operational state
    // Only populated when Active condition exists
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
    
    // QueuedEvents is the number of events waiting to be processed
    // +optional
    QueuedEvents int `json:"queuedEvents,omitempty"`
    
    // LastPushTime is when we last successfully pushed to Git
    // +optional
    LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`
    
    // LastPushStatus indicates the result of the last push attempt
    // Values: "Success", "Failed", "Pending"
    // +optional
    LastPushStatus string `json:"lastPushStatus,omitempty"`
}
```

## Key Changes from Initial Architect Recommendations

### 1. Condition Naming (CRITICAL)

**Before**:
```go
TypeProgressing = "Progressing"  // ❌ Describes transition
```

**After**:
```go
TypeActive = "Active"  // ✅ Describes state
```

**Rationale**: Superorbital article emphasizes: "Condition type names should always describe the current state of the observed object, never a transition phase."

### 2. Ready as Summary Condition (CLARIFICATION)

**Enhancement**: Explicitly document that `Ready` is the summary condition that considers all others.

```go
// Ready condition logic:
// - True: Configuration valid AND Available=True AND Active=True
// - False: Any critical error (config invalid, conflicts)
// - Unknown: Initial validation or waiting for dependencies
```

### 3. Remove SyncStatus Field (UNCHANGED)

Still removing `SyncStatus` string field - this was correct in original analysis.

**Rationale**: Duplicates information already in conditions (anti-pattern).

### 4. Consistent Positive Polarity (UNCHANGED)

All conditions use positive polarity (True = good) - this was already correct.

## Best Practices Applied

### From Superorbital Article

1. ✅ **Summary condition**: `Ready` is the all-encompassing health check
2. ✅ **State not transition**: All conditions describe states (`Active` not `Progressing`)
3. ✅ **Consistent polarity**: All conditions use positive polarity
4. ✅ **Complement, don't duplicate**: Status fields complement conditions

### From Kubernetes API Conventions

1. ✅ **Standard condition schema**: type, status, reason, message, lastTransitionTime
2. ✅ **Separate status subresource**: Status has separate RBAC permissions
3. ✅ **ObservedGeneration tracking**: Detect stale status
4. ✅ **Machine-readable reasons**: PascalCase, stable identifiers

## Example Status Scenarios

### Scenario 1: Healthy and Active

```yaml
status:
  observedGeneration: 5
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
    message: "GitDestination is operational"
    lastTransitionTime: "2025-11-12T10:00:00Z"
  - type: Available
    status: "True"
    reason: Available
    message: "Git repository is accessible"
    lastTransitionTime: "2025-11-12T10:00:05Z"
  - type: Active
    status: "True"
    reason: Active
    message: "Worker is processing 2 events"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  - type: Synced
    status: "False"
    reason: EventsQueued
    message: "2 events queued for next push"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  gitStatus:
    branchExists: true
    lastCommitSHA: "abc123..."
    lastChecked: "2025-11-12T10:04:55Z"
  workerStatus:
    active: true
    queuedEvents: 2
    lastPushTime: "2025-11-12T10:04:50Z"
    lastPushStatus: "Success"
```

### Scenario 2: Configuration Error

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
  - type: Active
    status: "False"
    reason: WorkerNotStarted
    message: "Worker not started due to configuration error"
  - type: Synced
    status: "Unknown"
    reason: Synced
    message: "Cannot sync with invalid configuration"
  # No gitStatus or workerStatus - not applicable
```

### Scenario 3: Network Error (Temporary)

```yaml
status:
  observedGeneration: 5
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
    message: "Configuration is valid"
    lastTransitionTime: "2025-11-12T10:00:00Z"
  - type: Available
    status: "False"
    reason: NetworkError
    message: "Cannot reach git.example.com: connection timeout"
    lastTransitionTime: "2025-11-12T10:05:00Z"
  - type: Active
    status: "True"
    reason: Active
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
    lastChecked: "2025-11-12T10:04:00Z"  # Stale but shown
  workerStatus:
    active: true
    queuedEvents: 5
    lastPushTime: "2025-11-12T10:04:00Z"
    lastPushStatus: "Failed"
```

## Implementation Guidelines

### 1. Condition Management

```go
// Set Ready as summary condition
func (r *GitDestinationReconciler) updateReadyCondition(
    dest *configbutleraiv1alpha1.GitDestination,
) {
    // Ready is True only when:
    // 1. Configuration is valid
    // 2. Available is True
    // 3. Active is True (or Unknown if worker starting)
    
    available := meta.FindStatusCondition(dest.Status.Conditions, TypeAvailable)
    active := meta.FindStatusCondition(dest.Status.Conditions, TypeActive)
    
    if available != nil && available.Status == metav1.ConditionTrue &&
       active != nil && (active.Status == metav1.ConditionTrue || active.Status == metav1.ConditionUnknown) {
        meta.SetStatusCondition(&dest.Status.Conditions, metav1.Condition{
            Type:    TypeReady,
            Status:  metav1.ConditionTrue,
            Reason:  ReasonReady,
            Message: "GitDestination is operational",
        })
    }
}
```

### 2. Status Field Management

```go
// Only populate gitStatus when Available=True
if meta.IsStatusConditionTrue(dest.Status.Conditions, TypeAvailable) {
    dest.Status.GitStatus = &configbutleraiv1alpha1.GitStatus{
        BranchExists:  branchExists,
        LastCommitSHA: sha,
        LastChecked:   metav1.Now(),
    }
} else {
    // Clear when not available
    dest.Status.GitStatus = nil
}

// Only populate workerStatus when Active condition exists
if meta.FindStatusCondition(dest.Status.Conditions, TypeActive) != nil {
    dest.Status.WorkerStatus = &configbutleraiv1alpha1.WorkerStatus{
        Active:         workerActive,
        QueuedEvents:   queueSize,
        LastPushTime:   lastPush,
        LastPushStatus: pushStatus,
    }
} else {
    dest.Status.WorkerStatus = nil
}
```

### 3. kubectl wait Compatibility

Ensure conditions work with kubectl wait:

```bash
# Wait for configuration to be valid
kubectl wait --for=condition=Ready=true gitdestination/my-dest

# Wait for repository to be accessible
kubectl wait --for=condition=Available=true gitdestination/my-dest

# Wait for worker to be active
kubectl wait --for=condition=Active=true gitdestination/my-dest

# Wait for sync to complete
kubectl wait --for=condition=Synced=true gitdestination/my-dest
```

## Migration Strategy

### Phase 1: Add New Conditions (Non-Breaking)

1. Add `Available`, `Active`, `Synced` condition types
2. Keep existing `Ready` condition
3. Keep existing status fields including `SyncStatus`
4. Controllers set both old and new patterns

### Phase 2: Remove SyncStatus (Breaking)

1. Remove `SyncStatus` field from API
2. Update controllers to only use conditions
3. Update documentation
4. Run `make manifests` to update CRDs

### Phase 3: Restructure Status Fields (Breaking)

1. Move fields into `GitStatus` and `WorkerStatus` structs
2. Update all controllers
3. Update tests
4. Run `make manifests` to update CRDs

## Testing Requirements

### Unit Tests

```go
func TestConditionNaming(t *testing.T) {
    // Verify Active (not Progressing) is used
    assert.Equal(t, "Active", TypeActive)
}

func TestReadyAsSummary(t *testing.T) {
    // Verify Ready considers other conditions
    dest := &GitDestination{
        Status: GitDestinationStatus{
            Conditions: []metav1.Condition{
                {Type: TypeAvailable, Status: metav1.ConditionTrue},
                {Type: TypeActive, Status: metav1.ConditionTrue},
            },
        },
    }
    
    updateReadyCondition(dest)
    
    ready := meta.FindStatusCondition(dest.Status.Conditions, TypeReady)
    assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

func TestKubectlWaitCompatibility(t *testing.T) {
    // Verify conditions work with kubectl wait
    // Test that condition transitions are detected
}
```

### Integration Tests

- Verify condition transitions during reconciliation
- Verify status fields cleared when conditions become False
- Verify ObservedGeneration tracking
- Verify kubectl wait works correctly

### E2E Tests

- Verify kubectl output shows correct conditions
- Verify monitoring can distinguish error types
- Verify status updates in real-time

## Documentation Updates

### API Documentation

```go
// GitDestinationStatus follows Kubernetes condition best practices:
//
// Condition Design Principles:
// 1. Ready is the summary condition - check this first for overall health
// 2. All conditions use positive polarity (True = good state)
// 3. Condition types describe states, not transitions (Active not Progressing)
// 4. Conditions complement (not replace) resource-specific status fields
//
// Condition Types:
// - Ready: Overall health (summary of all conditions)
// - Available: Can we access the Git repository?
// - Active: Is the BranchWorker running?
// - Synced: Are all changes pushed to Git?
//
// For automation, use kubectl wait:
//   kubectl wait --for=condition=Ready=true gitdestination/my-dest
```

### Developer Guide

Add section on condition best practices:
- How to add new conditions
- When to use True/False/Unknown
- How to write good reason and message values
- How conditions relate to status fields

## Summary of Changes

### Breaking Changes

1. ❌ Remove `SyncStatus` field
2. ✅ Add `GitStatus` and `WorkerStatus` structs
3. ✅ Rename `Progressing` to `Active`

### Non-Breaking Additions

1. ✅ Add `Available`, `Active`, `Synced` conditions
2. ✅ Add specific error reasons (AuthenticationFailed, NetworkError, etc.)
3. ✅ Add WorkerStatus fields for operational visibility

### Timeline

- Phase 1 (Add new conditions): 2-3 hours
- Phase 2 (Remove SyncStatus): 1 hour  
- Phase 3 (Restructure fields): 2-3 hours
- Testing: 3-4 hours
- Documentation: 1 hour

**Total**: ~10-12 hours

Can be done in parallel with branch tracking refactor (~9 hours), so combined timeline is ~12-15 hours total.

## References

- Superorbital Article: https://superorbital.io/blog/status-and-conditions/
- Kubernetes API Conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- Condition Schema: https://github.com/kubernetes/apimachinery/blob/release-1.23/pkg/apis/meta/v1/types.go#L1433-L1493
- Branch Tracking Plan: [`docs/BRANCH_TRACKING_REFACTOR_PLAN.md`](BRANCH_TRACKING_REFACTOR_PLAN.md)
- Superorbital Analysis: [`docs/SUPERORBITAL_ARTICLE_ANALYSIS.md`](SUPERORBITAL_ARTICLE_ANALYSIS.md)