# Git Abstraction Integration Guide

**Version**: 1.0
**Date**: 2025-11-13
**Status**: Integration Ready

## Overview

This document provides the complete integration guide for replacing the existing git implementation with the new git abstraction layer. The integration follows a systematic approach to ensure backwards compatibility is maintained during the transition.

## Prerequisites

- ✅ New abstraction (`internal/git/abstraction.go`) is implemented and fully tested
- ✅ All abstraction tests pass (`go test ./internal/git -run "Test(CheckRepo|PrepareBranch|WriteEvents|PullBranch)"`)
- ✅ Test coverage >90% for abstraction functions
- ✅ Edge cases from empty-repo-handling-plan.md are validated

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

    // Use new prepareBranch abstraction
    if err := prepareBranch(ctx, repoConfig.Spec.RepoURL, repoPath, w.Branch, auth); err != nil {
        return fmt.Errorf("failed to prepare repository: %w", err)
    }

    // Update metadata after repository initialization
    // Note: prepareBranch leaves us on default branch, metadata will reflect this
    if err := w.updateBranchMetadataAfterPrepare(repoPath); err != nil {
        w.Log.V(1).Error(err, "Failed to update branch metadata after preparation")
    }

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
    auth, err := getAuthFromSecret(w.ctx, w.Client, repoConfig)
    if err != nil {
        log.Error(err, "Failed to get auth")
        return
    }

    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)

    // Use new writeEvents abstraction
    if err := writeEvents(w.ctx, repoPath, w.Branch, events, auth); err != nil {
        log.Error(err, "Failed to write events")
        return
    }

    // ... existing success handling ...
}
```

#### 1.3 Add BranchWorker.pullBranchForReconciliation()

**File**: `internal/git/branch_worker.go`

**Add new method:**
```go
func (w *BranchWorker) pullBranchForReconciliation(ctx context.Context) (*PullReport, error) {
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

    // Use new pullBranch abstraction
    report, err := pullBranch(ctx, repoPath, w.Branch, auth)
    if err != nil {
        return nil, fmt.Errorf("failed to pull branch: %w", err)
    }

    // Update metadata based on pull report
    w.updateBranchMetadataFromPullReport(report)

    return report, nil
}
```

#### 1.4 Update BranchWorker Metadata Methods

**File**: `internal/git/branch_worker.go`

**Update metadata handling:**
```go
func (w *BranchWorker) updateBranchMetadataAfterPrepare(repoPath string) error {
    w.metaMu.Lock()
    defer w.metaMu.Unlock()

    // After prepareBranch, we're on default branch
    // Branch-specific metadata will be updated during first writeEvents or pullBranch
    w.repoInitialized = true
    w.lastFetchTime = time.Now()

    return nil
}

func (w *BranchWorker) updateBranchMetadataFromPullReport(report *PullReport) {
    w.metaMu.Lock()
    defer w.metaMu.Unlock()

    w.branchExists = report.ExistsOnRemote
    w.lastCommitSHA = report.HeadSha
    w.lastFetchTime = report.LastChecked

    // Update push status based on changes
    if report.IncomingChanges {
        w.lastPushStatus = "Updated"
    }
}
```

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
    // Use new checkRepo abstraction
    repoInfo, err := checkRepo(ctx, repoURL, auth)
    if err != nil {
        return 0, err
    }

    return repoInfo.RemoteBranchCount, nil
}
```

### Phase 3: Remove Obsolete Code

#### Files to Remove

**Remove entire files:**
- `internal/git/status.go` - Obsolete status checking logic
- `internal/git/status_test.go` - Tests for removed status logic

**Remove functions from `internal/git/git.go`:**
- `Clone()` - Replaced by `prepareBranch()`
- `TryPushCommits()` - Replaced by `writeEvents()`
- `Checkout()` - Logic moved to abstraction functions
- `GetBranchStatus()` - Replaced by `checkRepo()`

**Remove functions from `internal/git/branch_worker.go`:**
- `updateBranchMetadata(repo *Repo)` - Replaced with report-based updates
- Direct git plumbing calls in `ensureRepositoryInitialized()`

#### Update Imports

**Remove imports from affected files:**
```go
// Remove these imports where no longer needed
"github.com/go-git/go-git/v5/plumbing"
```

### Phase 4: Update Tests

#### 4.1 Update BranchWorker Tests

**File**: `internal/git/branch_worker_test.go`

**Update existing tests:**
- `TestBranchWorker_EmptyRepository` → Test with new abstraction
- Add tests for new `pullBranchForReconciliation()` method

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
3. Verify `pullBranch` detects merge and switches to default
4. Verify GitDestination can create new branch from updated default

**Conflict Resolution:**
1. Create conflicting changes
2. Verify `writeEvents` handles conflicts via rebase strategy
3. Verify no data loss during conflict resolution

## Migration Checklist

### Pre-Migration
- [ ] All abstraction tests pass in isolation
- [ ] Performance benchmarks show improvements
- [ ] Edge case scenarios validated

### Phase 1: BranchWorker Integration
- [ ] `ensureRepositoryInitialized()` uses `prepareBranch()`
- [ ] `commitAndPush()` uses `writeEvents()`
- [ ] New `pullBranchForReconciliation()` method added
- [ ] Metadata update methods updated for report-based operation

### Phase 2: Controller Integration
- [ ] `checkRemoteConnectivity()` uses `checkRepo()`
- [ ] Empty repository detection works correctly

### Phase 3: Code Cleanup
- [ ] Obsolete files removed (`status.go`, `status_test.go`)
- [ ] Unused functions removed from `git.go`
- [ ] Direct plumbing calls removed from BranchWorker
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

## Rollback Plan

If issues occur during integration:

1. **Immediate rollback**: Revert BranchWorker changes to use old methods
2. **Partial rollback**: Keep abstraction but revert controller changes
3. **Full rollback**: Remove abstraction files, restore old implementation

The new abstraction is designed to be non-disruptive - old code paths remain functional during transition.

## Success Criteria

1. ✅ All existing tests pass
2. ✅ New integration tests pass
3. ✅ `make test-e2e` passes with empty repositories
4. ✅ Performance improved (shallow clones, reduced network calls)
5. ✅ BranchWorker metadata correctly updated
6. ✅ GitRepoConfig controller detects empty repos
7. ✅ No breaking changes to external APIs
8. ✅ Clean separation between connectivity validation and operations

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

This integration guide ensures a smooth transition to the new git abstraction while maintaining system stability and backwards compatibility during the migration process.