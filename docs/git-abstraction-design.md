# Git Abstraction Design for GitOps Reverser

## Overview

This document describes the design and implementation of a new git abstraction layer specifically crafted for GitOps Reverser. The abstraction provides a simple, focused interface that handles the complexities of git operations, particularly around empty repository scenarios, while optimizing for the GitOps Reverser's specific operational patterns.

## Alignment with GitOps Reverser Refactor Plan

This git abstraction design directly supports and enables the goals outlined in the [GitOps Reverser Refactor Plan](../GITOPS_REVERSER_REFACTOR_PLAN.md):

### Key Alignments

1. **Eliminate Duplicate Repository Clones**: The refactor plan aims to replace separate status-checking clones with cached metadata in BranchWorker. Our `prepareBranch()` function clones once when GitDestination is created, and `writeEvents()` reuses that clone for all operations.

2. **Branch Metadata Caching**: The plan adds metadata fields to BranchWorker (`branchExists`, `lastCommitSHA`, `lastFetchTime`, etc.). Our abstraction provides the git operations that populate and maintain this metadata.

3. **Empty Repository Handling**: The plan specifically addresses empty repository issues. Our abstraction is designed from the ground up to handle unborn branch states and first commits gracefully.

4. **Single Clone Per Branch**: The plan establishes one clone per (repo, branch) combination. Our `prepareBranch()` creates this clone, and all subsequent operations use the same repository instance.

### Integration Points

- **BranchWorker.ensureRepositoryInitialized()** will call our `prepareBranch()`
- **BranchWorker.updateBranchMetadata()** will be called after our git operations
- **GitRepoConfig controller** will use our `checkRepo()` for lightweight validation
- **BranchWorker.commitAndPush()** will use our `writeEvents()` for the complete workflow

### Metadata & Status Information Delivery

The abstraction delivers all metadata needed by the refactor plan:

**BranchWorker Metadata Fields** (populated by abstraction operations):
- `branchExists` ← Determined by `checkRepo()` and `writeEvents()` operations
- `lastCommitSHA` ← Returned by git operations, updated after commits/pushes
- `lastFetchTime` ← Set when `checkRepo()` or clone operations complete
- `repoInitialized` ← Set to true after successful `prepareBranch()`
- `lastPushTime` ← Set after successful `writeEvents()` push operations
- `lastPushStatus` ← Set by `writeEvents()` ("Success", "Failed", "Pending")

**GitDestination Status Fields** (provided by BranchWorker using abstraction data):
- `GitStatus.BranchExists` ← From cached `branchExists`
- `GitStatus.LastCommitSHA` ← From cached `lastCommitSHA`
- `GitStatus.LastChecked` ← From cached `lastFetchTime`
- `WorkerStatus.Active` ← `started && repoInitialized`
- `WorkerStatus.QueuedEvents` ← `len(eventQueue)`
- `WorkerStatus.LastPushTime` ← From cached `lastPushTime`
- `WorkerStatus.LastPushStatus` ← From cached `lastPushStatus`

**Empty Repository Support**: All operations handle unborn branch states gracefully, providing accurate metadata for empty repositories.

This abstraction provides the complete git operations layer that enables the refactor plan's goals of eliminating duplicate clones and providing comprehensive status information.

## Background and Problem Statement

### Current Implementation Issues

The existing git implementation in GitOps Reverser has several critical issues with empty repository handling:

1. **Inconsistent empty repo handling**: `checkRemoteConnectivity` accepts empty repos (returns `branchCount=0`), but `BranchWorker.Clone()` fails on empty repos due to go-git's HEAD resolution issues
2. **Direct go-git plumbing exposure**: BranchWorker directly handles `plumbing.ErrReferenceNotFound` and HEAD checks
3. **Complex checkout logic**: Current `Checkout` method assumes branch existence and has special cases for empty repos
4. **Scattered git logic**: Git operations are spread across `git.go`, `branch_worker.go`, and controller code

### GitOps Reverser Operational Context

The GitOps Reverser has unique operational characteristics that simplify git handling:

- **No unnecessary commits**: We won't create commits during checkout/initialization unless there's actual event data to commit. Empty repositories remain empty until real changes occur.
- **Cluster as source of truth**: The cluster state is authoritative; we don't perform merges or pull before committing, expecting to be the primary contributor to target branches.
- **Conflict resolution strategy**: On push conflicts, perform hard reset and reapply the buffered events rather than merging. This prioritizes speed over complex merge strategies.
- **Speed optimization**: These design choices enable fast, conflict-free operations by avoiding traditional Git workflow complexities.

## Git Internals Background

### How Git Tracks Branches
In a checked-out Git directory, branch state is tracked in the `.git` folder:

- **`.git/HEAD`**: Contains a reference to the current branch (e.g., `ref: refs/heads/main`)
- **`.git/refs/heads/`**: Directory containing branch reference files
- **Empty repositories**: Have `.git/HEAD` but no commits, so `refs/heads/` is empty

### Unborn Branch State
Empty repositories exhibit a special "unborn branch" state where:

- **HEAD exists**: Points to a symbolic reference like `ref: refs/heads/main`
- **Branch reference missing**: The actual file `.git/refs/heads/main` doesn't exist because no commits have been made
- **git status behavior**: Shows "On branch main" for user-friendliness, indicating intended state
- **git branch --list behavior**: Returns empty list since no branch reference files exist
- **go-git challenge**: Library attempts to resolve HEAD reference and fails when branch ref is missing, unlike Git CLI which handles this gracefully

This state occurs during repository initialization before the first commit creates the branch reference.

**Note on Branch Creation**:
- **In empty repositories**: Creating a new branch creates the reference file, but the branch remains unborn until the first commit is made, as there's no commit SHA to point to. If a commit needs to be created the branch will be orphaned (it does not have a parent commit).
- **In repositories with commits on the default branch**: Creating a new branch immediately sets the branch reference to the current HEAD commit SHA, so it's not unborn.
- **Unborn state can occur even in repositories with commits**: If commits exist but the default branch (e.g., "main") has never received commits (only orphan branches have been used), the default branch remains unborn.

### go-git Issue with Empty Repositories
go-git has a known issue where `Checkout()` fails on empty repositories because:
- HEAD exists but points to a non-existent branch reference (unborn branch state)
- The refs directory is empty (no branch refs created yet)
- Unlike Git CLI, go-git strictly resolves references and reports "reference not found" errors
- This differs from standard `git clone` behavior which masks the initial state

**Reference**: https://github.com/go-git/go-git/issues/118#issuecomment-1759167978

## Solution Architecture

### Core Data Structures

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
```

### Core Functions

The new abstraction provides three main functions:

#### 1. checkRepo
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

#### 2. prepareBranch
Clones repository immediately when GitDestination is created, optimized for single branch usage.

```go
func prepareBranch(ctx context.Context, repoURL, repoPath, branchName string, auth transport.AuthMethod) error
```

**Purpose**: Prepare repository for operations (called when GitDestination is created)
**Behavior**:
- Performs shallow clone (`Depth: 1`) with target branch as primary
- Fetches both target and default branche for complete branch coverage
- Checks out default branch to establish stable baseline state
- Handles empty repositories gracefully during checkout
- Sets up repository structure for subsequent operations
- Provides immediate access to both target and default branche
- Optimized for speed while ensuring fallback availability

#### 3. writeEvents
Handles the complete write workflow: checkout, commit, push with conflict resolution.

```go
func writeEvents(ctx context.Context, repoPath, branchName string, events []Event, auth transport.AuthMethod) error
```

**Purpose**: Complete event writing workflow
**Behavior**:
- Performs checkout logic (creates branch if needed)
- Generates commits from events
- Pushes with conflict resolution (rebase strategy)
- Handles empty repository first commits
- Optimized for speed: rare edge cases (like branch removal) handled via typed errors rather than comprehensive checks
- Returns specific errors (ErrBranchRemoved, ErrBranchMerged) for BranchWorker to handle

#### 4. pullBranch
Periodic reconciliation that syncs local clone with remote state and reports detailed operation results.

```go
type PullReport struct {
    ExistsOnRemote  bool      // Branch still exists on remote
    SwitchedToDefault bool    // Switched to default branch (remote deleted or empty repo)
    HeadSha         string    // Current checked out SHA (default, remote branch, or empty)
    IsOrphan        bool      // First commit created without default branch parent
    IncomingChanges bool      // SHA changed, requiring resource-level reconcile
    LastChecked     time.Time // When this pull was performed
}

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

## Implementation Details

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

### Error Handling Strategy

#### Connectivity vs Operational Errors
- **Connectivity errors**: Repository unreachable, auth failures
- **Operational errors**: Branch conflicts, commit failures
- Clear error messages distinguish between these cases

#### go-git Quirks
- Wrap go-git errors with context-specific messages
- Handle unborn branch state gracefully
- Provide fallbacks for edge cases

### Concurrent Operations

#### Multiple GitDestinations
When multiple GitDestinations target different branches in the same empty repository:

- **Race condition risk**: Multiple workers may attempt simultaneous first commits on different branches
- **Resolution approach**: Since cluster is source of truth, the first successful push establishes the initial commit. Subsequent workers detect the repository is no longer empty and operate normally.
- **Branch isolation**: Each branch remains independent; no automatic merging occurs between branches during initialization.
- **Conflict handling**: If concurrent pushes conflict, hard reset and reapply strategy applies, ensuring eventual consistency.
- **No cross-branch coordination**: Workers don't merge branches; each maintains its own branch state as per GitDestination specification.

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

### Phase 3: Comprehensive Testing (Test First Approach)
9. **Test abstraction in isolation** before integration
10. Create comprehensive unit tests for all functions
11. **Add extensive integration tests** covering all edge cases:
    - Empty repository lifecycle (creation → first commit → normal ops)
    - Branch merging scenarios (feature → main, GitDestination handling)
    - Branch deletion scenarios (remote removed, fallback to default)
    - Concurrent operations (multiple GitDestinations)
    - Error conditions (auth failures, network issues, conflicts)
12. **Validate all scenarios** with real Git repositories before integration

#### Test Implementation Strategy

The testing strategy follows a **test-first approach** with comprehensive coverage of all edge cases. Tests are implemented in `internal/git/abstraction_test.go` and run before any application integration.

##### Test Categories

**1. Unit Tests** - Test individual functions with mocked dependencies
**2. Integration Tests** - Test with real Git repositories in temporary directories
**3. Edge Case Tests** - Cover all scenarios from empty-repo-handling-plan.md
**4. Performance Tests** - Validate shallow clone optimizations

##### Test Execution Strategy

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

##### Test Structure Examples

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
    err = writeEvents(context.Background(), filepath.Join(tempDir, "clone"), "feature", events, nil)
    require.NoError(t, err)

    // Verify repository state
    repo, err := git.PlainOpen(filepath.Join(tempDir, "clone"))
    require.NoError(t, err)

    head, err := repo.Head()
    require.NoError(t, err)
    assert.Equal(t, plumbing.NewBranchReferenceName("feature"), head.Name())
}
```

##### Test Conversion Plan

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

##### Test Data Setup

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

##### Edge Case Test Scenarios

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

##### Performance Validation

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

##### Test Organization

```
internal/git/
├── abstraction.go           # Core abstraction functions
├── abstraction_test.go      # Unit and integration tests
├── test_helpers_test.go     # Shared test utilities
└── testdata/               # Test data files (if needed)
```

All tests run with `go test ./internal/git` and provide comprehensive coverage before integration with the rest of the application.

### Phase 4: Application Integration
13. Update BranchWorker to use new abstraction functions
14. Update GitRepoConfig controller to use `checkRepo`
15. Add end-to-end integration tests with full application stack

### Phase 4: Validation & Documentation
11. Validate with `make test-e2e`
12. Update godoc comments and documentation
13. Document go-git behavior quirks and workarounds

## Expected Outcomes

### Functional Improvements
- **Consistent empty repo handling**: Connectivity checks and actual operations agree
- **Branch-specific operations**: No more default branch assumptions
- **Immediate preparation**: Clone happens when GitDestination is created, not during first write
- **Better error messages**: Clear distinction between connectivity and operational issues

### Code Quality Improvements
- **Clean abstractions**: BranchWorker shielded from go-git plumbing details
- **Simple interface**: Only expose what's needed for GitOps Reverser
- **Testable code**: Clean abstraction enables thorough unit testing
- **Maintainable**: Centralized git logic with clear separation of concerns

## Success Criteria

1. ✅ `make test-e2e` passes with empty repositories
2. ✅ BranchWorker correctly handles empty repo initialization
3. ✅ No default branch assumptions in code
4. ✅ Clean separation between connectivity validation and actual operations
5. ✅ Immediate clone preparation when GitDestination is created
6. ✅ Comprehensive test coverage for edge cases

## Migration Strategy

Since backwards compatibility is not required:

1. Implement new abstraction alongside existing code
2. Update all callers to use new interface
3. Remove old implementation once migration complete
4. Validate all functionality with comprehensive testing

This design creates a focused, efficient git abstraction that solves the empty repository issues while optimizing for GitOps Reverser's specific operational patterns.