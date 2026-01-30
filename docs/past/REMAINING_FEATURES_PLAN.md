# GitOps Reverser - Remaining Features Implementation Plan

## Overview

This document outlines the implementation plan for the remaining features in the GitOps Reverser project:

1. **Wildcard Support for allowedBranches** - Allow patterns like `feat/*`, `release/*` in GitRepoConfig
2. **GitDestination Controller Improvements** - Move branch validation and add status tracking

## 1. Wildcard Support for allowedBranches

### Current State
- GitRepoConfig currently accepts a list of exact branch names in `allowedBranches`
- No pattern matching or wildcards supported

### Industry Best Practice Approach (Updated)

Following GitHub/GitLab branch protection patterns, we'll treat each string in `allowedBranches` as a potential glob pattern rather than adding a separate field.

#### API Changes (Minimal)
- **No new fields added** - Keep existing `allowedBranches` array
- **Update field description** to document glob support
- **Backward compatible** - exact names like "main" still work as valid globs

#### Pattern Matching Logic
- Support glob-style patterns: `main`, `feature/*`, `release/v*`, `*`
- Use Go's built-in `filepath.Match` for globbing
- **Branch is allowed if ANY pattern in the array matches** (OR logic)
- Invalid patterns are logged as warnings but don't prevent validation

#### Implementation Steps
1. **Update CRD description**: Document that `allowedBranches` supports glob patterns
2. **Branch matching logic**: Replace exact string matching with `filepath.Match`
3. **Pattern validation**: Add validation webhook for malformed glob patterns
4. **Error handling**: Log warnings for invalid patterns but don't fail validation
5. **Documentation**: Update examples to show pattern usage

#### Example Usage (Updated)
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: my-repo
spec:
  repoURL: "https://github.com/myorg/myrepo.git"
  allowedBranches: ["main", "develop", "feature/*", "release/v*"]  # Mix of exact and patterns
  secretRef:
    name: git-creds
```

**Branch Matching Examples:**
- Branch `main` → matches `"main"` pattern ✅
- Branch `develop` → matches `"develop"` pattern ✅
- Branch `feature/login` → matches `"feature/*"` pattern ✅
- Branch `release/v1.2` → matches `"release/v*"` pattern ✅
- Branch `hotfix/urgent` → matches NO patterns ❌ (blocked)

#### Code Changes
```go
// OLD: Exact match
isAllowed := false
for _, allowedBranch := range gitRepoConfig.Spec.AllowedBranches {
    if gitDestination.Spec.Branch == allowedBranch {
        isAllowed = true
        break
    }
}

// NEW: Glob match
isAllowed := false
for _, pattern := range gitRepoConfig.Spec.AllowedBranches {
    if match, err := filepath.Match(pattern, gitDestination.Spec.Branch); match {
        isAllowed = true
        break
    } else if err != nil {
        // Log malformed pattern but continue checking other patterns
        log.Warn("Invalid glob pattern in allowedBranches", "pattern", pattern, "error", err)
    }
}
```

## 2. GitDestination Controller Improvements

### Current State
- GitDestination is a simple CRD that defines (repo, branch, baseFolder) tuples
- **Uniqueness Requirement**: The combination of (GitRepoConfig reference, branch, baseFolder) must be unique across ALL namespaces
- No status tracking or validation
- Branch validation was previously in GitRepoConfig

### Git Repository Management Strategy

#### Repository Instance Management
- **One git clone per GitDestination**: Each GitDestination maintains its own local git repository clone
- **Shared by all operations**: The same git instance is used for both reading (validation) and writing (commits)
- **Regular fetching**: Repository is fetched on every GitDestination reconcile (every 5 minutes) to stay current
- **Isolated storage**: Each GitDestination uses a unique local path to prevent conflicts

#### Repository Lifecycle
```
GitDestination Creation:
├── Clone repository (if not exists)
├── Check if branch exists in remote
├── If branch exists: checkout and validate against patterns
├── If branch doesn't exist: set BranchExists=false, record HEAD SHA as LastCommitSHA
└── Update status accordingly

Regular Reconciliation (every 5min):
├── Fetch latest changes from remote
├── If branch exists: validate it still matches patterns, update SHA
├── If branch doesn't exist: keep BranchExists=false, update HEAD SHA
└── Check for branch creation/deletion changes

Git Operations (when events arrive):
├── If branch doesn't exist yet: create it from current HEAD
├── Use existing local clone (no new clone needed)
├── Perform commits/pushes using same instance
├── Update BranchExists=true and LastCommitSHA after first push
└── Repository stays available for next operations
```

#### Branch Creation Behavior
- **Lazy Branch Creation**: Branches specified in GitDestination that don't exist are created on-demand when the first event needs to be written
- **Base Branch**: New branches are created from the repository's current HEAD (no explicit base branch configuration needed)
- **Status Tracking**: `BranchExists=false` until first event creates the branch, then `LastCommitSHA` tracks the branch's commits

### Proposed Implementation

#### Status Tracking
**Current GitDestination Status:**
```go
type GitDestinationStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

**Proposed Enhanced GitDestination Status:**
```go
type GitDestinationStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // New fields for GitDestination controller
    LastCommitSHA string `json:"lastCommitSHA,omitempty"` // SHA of branch HEAD, or repo HEAD if branch doesn't exist
    BranchExists bool `json:"branchExists,omitempty"` // True if branch exists on remote, false until first event creates it
    LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
    SyncStatus string `json:"syncStatus,omitempty"` // "idle", "syncing", "error"
}
```

**Current GitRepoConfig Status:**
```go
type GitRepoConfigStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

**Updated GitRepoConfig Status (simplified):**
```go
type GitRepoConfigStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    // No additional fields needed - focus on lightweight connectivity validation
}
```

#### Controller Logic
1. **Branch Validation**: Validate branch against GitRepoConfig patterns (branch may not exist yet)
2. **Lazy Branch Creation**: Create branches on-demand when first event arrives, based on current HEAD
3. **SHA Tracking**: Track latest commit SHA - shows HEAD SHA when branch doesn't exist, branch SHA after creation
4. **Repository Management**: Maintain persistent git clone with regular fetching
5. **Status Updates**: Update status with validation results, branch existence, and sync state
6. **Event Handling**: Handle branch creation on first write, update status accordingly

#### Expected Status Conditions

Following the [Kubernetes status and conditions best practices](https://superorbital.io/blog/status-and-conditions/), we'll use a simplified condition structure that avoids redundancy and follows the principle that **conditions should represent the current state, not historical events**.

**GitDestination Controller Conditions:**

**Ready Condition (Primary Status - Required):**
- **Type**: `"Ready"`, **Status**: `"True"` - GitDestination is fully operational (repo accessible, branch valid/configured, no conflicts)
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"GitRepoConfigNotFound"` - Referenced GitRepoConfig doesn't exist
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"BranchNotAllowed"` - Branch doesn't match any allowed pattern in GitRepoConfig
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"RepositoryUnavailable"` - Cannot clone/access repository (network, auth, or other issues)
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"Conflict"` - Another GitDestination has the same (GitRepoConfig, branch, baseFolder) tuple

**Security Consideration - Information Disclosure:**
When `Ready=False` with `Reason="BranchNotAllowed"`, the status fields `BranchExists` and `LastCommitSHA` **MUST NOT** be populated. This prevents information leakage about repository structure to users who don't have permission to access certain branches.

**Implementation Requirement:**
- Controller must clear `BranchExists` and `LastCommitSHA` fields when setting `Ready=False` with `BranchNotAllowed`
- Only populate these fields when the branch is actually allowed by GitRepoConfig patterns
- **Special Security Test Required**: Create a test that verifies unauthorized users cannot discover branch existence or SHA information through status fields

**Note**: We consolidate multiple failure reasons under `Ready=False` rather than separate conditions, following the best practice of having one primary condition that represents overall readiness.

**Additional Status Fields (Not Conditions):**
- `LastCommitSHA`: Current commit SHA (HEAD SHA if branch doesn't exist, branch SHA if it does)
- `BranchExists`: Boolean indicating if branch exists on remote
- `LastSyncTime`: Timestamp of last successful sync with remote
- `SyncStatus`: Current sync state ("idle", "syncing", "error") - **Not a condition, just status info**

**GitRepoConfig Controller Conditions (Simplified):**
- **Type**: `"Ready"`, **Status**: `"True"` - Repository connectivity validated
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"ConnectionFailed"` - Cannot connect to repository
- **Type**: `"Ready"`, **Status**: `"False"`, **Reason**: `"AuthFailed"` - Authentication failed

#### BranchWorker Uniqueness and Status Sharing

**Uniqueness Constraints (Critical Requirements):**

**BranchWorker Uniqueness (Internal):**
- Each `(repoURL, branch)` combination must be managed by exactly **one BranchWorker**
- Multiple GitDestinations can share the same BranchWorker **only if they have different `baseFolder` values**
- This prevents merge conflicts and ensures serialized commits per branch
- **Enforced internally by gitops-reverser** (not exposed as user-facing error)

**GitDestination Tuple Uniqueness (User-Facing):**
- The combination of `(GitRepoConfig reference, branch, baseFolder)` must be unique across **ALL namespaces**
- This prevents conflicts in file system layout and Git operations
- **Enforced by admission webhook AND reconciliation loop validation**
- **User-facing error** when violated: `Ready=False` with `Reason="Conflict"`

**Defense in Depth Strategy:**

**🚪 Validating Webhook (The Gatekeeper - Prevents):**
- **When**: Before object creation/update in etcd (synchronous, blocking)
- **Purpose**: Fast-fail check preventing conflicting CRs
- **Behavior**: Queries API for existing CRs, rejects if `(repoURL, branch)` already taken
- **UX**: Immediate error on `kubectl apply` with clear message
- **Timing Protection**: API server serializes requests, preventing race conditions

**🩺 CRD Status (The Reconciler - Reports & Corrects):**
- **When**: After object in etcd, during reconciliation loop (asynchronous)
- **Purpose**: Ultimate source of truth, handles split-brain scenarios
- **Behavior**: Detects conflicts, elects winner by `creationTimestamp`, updates status accordingly
- **Conflict Resolution**: Winner gets `Ready=True`, loser gets `Ready=False` with `Conflict` reason and reference to winning CR

**Status Sharing Strategy:**
- All GitDestinations sharing the same BranchWorker show **identical** `LastCommitSHA`, `BranchExists`, `LastSyncTime`, and `SyncStatus` values
- Individual GitDestinations track their own `Ready` condition (validation of their specific config)
- Status updates coordinated through BranchWorker for consistency
- **No `SharedBranchWorker` condition needed** - this is implicit from identical status values across destinations

**Reconciliation Loop Validation:**
- Controller double-checks uniqueness constraint during every reconciliation
- If conflict detected: set `Ready=False` with `BranchWorkerConflict` reason and reference to winning CR
- Prevents timing issues where webhook validation might be bypassed
- Follows Kubernetes best practice of validating constraints in both admission and reconciliation

#### Implementation Steps

##### Phase 2: GitDestination Controller Foundation
1. Add status fields to GitDestination CRD (BranchExists, LastCommitSHA)
2. Create GitDestination controller with reconciliation loop
3. Implement repository cloning and persistent storage per destination
4. Add pattern-based branch validation (allowing non-existent branches)
5. Add status conditions for branch validation and existence

##### Phase 3: Lazy Branch Creation and SHA Tracking
1. Implement lazy branch creation from HEAD on first event
2. Add SHA tracking - HEAD SHA when branch doesn't exist, branch SHA after creation
3. Update BranchExists status after branch creation
4. Add regular repository fetching (every reconcile cycle)
5. Track sync status and last sync time

##### Phase 4: Advanced Branch Management
1. Detect branch deletions and update status accordingly
2. Handle branch creation/deletion race conditions
3. Add alerting for branch validation failures
4. Support for branch protection status checking
5. Implement proper repository cleanup on destination deletion

#### Controller Architecture
```
GitDestination Controller
├── Uniqueness Validator
│   ├── Check (GitRepoConfig, branch, baseFolder) tuple uniqueness across ALL namespaces
│   ├── Prevent duplicate GitDestination configurations
│   ├── Allow multiple destinations with different baseFolders on same repo+branch
│   └── Set Conflict condition on tuple violations
├── Repository Manager
│   ├── Clone and maintain local git instance per BranchWorker (not per destination)
│   ├── Regular fetching (every 5 minutes)
│   ├── Branch checkout and lazy creation
│   └── Repository cleanup on last destination deletion
├── Branch Validator
│   ├── Pattern matching against GitRepoConfig allowedBranches
│   ├── Remote branch existence validation
│   └── Branch change detection
├── SHA Tracker
│   ├── Latest commit SHA monitoring (shared across destinations in same BranchWorker)
│   ├── SHA updates after successful pushes
│   └── SHA validation and drift detection
└── Status Manager
    ├── Ready condition updates for validation results
    ├── Status field updates (SHA, BranchExists, LastSyncTime, SyncStatus)
    ├── Coordinated status sharing across destinations in same BranchWorker
    ├── Metrics emission
    └── Event generation for status changes
```

## 3. Integration and Testing

### Unit Tests
- Pattern matching logic tests
- Branch validation tests
- Status update tests
- **Security test**: Verify BranchExists and LastCommitSHA are NOT populated when branch is not allowed by GitRepoConfig

### Integration Tests
- End-to-end wildcard pattern validation
- GitDestination status tracking
- Branch change detection

### E2E Tests
- Full workflow with wildcard patterns
- GitDestination status verification
- Branch lifecycle management

## 4. Migration Strategy

### Backward Compatibility
- Existing `allowedBranches` configurations continue to work unchanged
- Exact branch names like "main" are still valid (they match themselves as globs)
- No breaking changes - this is purely additive functionality

### Gradual Rollout
1. Deploy wildcard support (no API changes needed)
2. Deploy GitDestination controller alongside existing system
3. Add feature flags for new functionality if needed
4. Update documentation and examples

## 5. Success Criteria

### Wildcard Support
- ✅ Users can specify patterns like `main`, `feat/*`, `release/v*`
- ✅ Branch is allowed if ANY pattern matches (OR logic)
- ✅ Invalid patterns are logged but don't break validation
- ✅ Documentation includes examples and behavior explanation

### GitDestination Improvements
- ✅ One persistent git clone per GitDestination
- ✅ Repository fetched regularly (every reconcile cycle ~5min)
- ✅ Lazy branch creation from HEAD on first event
- ✅ SHA shows HEAD when branch doesn't exist, branch SHA after creation
- ✅ BranchExists status tracks actual branch existence
- ✅ Status shows branch existence, SHA, and sync state
- ✅ Branch changes detected and status updated

### Repository Management
- ✅ Same git instance used for reading and writing operations
- ✅ No redundant cloning during git operations
- ✅ Repository stays synchronized with remote
- ✅ Proper cleanup on GitDestination deletion

### Overall
- ✅ All existing functionality preserved
- ✅ New features are additive and optional
- ✅ Performance impact is minimal
- ✅ Testing coverage >90%

## 6. Implementation Phases

### Phase 1: Wildcard Support
1. Update CRD field description for glob pattern support
2. Implement `filepath.Match` pattern matching in branch validation
3. Add validation for malformed glob patterns with warning logs
4. Update documentation and examples
5. Add comprehensive unit tests for pattern matching

### Phase 2: GitDestination Controller Foundation
1. Add status fields to GitDestination CRD (BranchExists, LastCommitSHA, LastSyncTime, SyncStatus)
2. Create GitDestination controller with reconciliation loop
3. Implement repository cloning and persistent storage per BranchWorker
4. Add pattern-based branch validation with security considerations
5. **Implement security requirement**: Clear BranchExists/LastCommitSHA when branch not allowed
6. Add Ready condition with proper failure reasons

### Phase 3: Advanced GitDestination Features
1. Implement regular repository fetching (every 5 minutes)
2. Add SHA updates after successful git operations/pushes
3. Track sync status and last sync time
4. Add branch change detection and status updates
5. Implement proper repository cleanup on deletion

### Phase 4: Monitoring and Observability
1. Add metrics for sync operations and repository health
2. Implement alerting for branch validation failures
3. Add comprehensive logging for git operations
4. Create dashboards for GitDestination status monitoring

## 7. Risk Assessment

### Low Risk
- Wildcard pattern matching (additive feature, no API changes)
- Status field additions (backward compatible)

### Medium Risk
- GitDestination controller logic (need thorough testing)
- Repository management and cleanup (resource management)
- Remote branch validation (network dependencies)

### High Risk
- SHA tracking accuracy during concurrent operations
- Repository corruption from failed operations

### Mitigation Strategies
- Comprehensive unit and integration tests
- Feature flags for new functionality
- Gradual rollout with monitoring
- Rollback procedures documented
- Repository backup/validation mechanisms