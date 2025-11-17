# Git Abstraction Integration Guide

**Version**: 2.0
**Date**: 2025-11-17
**Status**: Implementation Complete, Integration Pending

## Overview

This document provides the complete integration guide for replacing the existing git implementation with the new git abstraction layer. The integration follows a systematic approach to ensure backwards compatibility is maintained during the transition.

## Architectural Vision

**Core Principle:** All Git operations in production code must go through the abstraction layer. Direct use of go-git is prohibited outside of:
1. The abstraction layer itself ([`internal/git/abstraction.go`](../internal/git/abstraction.go:1))
2. Test files for verification purposes

**Rationale:**
- **Encapsulation**: Git complexity is isolated in one place
- **Testability**: Easy to mock abstraction functions in unit tests
- **Consistency**: All empty repo handling, conflict resolution, and error handling follows the same patterns
- **Maintainability**: Changes to Git behavior only require updates in the abstraction layer
- **Safety**: No direct plumbing operations scattered across the codebase

**What this means:**
- ❌ **No** `git.PlainOpen()`, `git.PlainClone()`, `repo.Fetch()`, `repo.Push()`, etc. in production code
- ❌ **No** `plumbing.*` imports outside abstraction layer
- ❌ **No** direct worktree manipulation outside abstraction layer
- ✅ **Only** `git.CheckRepo()`, `git.PrepareBranch()`, `git.WriteEvents()` in production code
- ✅ **Tests** may use go-git directly to verify repository state

**Post-integration file structure:**
```
internal/git/
├── abstraction.go        # ✅ Uses go-git (the ONLY file that should)
├── abstraction_test.go   # ✅ Uses go-git to verify behavior
├── git.go                # ❌ Should NOT use go-git after cleanup
├── branch_worker.go      # ❌ Should NOT use go-git
└── events.go             # ❌ Should NOT use go-git

internal/controller/
└── *.go                  # ❌ Should NOT use go-git

test/e2e/
└── *.go                  # ✅ Can use go-git to verify test results
```

## Prerequisites

- ✅ New abstraction ([`internal/git/abstraction.go`](../internal/git/abstraction.go:1)) is implemented
- ⚠️ Tests for abstraction need verification
- ❌ **NO integration has been done - old code still in use**
- ❌ **BranchWorker still uses old `Clone()`, `Checkout()`, `TryPushCommits()`**
- ❌ **Controller still uses old connectivity check pattern**

## Abstraction API

The abstraction layer provides these functions:

### CheckRepo
```go
func CheckRepo(ctx context.Context, repoURL string, auth transport.AuthMethod) (*RepoInfo, error)
```
Returns `RepoInfo` with:
- `DefaultBranch *BranchInfo` (nil for empty repos)
- `RemoteBranchCount int`

### PrepareBranch
```go
func PrepareBranch(ctx context.Context, repoURL, repoPath, targetBranchName string, auth transport.AuthMethod) (*PullReport, error)
```
- Handles both initial clone and pull/update operations
- Returns `*PullReport` with branch state

### WriteEvents
```go
func WriteEvents(ctx context.Context, repoPath string, events []Event, auth transport.AuthMethod) (*WriteEventsResult, error)
```
- Takes `repoPath` (not repo object)
- Opens repo internally
- Returns `*WriteEventsResult` with commits created, conflict pulls, failures

## Integration Strategy

### Phase 1: BranchWorker Integration
Replace direct git operations in BranchWorker with abstraction calls.

#### 1.1 Update BranchWorker.ensureRepositoryInitialized()

**File**: `internal/git/branch_worker.go`

**Before:**
```go
func (w *BranchWorker) ensureRepositoryInitialized(ctx context.Context) error {
    repoConfig, err := w.getGitRepoConfig(ctx)
    if err != nil {
        return fmt.Errorf("failed to get GitRepoConfig: %w", err)
    }

    auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
    if err != nil {
        return fmt.Errorf("failed to get auth: %w", err)
    }

    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

    repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
    if err != nil {
        return fmt.Errorf("failed to clone repository: %w", err)
    }

    // Try to checkout the branch, but don't fail if it doesn't exist (empty repo case)
    if err := repo.Checkout(w.Branch); err != nil {
        // Check if this is an empty repository (no HEAD yet)
        if _, headErr := repo.Head(); errors.Is(headErr, plumbing.ErrReferenceNotFound) {
            // Empty repository - this is expected, don't treat as error
            w.Log.V(1).Info("Repository is empty, branch checkout skipped")
        } else {
            return fmt.Errorf("failed to checkout branch: %w", err)
        }
    }

    // Update metadata after repository initialization
    if err := w.updateBranchMetadata(repo); err != nil {
        w.Log.V(1).Error(err, "Failed to update branch metadata after initialization")
    }

    return nil
}
```

**After:**
```go
func (w *BranchWorker) ensureRepositoryInitialized(ctx context.Context) error {
    repoConfig, err := w.getGitRepoConfig(ctx)
    if err != nil {
        return fmt.Errorf("failed to get GitRepoConfig: %w", err)
    }

    auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
    if err != nil {
        return fmt.Errorf("failed to get auth: %w", err)
    }

    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

    // Use new PrepareBranch abstraction (note: PascalCase)
    pullReport, err := git.PrepareBranch(ctx, repoConfig.Spec.RepoURL, repoPath, w.Branch, auth)
    if err != nil {
        return fmt.Errorf("failed to prepare repository: %w", err)
    }

    // Update metadata from pull report
    w.updateBranchMetadataFromPullReport(pullReport)

    return nil
}
```


#### 1.2 Update BranchWorker.commitAndPush()

**File**: `internal/git/branch_worker.go`

**Before:**
```go
func (w *BranchWorker) commitAndPush(
    repoConfig *configv1alpha1.GitRepoConfig,
    events []Event,
) {
    // ... existing clone and checkout logic ...

    // Pass branch from worker context directly to git operations
    if err := repo.TryPushCommits(w.ctx, w.Branch, events); err != nil {
        log.Error(err, "Failed to push commits")
        return
    }

    // ... existing success handling ...
}
```

**After:**
```go
func (w *BranchWorker) commitAndPush(
    repoConfig *configv1alpha1.GitRepoConfig,
    events []Event,
) {
    log := w.Log.WithValues("eventCount", len(events))

    auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
    if err != nil {
        log.Error(err, "Failed to get auth")
        return
    }

    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

    // Use new WriteEvents abstraction (note: PascalCase, no branch param)
    result, err := git.WriteEvents(w.ctx, repoPath, events, auth)
    if err != nil {
        log.Error(err, "Failed to write events")
        return
    }

    log.Info("Successfully pushed commits",
        "commitsCreated", result.CommitsCreated,
        "conflictPulls", len(result.ConflictPulls),
        "failures", result.Failures)

    // Update metadata if conflicts were resolved
    if len(result.ConflictPulls) > 0 {
        lastPull := result.ConflictPulls[len(result.ConflictPulls)-1]
        w.updateBranchMetadataFromPullReport(lastPull)
    }

    // ... existing metrics ...
}
```


#### 1.3 Add periodic sync method (REQUIRED for reconciliation)

**File**: `internal/git/branch_worker.go`

**Why this is required:** The reconciliation loop must detect external changes to the Git repository (e.g., manual commits, changes from other tools, branch deletions). Without periodic sync, the worker operates on stale data and cannot reconcile the actual state.

**Add sync method:**
```go
// syncWithRemote fetches latest changes from remote to detect drift.
// This is called periodically by the reconciliation loop to ensure
// the local state matches the remote repository.
func (w *BranchWorker) syncWithRemote(ctx context.Context) (*git.PullReport, error) {
    repoConfig, err := w.getGitRepoConfig(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to get GitRepoConfig: %w", err)
    }

    auth, err := getAuthFromSecret(ctx, w.Client, repoConfig)
    if err != nil {
        return nil, fmt.Errorf("failed to get auth: %w", err)
    }

    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

    // PrepareBranch handles both initial and update cases
    report, err := git.PrepareBranch(ctx, repoConfig.Spec.RepoURL, repoPath, w.Branch, auth)
    if err != nil {
        return nil, fmt.Errorf("failed to sync with remote: %w", err)
    }

    w.updateBranchMetadataFromPullReport(report)
    
    // Log if incoming changes were detected
    if report.IncomingChanges {
        w.Log.Info("Detected remote changes during sync",
            "branch", report.HEAD.ShortName,
            "newSHA", report.HEAD.Sha)
    }
    
    return report, nil
}
```

**Integration with reconciliation:**
The sync method should be called:
1. Periodically (e.g., every 30 seconds) to detect drift
2. Before listing resources in `ListResourcesInBaseFolder()`
3. After failed operations to ensure fresh state

Example reconciliation loop integration:
```go
ticker := time.NewTicker(30 * time.Second)
defer ticker.Stop()

for {
    select {
    case <-ticker.C:
        // Periodic sync to detect external changes
        if report, err := w.syncWithRemote(ctx); err != nil {
            w.Log.Error(err, "Failed to sync with remote")
        } else if report.IncomingChanges {
            // Trigger reconciliation of affected GitDestinations
            w.triggerReconciliation()
        }
    // ... other cases
    }
}
```

#### 1.4 Update BranchWorker Metadata Methods

**File**: `internal/git/branch_worker.go`

**Update metadata handling:**
```go
func (w *BranchWorker) updateBranchMetadataFromPullReport(report *git.PullReport) {
    w.metaMu.Lock()
    defer w.metaMu.Unlock()

    w.branchExists = report.ExistsOnRemote
    w.lastCommitSHA = report.HEAD.Sha
    w.lastFetchTime = time.Now()

    // Log if this was an unborn branch
    if report.HEAD.Unborn {
        w.Log.Info("Branch is unborn (no commits yet)", "branch", report.HEAD.ShortName)
    }
}
```

**Note:** `PullReport` structure:
- `HEAD.Sha` contains the commit hash (empty string if unborn)
- `HEAD.Unborn` flag indicates if branch has no commits yet
- `HEAD.ShortName` is the branch name

### Phase 2: GitRepoConfig Controller Integration

#### 2.1 Update checkRemoteConnectivity()

**File**: `internal/controller/gitrepoconfig_controller.go`

**Before:**
```go
func (r *GitRepoConfigReconciler) checkRemoteConnectivity(
    ctx context.Context, repoURL string, auth transport.AuthMethod,
) (int, error) {
    // ... existing remote.List() logic ...
    return branchCount, nil
}
```

**After:**
```go
func (r *GitRepoConfigReconciler) checkRemoteConnectivity(
    ctx context.Context, repoURL string, auth transport.AuthMethod,
) (int, error) {
    // Use new CheckRepo abstraction (note: PascalCase)
    repoInfo, err := git.CheckRepo(ctx, repoURL, auth)
    if err != nil {
        return 0, err
    }

    return repoInfo.RemoteBranchCount, nil
}
```


### Phase 3: Remove Obsolete Code and Enforce Abstraction

#### Files to Remove

**Remove entire files:**
- `internal/git/status.go` - Obsolete status checking logic
- `internal/git/status_test.go` - Tests for removed status logic

**Remove functions from `internal/git/git.go`:**
- `Clone()` - Replaced by `PrepareBranch()`
- `TryPushCommits()` - Replaced by `WriteEvents()`
- `Checkout()` - Logic moved to abstraction
- `GetBranchStatus()` - Replaced by `CheckRepo()`

**Remove functions from `internal/git/branch_worker.go`:**
- `updateBranchMetadata(repo *Repo)` - Replaced with report-based updates
- Direct git plumbing calls in `ensureRepositoryInitialized()`

#### Enforce Abstraction Boundary

**Critical**: After integration, verify no go-git usage outside abstraction:

```bash
# Check for forbidden go-git imports in production code
# (excluding abstraction.go and test files)
grep -r "github.com/go-git/go-git" internal/ \
  --include="*.go" \
  --exclude="*_test.go" \
  --exclude="abstraction.go" \
  --exclude="abstraction_test.go"

# Should return ZERO results
```

**Remove all go-git imports from:**
- `internal/git/branch_worker.go`
- `internal/git/git.go` (except legacy functions until fully removed)
- `internal/controller/gitrepoconfig_controller.go`
- Any other production files

**Allowed go-git usage after cleanup:**
- ✅ `internal/git/abstraction.go` - Implementation
- ✅ `internal/git/abstraction_test.go` - Tests
- ✅ `internal/git/*_test.go` - Integration tests
- ✅ `test/e2e/*.go` - E2E verification

**Required imports post-integration:**
```go
// Production code should only import:
import (
    "github.com/ConfigButler/gitops-reverser/internal/git"
    // And use: git.CheckRepo(), git.PrepareBranch(), git.WriteEvents()
)

// NOT:
import (
    "github.com/go-git/go-git/v5"           // ❌ Forbidden
    "github.com/go-git/go-git/v5/plumbing"  // ❌ Forbidden
)
```

### Phase 4: Update Tests

#### 4.1 Update BranchWorker Tests

**File**: `internal/git/branch_worker_test.go`

**Update existing tests:**
- Update `TestBranchWorker_EmptyRepository` to use new abstraction
- Add tests for synchronization with remote

#### 4.2 Update Controller Tests

**File**: `internal/controller/gitrepoconfig_controller_test.go`

**Update connectivity tests:**
- Tests using `checkRemoteConnectivity` should work with new implementation
- Add tests for empty repository detection

### Phase 5: End-to-End Validation

#### 5.1 Run Full Test Suite

```bash
# Run all tests to ensure integration works
make test

# Run e2e tests with real Git repositories
make test-e2e

# Validate performance improvements
go test -bench=BenchmarkPrepareBranch ./internal/git
```

#### 5.2 Validate Key Scenarios

**Empty Repository Bootstrap:**
1. Create GitRepoConfig pointing to empty repo
2. Create GitDestination
3. Verify GitDestination becomes Ready
4. Trigger event, verify first commit
5. Verify branch metadata updates correctly

**Branch Lifecycle Management:**
1. Create feature branch via GitDestination
2. Merge feature branch to main remotely
3. Verify `PrepareBranch` detects merge and updates branch
4. Verify GitDestination can create new branch from updated default

**Conflict Resolution:**
1. Create conflicting changes
2. Verify `WriteEvents` handles conflicts with retry logic
3. Verify no data loss during conflict resolution

**Reconciliation Loop:**
1. Make external change to repository (manual commit)
2. Verify periodic sync detects change via `IncomingChanges` flag
3. Verify reconciliation is triggered
4. Verify GitDestination status reflects actual Git state

## Migration Checklist

### Pre-Migration
- [ ] All abstraction tests pass in isolation
- [ ] Performance benchmarks show improvements
- [ ] Edge case scenarios validated

### Phase 1: BranchWorker Integration
- [ ] `ensureRepositoryInitialized()` uses `PrepareBranch()`
- [ ] `commitAndPush()` uses `WriteEvents()`
- [ ] **REQUIRED**: Add `syncWithRemote()` method for reconciliation loop
- [ ] **REQUIRED**: Integrate sync into periodic reconciliation ticker
- [ ] Metadata update methods use `PullReport`
- [ ] Handle `IncomingChanges` flag to trigger reconciliation

### Phase 2: Controller Integration
- [ ] `checkRemoteConnectivity()` uses `CheckRepo()`
- [ ] Empty repository detection works correctly

### Phase 3: Code Cleanup & Abstraction Enforcement
- [ ] Obsolete files removed (`status.go`, `status_test.go`)
- [ ] Unused functions removed from `git.go`
- [ ] Direct plumbing calls removed from BranchWorker
- [ ] **All go-git imports removed from production code**
- [ ] **Verify: `grep` check shows zero go-git usage outside abstraction**
- [ ] Imports cleaned up

### Phase 4: Test Updates
- [ ] BranchWorker tests updated for new abstraction
- [ ] Controller tests validated
- [ ] Integration tests added

### Phase 5: Validation
- [ ] `make test` passes
- [ ] `make test-e2e` passes
- [ ] Performance improved
- [ ] Empty repository scenarios work
- [ ] Branch lifecycle management works

## Integration Status

The abstraction is implemented in [`internal/git/abstraction.go`](../internal/git/abstraction.go:1) and ready for integration. The following files need to be updated:

**Files requiring changes:**
- [`internal/git/branch_worker.go`](../internal/git/branch_worker.go:299) - Replace `Clone()` with `PrepareBranch()`
- [`internal/git/branch_worker.go`](../internal/git/branch_worker.go:308) - Remove manual `Checkout()` calls
- [`internal/git/branch_worker.go`](../internal/git/branch_worker.go:319) - Replace `TryPushCommits()` with `WriteEvents()`
- [`internal/controller/gitrepoconfig_controller.go`](../internal/controller/gitrepoconfig_controller.go:330) - Replace connectivity check with `CheckRepo()`

## Rollback Strategy

Integration will be done incrementally with testing at each step:
1. Each phase can be committed separately
2. If issues arise, revert the specific commit
3. Old functions remain available during transition for safety
4. Full removal of old code only after all tests pass

## Success Criteria

1. All existing tests pass
2. New integration tests pass
3. `make test-e2e` passes with empty repositories
4. Performance improved (shallow clones, reduced network calls)
5. BranchWorker metadata correctly updated from `PullReport`
6. GitRepoConfig controller uses `CheckRepo`
7. No breaking changes to external APIs
8. `WriteEvents` correctly handles conflicts with retry logic
9. Periodic sync detects external changes and triggers reconciliation
10. `IncomingChanges` flag properly used to detect drift
11. **No go-git imports in production code (verified via grep)**
12. **All Git operations go through abstraction layer**

## Files Modified Summary

### Modified Files
- `internal/git/branch_worker.go` - Updated to use abstraction
- `internal/controller/gitrepoconfig_controller.go` - Updated connectivity check
- `internal/git/branch_worker_test.go` - Updated tests
- `internal/controller/gitrepoconfig_controller_test.go` - Updated tests

### Removed Files
- `internal/git/status.go`
- `internal/git/status_test.go`

### New Files
- `internal/git/abstraction.go` (created in implementation phase)
- `internal/git/abstraction_test.go` (created in implementation phase)

## Next Steps

To actually perform the integration:

1. **Test the abstraction thoroughly**
   - Verify all edge cases work (empty repos, conflicts, unborn branches)
   - Run `make test` on abstraction code
   - Check test coverage

2. **Plan integration phases**
   - Start with GitRepoConfig controller (simpler)
   - Then integrate BranchWorker (more complex)
   - Test each phase independently

3. **Update this plan**
   - Verify actual API matches updated plan
   - Ensure function signatures are correct
   - Add any missing helper functions

4. **Execute integration with testing**
   - One component at a time
   - Full test suite after each change
   - Monitor for regressions

5. **Clean up old code**
   - Remove obsolete functions from `git.go`
   - Remove old status checking code
   - Update imports

This guide provides the complete integration plan for connecting the git abstraction layer with the existing codebase.