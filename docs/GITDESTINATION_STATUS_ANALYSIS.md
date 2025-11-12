# GitDestination Status and Condition Analysis

## Current Status Structure

### Status Fields

```go
type GitDestinationStatus struct {
    Conditions         []metav1.Condition  // Standard K8s conditions
    ObservedGeneration int64               // Reconciliation tracking
    LastCommitSHA      string              // Git metadata
    BranchExists       bool                // Git metadata
    LastSyncTime       *metav1.Time        // Sync tracking
    SyncStatus         string              // "idle", "syncing", "error"
}
```

### Current Condition Reasons

| Reason | Status | Usage | Issues |
|--------|--------|-------|--------|
| `Validating` | Unknown | Initial reconciliation state | ✅ Good |
| `GitRepoConfigNotFound` | False | Referenced GitRepoConfig doesn't exist | ✅ Good |
| `BranchNotAllowed` | False | Branch doesn't match allowedBranches patterns | ✅ Good |
| `RepositoryUnavailable` | False | Cannot connect to Git repository | ⚠️ Too broad |
| `Conflict` | False | Another GitDestination uses same repo+branch+baseFolder | ✅ Good |
| `Ready` | True | GitDestination is valid and operational | ✅ Good |

## Problems Identified

### Problem 1: `SyncStatus` Field is Ambiguous

**Current Values**: `"idle"`, `"syncing"`, `"error"`, `"initializing"`, `""` (empty)

**Issues**:
1. **String-based enum** - No type safety, easy to typo
2. **Overlaps with Condition** - Duplicates information already in Ready condition
3. **Unclear semantics** - What's the difference between `"error"` and `Ready=False`?
4. **Not used consistently** - Sometimes set, sometimes cleared, sometimes empty

**Example Confusion**:
```go
// Line 378: Set to "syncing" before status check
dest.Status.SyncStatus = "syncing"

// Line 384: Set to "error" on auth failure
dest.Status.SyncStatus = "error"

// Line 408: Set to "idle" on success
dest.Status.SyncStatus = "idle"

// Line 218: Cleared on branch not allowed
dest.Status.SyncStatus = ""
```

### Problem 2: `RepositoryUnavailable` Reason is Too Broad

**Current Usage**:
- Authentication failures
- Network connectivity issues
- Git operation failures
- Branch status check failures

**Problem**: Users can't distinguish between:
- "My credentials are wrong" (fixable by user)
- "The Git server is down" (wait and retry)
- "The repository doesn't exist" (configuration error)

### Problem 3: Status Fields Cleared Inconsistently

**Security Requirement** (lines 214-218):
```go
// Security requirement: Clear BranchExists and LastCommitSHA when branch not allowed
dest.Status.BranchExists = false
dest.Status.LastCommitSHA = ""
dest.Status.LastSyncTime = nil
dest.Status.SyncStatus = ""
```

**Issue**: This is the ONLY place where these fields are cleared. Other error conditions leave stale data.

### Problem 4: Missing Worker State Information

**Current Gap**: No visibility into:
- Is the BranchWorker running?
- How many events are queued?
- When was the last successful push?
- Are there any push failures?

## Recommendations

### Recommendation 1: Remove `SyncStatus` Field ⭐ HIGH PRIORITY

**Rationale**: Redundant with Condition system, adds confusion

**Migration**:
```go
// REMOVE from GitDestinationStatus:
SyncStatus string `json:"syncStatus,omitempty"`

// Information is already captured in:
// - Ready condition Status (True/False/Unknown)
// - Ready condition Reason (specific error type)
// - LastSyncTime (when last operation completed)
```

**Impact**: Breaking change to API, but you said no users yet ✅

### Recommendation 2: Add More Specific Condition Reasons

**Replace** `RepositoryUnavailable` with:

| New Reason | When to Use | User Action |
|------------|-------------|-------------|
| `AuthenticationFailed` | Secret missing, malformed, or credentials rejected | Fix credentials |
| `RepositoryNotFound` | Git repository doesn't exist at URL | Check repository URL |
| `NetworkError` | Cannot reach Git server | Check network/firewall |
| `GitOperationFailed` | Git command failed (clone, fetch, etc.) | Check Git server logs |

**Example**:
```go
const (
    GitDestinationReasonValidating            = "Validating"
    GitDestinationReasonReady                 = "Ready"
    
    // Configuration errors (user fixable)
    GitDestinationReasonGitRepoConfigNotFound = "GitRepoConfigNotFound"
    GitDestinationReasonBranchNotAllowed      = "BranchNotAllowed"
    GitDestinationReasonConflict              = "Conflict"
    
    // Repository access errors (specific)
    GitDestinationReasonAuthenticationFailed  = "AuthenticationFailed"
    GitDestinationReasonRepositoryNotFound    = "RepositoryNotFound"
    GitDestinationReasonNetworkError          = "NetworkError"
    GitDestinationReasonGitOperationFailed    = "GitOperationFailed"
    
    // Worker state
    GitDestinationReasonWorkerNotReady        = "WorkerNotReady"
)
```

### Recommendation 3: Add Worker Status Fields

**Add to GitDestinationStatus**:
```go
type GitDestinationStatus struct {
    // ... existing fields ...
    
    // WorkerStatus provides visibility into the BranchWorker state
    // +optional
    WorkerStatus *WorkerStatus `json:"workerStatus,omitempty"`
}

type WorkerStatus struct {
    // Active indicates if the BranchWorker is running
    Active bool `json:"active"`
    
    // QueuedEvents is the number of events waiting to be processed
    // +optional
    QueuedEvents int `json:"queuedEvents,omitempty"`
    
    // LastPushTime is when the worker last successfully pushed to Git
    // +optional
    LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`
    
    // LastPushError contains the error from the most recent push failure
    // +optional
    LastPushError string `json:"lastPushError,omitempty"`
}
```

**Benefits**:
- Operators can see if worker is stuck
- Debugging is much easier
- Can detect queue backlog issues

### Recommendation 4: Consistent Status Field Management

**Rule**: Status fields should be:
1. **Set** when information is available
2. **Cleared** when information becomes invalid
3. **Never left stale**

**Implementation**:
```go
// Helper function to clear all status fields
func (r *GitDestinationReconciler) clearStatusFields(dest *configbutleraiv1alpha1.GitDestination) {
    dest.Status.BranchExists = false
    dest.Status.LastCommitSHA = ""
    dest.Status.LastSyncTime = nil
    dest.Status.WorkerStatus = nil
}

// Call this whenever Ready becomes False
func (r *GitDestinationReconciler) setConditionFailed(
    dest *configbutleraiv1alpha1.GitDestination,
    reason, message string,
) {
    r.setCondition(dest, metav1.ConditionFalse, reason, message)
    r.clearStatusFields(dest)  // Always clear on failure
}
```

### Recommendation 5: Add Status Subresource Conditions

**Add additional condition types** for better observability:

```go
const (
    ConditionTypeReady          = "Ready"           // Existing
    ConditionTypeWorkerActive   = "WorkerActive"    // NEW: Is BranchWorker running?
    ConditionTypeSynced         = "Synced"          // NEW: Is Git state up-to-date?
)
```

**Example**:
```yaml
status:
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
    message: "GitDestination is valid and operational"
  - type: WorkerActive
    status: "True"
    reason: WorkerRunning
    message: "BranchWorker is processing events"
  - type: Synced
    status: "True"
    reason: UpToDate
    message: "Last sync completed 2m ago"
    lastTransitionTime: "2025-11-12T12:00:00Z"
```

## Proposed New Status Structure

```go
type GitDestinationStatus struct {
    // Conditions represent the latest available observations
    // Types: Ready, WorkerActive, Synced
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    
    // ObservedGeneration tracks reconciliation
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    
    // Git metadata (from BranchWorker cache)
    LastCommitSHA string      `json:"lastCommitSHA,omitempty"`
    BranchExists  bool        `json:"branchExists,omitempty"`
    LastSyncTime  *metav1.Time `json:"lastSyncTime,omitempty"`
    
    // Worker state (NEW)
    WorkerStatus *WorkerStatus `json:"workerStatus,omitempty"`
}

type WorkerStatus struct {
    Active        bool         `json:"active"`
    QueuedEvents  int          `json:"queuedEvents,omitempty"`
    LastPushTime  *metav1.Time `json:"lastPushTime,omitempty"`
    LastPushError string       `json:"lastPushError,omitempty"`
}
```

## Implementation Priority

### Phase 1: Critical Fixes (Do with branch tracking refactor)
1. ✅ Remove `SyncStatus` field
2. ✅ Split `RepositoryUnavailable` into specific reasons
3. ✅ Implement consistent status field clearing
4. ✅ Update status from BranchWorker cache (already in plan)

### Phase 2: Enhanced Observability (Do after Phase 1)
1. Add `WorkerStatus` fields
2. Add `WorkerActive` condition
3. Add `Synced` condition
4. Update BranchWorker to report status back to GitDestination

### Phase 3: Polish (Optional)
1. Add metrics for status transitions
2. Add events for important status changes
3. Add status summary in kubectl output

## Migration Guide

### Breaking Changes

**Removed Field**: `status.syncStatus`

**Before**:
```yaml
status:
  syncStatus: "idle"
  conditions:
  - type: Ready
    status: "True"
```

**After**:
```yaml
status:
  conditions:
  - type: Ready
    status: "True"
    reason: Ready
  - type: Synced
    status: "True"
    reason: UpToDate
```

**Migration**: Users checking `syncStatus` should check `Ready` condition instead:
```go
// OLD (deprecated)
if dest.Status.SyncStatus == "idle" { ... }

// NEW
readyCondition := meta.FindStatusCondition(dest.Status.Conditions, "Ready")
if readyCondition != nil && readyCondition.Status == metav1.ConditionTrue { ... }
```

### New Condition Reasons

Users may need to update alerting rules:

**Before**:
```yaml
# Alert on any repository issue
- alert: GitDestinationRepositoryUnavailable
  expr: gitdestination_condition{reason="RepositoryUnavailable"} == 1
```

**After**:
```yaml
# Alert on specific issues
- alert: GitDestinationAuthFailed
  expr: gitdestination_condition{reason="AuthenticationFailed"} == 1
  
- alert: GitDestinationNetworkError
  expr: gitdestination_condition{reason="NetworkError"} == 1
```

## Testing Requirements

### Unit Tests
- [ ] Test all new condition reasons
- [ ] Test status field clearing on all error paths
- [ ] Test WorkerStatus updates
- [ ] Test condition transitions

### Integration Tests
- [ ] Test GitDestination with authentication failure
- [ ] Test GitDestination with network error
- [ ] Test GitDestination with missing repository
- [ ] Test status updates from BranchWorker

### E2E Tests
- [ ] Verify kubectl output shows correct status
- [ ] Verify status updates in real-time
- [ ] Verify worker status reflects reality

## Summary

**Key Changes**:
1. ❌ Remove `SyncStatus` - redundant and confusing
2. ✅ Add specific error reasons - better debugging
3. ✅ Add `WorkerStatus` - operational visibility
4. ✅ Consistent field clearing - no stale data
5. ✅ Multiple condition types - richer status model

**Benefits**:
- Clearer error messages for users
- Better operational visibility
- Easier debugging
- More idiomatic Kubernetes API
- No stale status data

**Timeline**: Can be done in parallel with branch tracking refactor (Phase 1 changes overlap)