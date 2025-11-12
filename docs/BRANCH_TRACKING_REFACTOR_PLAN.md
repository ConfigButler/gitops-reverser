# Branch Tracking Refactor Plan

## Overview

This document outlines the plan to eliminate separate repository clones for status checking and improve empty repository handling in the GitOps Reverser project.

## Current Problems

### Problem 1: Inefficient Status Checking
Currently, `internal/git/status.go` performs separate clones to check branch status:
- **Location**: `GitDestinationReconciler.updateRepositoryStatus()` calls `git.GetBranchStatus()`
- **Issue**: Creates a separate clone in `/tmp/gitops-reverser-status/<uid>` just to check if a branch exists
- **Impact**: Wastes disk space, network bandwidth, and time with redundant Git operations

### Problem 2: Empty Repository Handling
Empty repositories (no commits) cause errors in GitDestination status updates:
- **Symptom**: GitDestination goes to error state when repository is empty
- **Root Cause**: Status checking expects at least one commit to exist
- **Impact**: Cannot bootstrap new repositories through GitOps Reverser

## Current Architecture

```mermaid
graph TD
    A[GitDestinationReconciler] -->|calls| B[GetBranchStatus]
    B -->|clones to| C[/tmp/gitops-reverser-status/uid]
    A -->|registers with| D[WorkerManager]
    D -->|creates| E[BranchWorker]
    E -->|clones to| F[/tmp/gitops-reverser-workers/ns/repo/branch]
    
    style C fill:#f99,stroke:#333
    style F fill:#9f9,stroke:#333
```

**Key Issue**: Two separate clones for the same repository!

## Proposed Architecture

```mermaid
graph TD
    A[GitDestinationReconciler] -->|queries| B[BranchWorker]
    B -->|maintains| C[Branch Metadata]
    B -->|single clone at| D[/tmp/gitops-reverser-workers/ns/repo/branch]
    A -->|registers with| E[WorkerManager]
    E -->|creates/returns| B
    
    C -->|tracks| F[BranchExists: bool]
    C -->|tracks| G[LastCommitSHA: string]
    C -->|tracks| H[LastFetchTime: time]
    
    style D fill:#9f9,stroke:#333
    style C fill:#99f,stroke:#333
```

**Benefits**: Single clone per (repo, branch) combination with cached metadata!

## Implementation Plan

### Phase 1: Add Branch Metadata to BranchWorker

**File**: `internal/git/branch_worker.go`

Add new fields to `BranchWorker`:
```go
type BranchWorker struct {
    // ... existing fields ...
    
    // Branch metadata (protected by metaMu)
    metaMu          sync.RWMutex
    branchExists    bool
    lastCommitSHA   string
    lastFetchTime   time.Time
    repoInitialized bool
}
```

Add new methods:
```go
// GetBranchMetadata returns current branch status without cloning
func (w *BranchWorker) GetBranchMetadata() (exists bool, sha string, lastFetch time.Time)

// updateBranchMetadata updates cached metadata after git operations
func (w *BranchWorker) updateBranchMetadata(repo *Repo) error

// ensureRepositoryInitialized ensures the worker's repository is cloned and ready
func (w *BranchWorker) ensureRepositoryInitialized(ctx context.Context) error
```

### Phase 2: Update BranchWorker to Track Metadata

**File**: `internal/git/branch_worker.go`

Modify `commitAndPush()` to update metadata after successful operations:
```go
func (w *BranchWorker) commitAndPush(repoConfig *configv1alpha1.GitRepoConfig, events []Event) {
    // ... existing clone and checkout logic ...
    
    // After successful checkout, update metadata
    if err := w.updateBranchMetadata(repo); err != nil {
        log.Error(err, "Failed to update branch metadata")
    }
    
    // ... rest of existing logic ...
    
    // After successful push, update metadata again
    if err := w.updateBranchMetadata(repo); err != nil {
        log.Error(err, "Failed to update branch metadata after push")
    }
}
```

Add initialization in `Start()`:
```go
func (w *BranchWorker) Start(parentCtx context.Context) error {
    // ... existing logic ...
    
    // Initialize repository and metadata in background
    go func() {
        if err := w.ensureRepositoryInitialized(w.ctx); err != nil {
            w.Log.Error(err, "Failed to initialize repository")
        }
    }()
    
    return nil
}
```

### Phase 3: Update GitDestinationReconciler

**File**: `internal/controller/gitdestination_controller.go`

Replace `updateRepositoryStatus()` implementation:

**Before**:
```go
func (r *GitDestinationReconciler) updateRepositoryStatus(...) error {
    // Get authentication
    auth, err := git.GetAuthFromSecret(ctx, r.Client, grc)
    // ...
    
    // Build work directory for status checking (using UID for uniqueness)
    workDir := filepath.Join("/tmp", "gitops-reverser-status", string(dest.UID))
    
    // Get branch status
    status, err := git.GetBranchStatus(grc.Spec.RepoURL, dest.Spec.Branch, auth, workDir)
    // ...
}
```

**After**:
```go
func (r *GitDestinationReconciler) updateRepositoryStatus(...) error {
    log.Info("Updating repository status from BranchWorker")
    
    // Get the branch worker for this destination
    repoNS := dest.Spec.RepoRef.Namespace
    if repoNS == "" {
        repoNS = dest.Namespace
    }
    
    worker, exists := r.WorkerManager.GetWorkerForDestination(
        dest.Spec.RepoRef.Name, repoNS, dest.Spec.Branch,
    )
    
    if !exists {
        // Worker not yet created - this is normal during initial reconciliation
        log.V(1).Info("Worker not yet available, will update status on next reconcile")
        dest.Status.SyncStatus = "initializing"
        return nil
    }
    
    // Get cached metadata from worker
    branchExists, lastCommitSHA, lastFetch := worker.GetBranchMetadata()
    
    // Update status fields
    dest.Status.BranchExists = branchExists
    dest.Status.LastCommitSHA = lastCommitSHA
    dest.Status.LastSyncTime = &metav1.Time{Time: lastFetch}
    dest.Status.SyncStatus = "idle"
    
    log.Info("Repository status updated from worker cache",
        "branchExists", branchExists,
        "lastCommitSHA", lastCommitSHA,
        "lastFetch", lastFetch)
    
    return nil
}
```

### Phase 4: Enhance Empty Repository Handling

**File**: `internal/git/git.go`

The `Clone()` function already has empty repository handling (lines 129-141), but we need to ensure it works correctly:

1. **Verify** `initializeEmptyRepository()` creates a valid repo without initial commit
2. **Add** proper handling in `Checkout()` for empty repos (no HEAD yet)
3. **Update** `TryPushCommits()` to handle first commit to empty repo

**File**: `internal/git/branch_worker.go`

Update `updateBranchMetadata()` to handle empty repos:
```go
func (w *BranchWorker) updateBranchMetadata(repo *Repo) error {
    w.metaMu.Lock()
    defer w.metaMu.Unlock()
    
    // Try to get HEAD
    head, err := repo.Head()
    if err != nil {
        // Repository might be empty (no commits yet)
        if errors.Is(err, plumbing.ErrReferenceNotFound) {
            w.branchExists = false
            w.lastCommitSHA = ""
            w.lastFetchTime = time.Now()
            w.repoInitialized = true
            return nil
        }
        return fmt.Errorf("failed to get HEAD: %w", err)
    }
    
    // Check if branch exists on remote
    remoteBranchRef := plumbing.NewRemoteReferenceName("origin", w.Branch)
    ref, err := repo.Reference(remoteBranchRef, true)
    
    if err == nil {
        // Branch exists on remote
        w.branchExists = true
        w.lastCommitSHA = ref.Hash().String()
    } else if errors.Is(err, plumbing.ErrReferenceNotFound) {
        // Branch doesn't exist yet, use HEAD
        w.branchExists = false
        w.lastCommitSHA = head.Hash().String()
    } else {
        return fmt.Errorf("failed to check branch reference: %w", err)
    }
    
    w.lastFetchTime = time.Now()
    w.repoInitialized = true
    return nil
}
```

### Phase 5: Add Comprehensive Tests

**File**: `internal/git/branch_worker_test.go`

Add test cases:
```go
func TestBranchWorker_EmptyRepository(t *testing.T)
func TestBranchWorker_GetBranchMetadata(t *testing.T)
func TestBranchWorker_MetadataUpdateAfterPush(t *testing.T)
```

**File**: `internal/controller/gitdestination_controller_test.go`

Add test cases:
```go
func TestGitDestinationReconciler_EmptyRepository(t *testing.T)
func TestGitDestinationReconciler_StatusFromWorkerCache(t *testing.T)
```

### Phase 6: Remove Obsolete Code

**Files to Remove**:
- `internal/git/status.go`
- `internal/git/status_test.go`

**Files to Update** (remove imports):
- `internal/controller/gitdestination_controller.go`

### Phase 7: Update Documentation

**Files to Update**:
- `README.md` - Update architecture description
- `DEVELOPMENT_RULES.md` - Update if it references status checking
- `docs/BRANCH_TRACKING_ANALYSIS.md` - Add note about this refactor

## Testing Strategy

### Unit Tests

1. **Empty Repository Tests**:
   - Clone empty repository
   - Checkout non-existent branch in empty repo
   - Create first commit in empty repo
   - Push first commit to empty repo

2. **Metadata Caching Tests**:
   - Verify metadata updates after clone
   - Verify metadata updates after push
   - Verify metadata is thread-safe
   - Verify stale metadata handling

3. **Integration Tests**:
   - GitDestination with empty repository
   - GitDestination status updates from worker cache
   - Multiple GitDestinations sharing same worker

### Manual Testing

1. Create empty Git repository
2. Create GitRepoConfig pointing to empty repo
3. Create GitDestination with non-existent branch
4. Verify GitDestination becomes Ready
5. Create WatchRule to trigger events
6. Verify first commit creates branch successfully
7. Verify GitDestination status shows correct branch info

## Migration Path

This refactor is **backward compatible**:
- No API changes to CRDs
- No changes to user-facing behavior
- Only internal implementation changes

**Rollout**:
1. Deploy new version
2. Existing GitDestinations will automatically use new code path
3. No manual intervention required

## Success Criteria

- [ ] `internal/git/status.go` removed
- [ ] No separate clones for status checking
- [ ] Empty repositories handled without errors
- [ ] All existing tests pass
- [ ] New tests for empty repo scenarios pass
- [ ] `make lint` passes
- [ ] `make test` passes with >90% coverage
- [ ] `make test-e2e` passes

## Risks and Mitigations

### Risk 1: Race Conditions in Metadata Access
**Mitigation**: Use `sync.RWMutex` for all metadata access

### Risk 2: Stale Metadata
**Mitigation**: Update metadata after every git operation (fetch, push)

### Risk 3: Worker Not Available During Status Check
**Mitigation**: Handle gracefully by marking status as "initializing"

### Risk 4: Empty Repo Edge Cases
**Mitigation**: Comprehensive test coverage for all empty repo scenarios

## Timeline Estimate

- **Phase 1-2**: Add metadata tracking to BranchWorker - 2 hours
- **Phase 3**: Update GitDestinationReconciler - 1 hour
- **Phase 4**: Enhance empty repo handling - 2 hours
- **Phase 5**: Add comprehensive tests - 3 hours
- **Phase 6**: Remove obsolete code - 30 minutes
- **Phase 7**: Update documentation - 30 minutes

**Total**: ~9 hours of development work

## References

- Current implementation: [`internal/git/status.go`](../internal/git/status.go)
- BranchWorker: [`internal/git/branch_worker.go`](../internal/git/branch_worker.go)
- GitDestination controller: [`internal/controller/gitdestination_controller.go`](../internal/controller/gitdestination_controller.go)
- Empty repo handling: [`internal/git/git.go`](../internal/git/git.go:112-203)