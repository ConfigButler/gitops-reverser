# Git Abstraction Implementation Guide

**Version**: 1.0
**Date**: 2025-11-13
**Status**: Implementation Ready

## Overview

This document provides the complete implementation guide for creating the new git abstraction layer for GitOps Reverser. The abstraction provides a simple, focused interface that handles the complexities of git operations, particularly around empty repository scenarios.

## Core Data Structures

```go
// RepoInfo represents high-level repository information
type RepoInfo struct {
    DefaultBranch struct {
        Name   string // e.g., "main"
        Ref    string // commit hash or empty for unborn
        Unborn bool   // true for empty repos / or that don't have commits on the default branch
    }
    RemoteBranchCount int
}

// PullReport provides detailed pull operation results
type PullReport struct {
    ExistsOnRemote  bool      // Branch exists on remote
    SwitchedToDefault bool    // Switched to default branch or empty (because the remote branch does not exist anymore)
    HeadSha         string    // Current checked out SHA (default, remote branch, or empty)
    IsOrphan        bool      // First commit created without default branch parent
    IncomingChanges bool      // SHA changed, requiring resource-level reconcile
    LastChecked     time.Time // When this pull was performed
}

// WriteEventsResult provides detailed writeEvents operation results
type WriteEventsResult struct {
    CommitsCreated int         // Number of succesfuly pushed commits (0 if no changes)
    ConflictPull   *PullReport // PullReport if conflict resolution required, nil otherwise
}
```

## Core Functions

### 1. checkRepo
Performs lightweight connectivity checks and gathers repository metadata.

```go
func checkRepo(ctx context.Context, repoURL string, auth transport.AuthMethod) (*RepoInfo, error)
```

**Purpose**: Lightweight validation used by GitRepoConfig controller
**Behavior**:
- Uses `remote.List()` for connectivity
- Detects default branch from remote refs
- Handles empty repositories gracefully
- Returns populated RepoInfo with branch metadata

### 2. prepareBranch
Clones repository immediately when GitDestination is created, optimized for single branch usage.

```go
func prepareBranch(ctx context.Context, repoURL, repoPath, branchName string, auth transport.AuthMethod) error
```

**Purpose**: Prepare repository for operations (called when GitDestination is created)
**Behavior**:
- Performs shallow clone (`Depth: 1`) with target branch as primary
- Fetches both target and default branch for fast access to required context.
- Checkout itself is NOT done (`NoCheckout: true`): so that we also properly support an empty git repo.
- Handles empty repositories gracefully during checkout
- Sets up repository structure for subsequent operations
- Provides immediate access to both target and default branch
- Optimized for speed while ensuring fallback availability

### 3. writeEvents
Handles the complete write workflow: checkout, commit, push with conflict resolution.

```go
func writeEvents(ctx context.Context, repoPath, branchName string, events []Event, auth transport.AuthMethod) (*WriteEventsResult, error)
```

**Purpose**: Complete event writing workflow
**Behavior**:
- Performs checkout logic:
    - creates target branch if needed
    - support scenario where the default branch does not exist (create a orphaned branch)
    - also must support scenario where the target branch already does exists remotely.
- Generates commits from events
- Pushes with conflict resolution (rebase strategy)
    - We first pull, and then we just write our changes like we intented to do.
- Optimized for speed: rare edge cases (like branch removal) handled via typed errors rather than comprehensive checks
- Only commit if things really changed: do support the edge case where somethiing does not result in a commit.
- Returns WriteEventsResult with commit count and PullReport (only filled when conflict resolution required)

**WriteEventsResult Analysis**:
- **`CommitsCreated`**: Number of commits actually created (0 if no changes)
- **`ConflictPull`**: PullReport if conflict resolution was needed (nil otherwise)

### 4. pullBranch
Periodic reconciliation that syncs local clone with remote state and reports detailed operation results.

```go
func pullBranch(ctx context.Context, repoPath, branchName string, auth transport.AuthMethod) (*PullReport, error)
```

**Purpose**: Kubernetes operator-style reconciliation (called on timer)
**Behavior**:
- Fetches and syncs with remote state (may fetch additional branches if needed)
- Reports exactly what happened during the pull operation
- Detects branch lifecycle changes (merges, deletions, new commits)
- Handles branch removal by switching to default branch
- Provides precise information for deciding if cluster resync is needed
- Complements prepareBranch's sparse clone with full branch coverage when required

**PullReport Analysis**:
- **`ExistsOnRemote`**: Check if target branch still exists (false = merged/deleted)
- **`SwitchedToDefault`**: Local checkout moved to default branch due to remote changes
- **`HeadSha`**: Current commit SHA - compare with previous to detect changes
- **`IsOrphan`**: Branch created without parent (first commit in empty repo)
- **`IncomingChanges`**: `HeadSha` changed, requiring resource reconciliation

## Implementation Plan

### Phase 1: Core Abstraction
1. Create `internal/git/abstraction.go` with clean interfaces
2. Implement `RepoInfo` struct for repository metadata
3. Implement `PullReport` struct for detailed pull operation results
4. Add `checkRepo` function with lightweight remote operations

### Phase 2: Repository Operations
5. Implement `prepareBranch` function for immediate cloning with NoCheckout
6. Implement `writeEvents` function with complete workflow and error signaling
7. Implement `pullBranch` function for periodic reconciliation with detailed reporting
8. Add proper error handling for go-git edge cases and branch lifecycle events

### Phase 3: Comprehensive Testing
9. **Test abstraction in isolation** before integration
10. Create comprehensive unit tests for all functions
11. **Add extensive integration tests** covering all edge cases:
    - Empty repository lifecycle (creation → first commit → normal ops)
    - Branch merging scenarios (feature → main, GitDestination handling)
    - Branch deletion scenarios (remote removed, fallback to default)
    - Concurrent operations (multiple GitDestinations)
    - Error conditions (auth failures, network issues, conflicts)
12. **Validate all scenarios** with real Git repositories before integration

## Test Implementation Strategy

The testing strategy follows a **test-first approach** with comprehensive coverage of all edge cases. Tests are implemented in `internal/git/abstraction_test.go` and run before any application integration.

### Test Categories

**1. Unit Tests** - Test individual functions with mocked dependencies
**2. Integration Tests** - Test with real Git repositories in temporary directories
**3. Edge Case Tests** - Cover all scenarios from empty-repo-handling-plan.md
**4. Performance Tests** - Validate shallow clone optimizations

### Test Execution Strategy

```bash
# Run abstraction tests in isolation
cd internal/git
go test -v -run "Test(CheckRepo|PrepareBranch|WriteEvents|PullBranch)" ./...

# Run with coverage
go test -cover -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run integration tests with real Git repos
go test -v -tags=integration ./...
```

### Test Structure Examples

**Unit Test Example:**
```go
func TestCheckRepo_EmptyRepository(t *testing.T) {
    // Test checkRepo with empty repository URL
    repoInfo, err := checkRepo(context.Background(), "https://github.com/empty/repo.git", nil)

    require.NoError(t, err)
    assert.True(t, repoInfo.DefaultBranch.Unborn)
    assert.Equal(t, 0, repoInfo.RemoteBranchCount)
    assert.Equal(t, "main", repoInfo.DefaultBranch.Name)
}
```

**Integration Test Example:**
```go
func TestWriteEvents_FirstCommitOnEmptyRepo(t *testing.T) {
    // Create temporary directory for test
    tempDir := t.TempDir()

    // Initialize empty Git repository
    repoPath := filepath.Join(tempDir, "repo")
    _, err := git.PlainInit(repoPath, false)
    require.NoError(t, err)

    // Test prepareBranch
    err = prepareBranch(context.Background(), "file://"+repoPath, filepath.Join(tempDir, "clone"), "feature", nil)
    require.NoError(t, err)

    // Test writeEvents with first commit
    events := []Event{createTestEvent("test-pod", "default")}
    result, err := writeEvents(context.Background(), filepath.Join(tempDir, "clone"), "feature", events, nil)
    require.NoError(t, err)
    assert.Equal(t, 1, result.CommitsCreated)
    assert.Nil(t, result.ConflictPull) // No conflict on first commit

    // Verify repository state
    repo, err := git.PlainOpen(filepath.Join(tempDir, "clone"))
    require.NoError(t, err)

    head, err := repo.Head()
    require.NoError(t, err)
    assert.Equal(t, plumbing.NewBranchReferenceName("feature"), head.Name())
}
```

### Test Conversion Plan

**Tests to Convert from `git_test.go`:**
- `TestClone_EmptyRepositoryHandling` → `TestPrepareBranch_EmptyRepository`
- `TestGenerateLocalCommits_FirstCommitOnEmptyRepo` → `TestWriteEvents_FirstCommitOnEmptyRepo`
- `TestGenerateLocalCommits_MultipleEventsOnEmptyRepo` → `TestWriteEvents_MultipleEventsOnEmptyRepo`
- `TestTryPushCommits_FirstCommitOnEmptyRepoWithBranchCreation` → `TestWriteEvents_BranchCreationAndPush`
- `TestCheckout_BranchCreationOnEmptyRepo` → `TestPrepareBranch_BranchCreation`
- `TestCheckout_BranchDoesNotExist` → `TestPrepareBranch_NonExistentBranch`

**Tests to Convert from `branch_worker_test.go`:**
- `TestBranchWorker_EmptyRepository` → `TestPullBranch_EmptyRepositoryReport`

**Tests to Convert from `conflict_resolution_test.go`:**
- `TestReEvaluateEvents` → `TestWriteEvents_EventReEvaluation`
- `TestIsEventStillValid` → `TestWriteEvents_EventValidation`

**New Tests for Abstraction:**
- `TestCheckRepo_ConnectivityAndMetadata`
- `TestCheckRepo_EmptyRepository`
- `TestPrepareBranch_ShallowCloneOptimization`
- `TestPrepareBranch_DefaultBranchCheckout`
- `TestWriteEvents_ConflictResolution`
- `TestWriteEvents_ErrorSignaling`
- `TestPullBranch_BranchLifecycleDetection`
- `TestPullBranch_MergeToDefaultScenario`
- `TestPullBranch_BranchDeletionScenario`

### Test Data Setup

**Helper Functions:**
```go
func createTestEvent(name, namespace string) Event {
    obj := &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "v1",
            "kind":       "Pod",
            "metadata": map[string]interface{}{
                "name":      name,
                "namespace": namespace,
            },
        },
    }
    return Event{
        Object: obj,
        Identifier: types.ResourceIdentifier{
            Group:     "",
            Version:   "v1",
            Resource:  "pods",
            Namespace: namespace,
            Name:      name,
        },
        Operation: "CREATE",
        UserInfo:  UserInfo{Username: "test-user"},
    }
}

func setupTestRepository(t *testing.T, empty bool) (remotePath, clonePath string) {
    tempDir := t.TempDir()
    remotePath = filepath.Join(tempDir, "remote")

    if empty {
        // Create empty repository
        _, err := git.PlainInit(remotePath, false)
        require.NoError(t, err)
    } else {
        // Create repository with initial commit
        repo, err := git.PlainInit(remotePath, false)
        require.NoError(t, err)
        createInitialCommit(t, repo, remotePath)
    }

    clonePath = filepath.Join(tempDir, "clone")
    return remotePath, clonePath
}
```

### Edge Case Test Scenarios

**Empty Repository Lifecycle:**
1. `checkRepo` detects empty repo correctly
2. `prepareBranch` clones and checks out default branch
3. `writeEvents` creates first commit on target branch
4. `pullBranch` reports orphan commit and incoming changes

**Branch Merging Scenarios:**
1. Feature branch exists and has commits
2. Remote merges feature to main, deletes feature branch
3. `pullBranch` detects `ExistsOnRemote=false`, `SwitchedToDefault=true`
4. GitDestination creates new branch from updated default

**Branch Deletion Scenarios:**
1. Target branch deleted remotely
2. `writeEvents` returns `ErrBranchRemoved`
3. `pullBranch` confirms switch to default branch

**Concurrent Operations:**
1. Multiple GitDestinations targeting different branches
2. Race conditions during first commits
3. Proper isolation and conflict resolution

### Performance Validation

```go
func BenchmarkPrepareBranch_ShallowClone(b *testing.B) {
    // Benchmark shallow clone performance
    tempDir := b.TempDir()
    remotePath := setupLargeTestRepo(b, tempDir)

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        clonePath := filepath.Join(tempDir, fmt.Sprintf("clone-%d", i))
        err := prepareBranch(context.Background(), "file://"+remotePath, clonePath, "test-branch", nil)
        require.NoError(b, err)
    }
}
```

### Test Organization

```
internal/git/
├── abstraction.go           # Core abstraction functions
├── abstraction_test.go      # Unit and integration tests
├── test_helpers_test.go     # Shared test utilities
└── testdata/               # Test data files (if needed)
```

All tests run with `go test ./internal/git` and provide comprehensive coverage before integration with the rest of the application.

## Success Criteria

1. ✅ `checkRepo` correctly identifies empty repositories and branch metadata
2. ✅ `prepareBranch` performs optimized shallow clones with default branch checkout
3. ✅ `writeEvents` handles first commits and conflict resolution, returns WriteEventsResult with commit count and conflict details
4. ✅ `pullBranch` provides detailed operation reporting
5. ✅ All edge cases from empty-repo-handling-plan.md are covered
6. ✅ Comprehensive test coverage (>90%) for all functions
7. ✅ Performance benchmarks show shallow clone benefits
8. ✅ All tests pass in isolation before application integration

## Implementation Notes

### Error Handling Strategy

#### Connectivity vs Operational Errors
- **Connectivity errors**: Repository unreachable, auth failures
- **Operational errors**: Branch conflicts, commit failures
- Clear error messages distinguish between these cases

#### go-git Quirks
- Wrap go-git errors with context-specific messages
- Handle unborn branch state gracefully
- Provide fallbacks for edge cases

### Repository State Management

#### Empty Repository Handling
- **Detection**: Check for commits via HEAD resolution failure
- **Initialization**: Create branches during first commit, not during preparation
- **State tracking**: Use unborn flag to track repositories waiting for first commit

#### Branch-Specific Operations
- All operations are branch-aware, no default branch assumptions
- Each GitDestination manages its own branch state
- Concurrent branch operations are isolated

#### Conflict Resolution Strategy
- On push conflict: hard reset to remote + reapply events
- No merging: cluster is source of truth
- Fast conflict resolution prioritizes speed over complex merge logic

This implementation guide provides everything needed to create and thoroughly test the git abstraction before integrating it with the rest of the GitOps Reverser application.