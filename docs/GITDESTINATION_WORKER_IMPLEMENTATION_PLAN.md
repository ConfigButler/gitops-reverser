# Implementation Plan: One Git Worker Per (Repo, Branch) Combination

## Executive Summary

**Objective**: Refactor to "one worker per (GitRepoConfig, Branch)" to ensure proper branch isolation while allowing multiple GitDestinations with different baseFolders to safely share the same branch.

**Core Insight**: The **branch** is the unit of serialization (to avoid merge conflicts). Multiple GitDestinations writing to the same branch must coordinate through a shared worker.

**Impact**: High (architectural restructuring of worker management)

**Risk**: Medium (isolated change, clear boundaries)

**Benefits**:
- ✅ **Merge conflict prevention**: One worker per branch ensures serialized commits
- ✅ **BaseFolder flexibility**: Multiple destinations can write to different folders in same branch
- ✅ **Simpler events**: Branch in worker context, only BaseFolder in events
- ✅ **Parallel branches**: Different branches process simultaneously
- ✅ **Natural lifecycle**: Workers created/destroyed based on active destinations

---

## Architectural Vision

### The Merge Conflict Problem

```yaml
# ❌ WRONG: One worker per GitDestination
GitDestination "prod-apps":  repo=shared, branch=main, baseFolder=apps/
  → Worker A: commits to main

GitDestination "prod-infra": repo=shared, branch=main, baseFolder=infra/
  → Worker B: commits to main  ← CONFLICT! Both push to main simultaneously

Result: Merge conflicts, lost commits
```

```yaml
# ✅ CORRECT: One worker per (repo, branch)
GitDestination "prod-apps":  repo=shared, branch=main, baseFolder=apps/
  ↓
  → Shared Worker: branch=main
  ↑
GitDestination "prod-infra": repo=shared, branch=main, baseFolder=infra/

Result: Serialized commits to main, baseFolders keep files separated
```

### Target Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ WorkerManager (in main.go)                                       │
│                                                                  │
│ Worker Key: "namespace/repo-name/branch"                        │
│                                                                  │
│ Monitors GitDestination CRs:                                    │
│   └─ Creates/reuses workers based on (repo, branch)             │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ Worker: "default/shared-repo/main"                               │
│                                                                  │
│ Shared by 2 GitDestinations:                                    │
│   • "prod-apps"  → baseFolder: apps/                            │
│   • "prod-infra" → baseFolder: infra/                           │
│                                                                  │
│ Worker Context:                                                  │
│   GitRepoConfig: shared-repo                                     │
│   Branch:        main                                            │
│   Checkout:      /tmp/workers/default/shared-repo/main/         │
│                                                                  │
│ Event Queue:                                                     │
│   ├─ {Object, Identifier, Operation, UserInfo, BaseFolder}     │
│   │   ↑ BaseFolder varies per GitDestination                   │
│   └─ SEED_SYNC: baseFolder="" (special: all folders)           │
│                                                                  │
│ ✓ Serializes commits from both destinations                    │
│ ✓ No merge conflicts                                            │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ Worker: "default/shared-repo/dev"                                │
│                                                                  │
│ Shared by 1 GitDestination:                                     │
│   • "dev-env" → baseFolder: dev-cluster/                        │
│                                                                  │
│ ✓ Processes in parallel with "main" worker                     │
│ ✓ No coordination needed (different branch)                    │
└──────────────────────────────────────────────────────────────────┘
```

---

## Data Structure Design

### SimplifiedEvent (minimal, but includes BaseFolder)
```go
// Events need BaseFolder but NOT branch
type SimplifiedEvent struct {
    Object     *unstructured.Unstructured
    Identifier types.ResourceIdentifier  
    Operation  string
    UserInfo   UserInfo
    BaseFolder string  // Which subfolder within worker's branch
}

// Branch comes from worker context!
```

### BranchWorker (one per repo+branch)
```go
// Worker owns a (repo, branch) combination
type BranchWorker struct {
    // Identity (unique key)
    GitRepoConfigRef        string
    GitRepoConfigNamespace  string
    Branch                  string
    
    // Metadata
    activeDestinations map[types.NamespacedName]string  // dest → baseFolder mapping
    
    // Processing
    eventQueue chan SimplifiedEvent
    // ...
}
```

### WorkerManager (tracks repo+branch workers)
```go
type WorkerManager struct {
    mu      sync.RWMutex
    workers map[BranchKey]*BranchWorker
}

type BranchKey struct {
    RepoNamespace string
    RepoName      string
    Branch        string
}
```

---

## Implementation Phases

### Phase 1: Foundation - New Types

#### 1.1: Define BranchKey
**New File**: `internal/git/types.go`

```go
package git

import "fmt"

// BranchKey uniquely identifies a (GitRepoConfig, Branch) combination.
// This is the unit of worker ownership to prevent merge conflicts.
type BranchKey struct {
    RepoNamespace string
    RepoName      string
    Branch        string
}

// String returns a string representation for logging.
func (k BranchKey) String() string {
    return fmt.Sprintf("%s/%s/%s", k.RepoNamespace, k.RepoName, k.Branch)
}

// SimplifiedEvent is an event with minimal context.
// Branch comes from worker, BaseFolder comes from event.
type SimplifiedEvent struct {
    Object     *unstructured.Unstructured
    Identifier types.ResourceIdentifier
    Operation  string  // CREATE, UPDATE, DELETE, SEED_SYNC
    UserInfo   eventqueue.UserInfo
    BaseFolder string  // Subfolder within branch (from GitDestination)
}

// IsControlEvent returns true for control events like SEED_SYNC.
func (e SimplifiedEvent) IsControlEvent() bool {
    return e.Operation == "SEED_SYNC"
}
```

**Testing**:
- [ ] BranchKey equality and hashing
- [ ] SimplifiedEvent construction
- [ ] Control event detection

---

#### 1.2: Create BranchWorker
**New File**: `internal/git/branch_worker.go`

```go
package git

import (
    "context"
    "fmt"
    "sync"
    "time"
    
    "github.com/go-logr/logr"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
    
    configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

const (
    branchWorkerQueueSize = 100
)

// BranchWorker processes events for a single (GitRepoConfig, Branch) combination.
// It can serve multiple GitDestinations that write to different baseFolders in the same branch.
type BranchWorker struct {
    // Identity (immutable)
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    
    // Dependencies
    Client client.Client
    Log    logr.Logger
    
    // Tracking active GitDestinations using this worker
    destMu             sync.RWMutex
    activeDestinations map[types.NamespacedName]string  // dest name → baseFolder
    
    // Event processing
    eventQueue chan SimplifiedEvent
    ctx        context.Context
    cancelFunc context.CancelFunc
    wg         sync.WaitGroup
    started    bool
    mu         sync.Mutex
}

// NewBranchWorker creates a worker for a (repo, branch) combination.
func NewBranchWorker(
    client client.Client,
    log logr.Logger,
    repoName, repoNamespace string,
    branch string,
) *BranchWorker {
    return &BranchWorker{
        GitRepoConfigRef:       repoName,
        GitRepoConfigNamespace: repoNamespace,
        Branch:                 branch,
        Client:                 client,
        Log: log.WithValues(
            "repo", repoName,
            "namespace", repoNamespace,
            "branch", branch,
        ),
        activeDestinations: make(map[types.NamespacedName]string),
        eventQueue:         make(chan SimplifiedEvent, branchWorkerQueueSize),
    }
}

// RegisterDestination adds a GitDestination to this worker's tracking.
// Multiple destinations can register if they share the same (repo, branch).
func (w *BranchWorker) RegisterDestination(destName, destNamespace, baseFolder string) {
    w.destMu.Lock()
    defer w.destMu.Unlock()
    
    key := types.NamespacedName{Name: destName, Namespace: destNamespace}
    w.activeDestinations[key] = baseFolder
    
    w.Log.Info("GitDestination registered with worker",
        "destination", destName,
        "baseFolder", baseFolder,
        "totalDestinations", len(w.activeDestinations))
}

// UnregisterDestination removes a GitDestination from tracking.
// Returns true if this was the last destination (worker can be destroyed).
func (w *BranchWorker) UnregisterDestination(destName, destNamespace string) bool {
    w.destMu.Lock()
    defer w.destMu.Unlock()
    
    key := types.NamespacedName{Name: destName, Namespace: destNamespace}
    delete(w.activeDestinations, key)
    
    isEmpty := len(w.activeDestinations) == 0
    
    w.Log.Info("GitDestination unregistered from worker",
        "destination", destName,
        "remainingDestinations", len(w.activeDestinations),
        "canDestroy", isEmpty)
    
    return isEmpty
}

// Start begins processing events.
func (w *BranchWorker) Start(parentCtx context.Context) error {
    w.mu.Lock()
    if w.started {
        w.mu.Unlock()
        return fmt.Errorf("worker already started")
    }
    w.ctx, w.cancelFunc = context.WithCancel(parentCtx)
    w.started = true
    w.mu.Unlock()
    
    w.Log.Info("Starting branch worker")
    
    w.wg.Add(1)
    go func() {
        defer w.wg.Done()
        w.processEvents()
    }()
    
    return nil
}

// Stop gracefully shuts down the worker.
func (w *BranchWorker) Stop() {
    w.mu.Lock()
    if !w.started {
        w.mu.Unlock()
        return
    }
    w.mu.Unlock()
    
    w.Log.Info("Stopping branch worker")
    w.cancelFunc()
    w.wg.Wait()
    w.Log.Info("Branch worker stopped")
}

// Enqueue adds an event to this worker's queue.
func (w *BranchWorker) Enqueue(event SimplifiedEvent) {
    select {
    case w.eventQueue <- event:
        w.Log.V(1).Info("Event enqueued",
            "operation", event.Operation,
            "baseFolder", event.BaseFolder)
    default:
        w.Log.Error(nil, "Event queue full, event dropped",
            "operation", event.Operation,
            "baseFolder", event.BaseFolder)
    }
}

// processEvents is the main event processing loop.
func (w *BranchWorker) processEvents() {
    // Get GitRepoConfig
    repoConfig, err := w.getGitRepoConfig(w.ctx)
    if err != nil {
        w.Log.Error(err, "Failed to get GitRepoConfig, worker exiting")
        return
    }
    
    // Setup timing
    pushInterval := w.getPushInterval(repoConfig)
    maxCommits := w.getMaxCommits(repoConfig)
    ticker := time.NewTicker(pushInterval)
    defer ticker.Stop()
    
    var eventBuffer []SimplifiedEvent
    var bufferByteCount int64
    
    // Track live paths PER baseFolder for orphan detection
    sLivePathsByFolder := make(map[string]map[string]struct{})
    
    for {
        select {
        case <-w.ctx.Done():
            w.handleShutdown(repoConfig, eventBuffer)
            return
            
        case event := <-w.eventQueue:
            if event.Operation == "SEED_SYNC" {
                // SEED_SYNC with baseFolder="" means "all registered baseFolders"
                w.handleSeedSync(repoConfig, eventBuffer, sLivePathsByFolder)
                eventBuffer = nil
                bufferByteCount = 0
                sLivePathsByFolder = make(map[string]map[string]struct{})
            } else {
                // Track live paths per baseFolder
                if event.BaseFolder != "" {
                    if sLivePathsByFolder[event.BaseFolder] == nil {
                        sLivePathsByFolder[event.BaseFolder] = make(map[string]struct{})
                    }
                    path := event.Identifier.ToGitPath()
                    sLivePathsByFolder[event.BaseFolder][path] = struct{}{}
                }
                
                // Buffer event
                eventBuffer = append(eventBuffer, event)
                bufferByteCount += w.estimateEventSize(event)
                
                // Check limits
                if len(eventBuffer) >= maxCommits || bufferByteCount >= maxBytesBytes {
                    w.commitAndPush(repoConfig, eventBuffer)
                    eventBuffer = nil
                    bufferByteCount = 0
                }
            }
            
        case <-ticker.C:
            if len(eventBuffer) > 0 {
                w.commitAndPush(repoConfig, eventBuffer)
                eventBuffer = nil
                bufferByteCount = 0
            }
        }
    }
}

// handleSeedSync processes SEED_SYNC and computes orphans for all registered baseFolders.
func (w *BranchWorker) handleSeedSync(
    repoConfig *configv1alpha1.GitRepoConfig,
    eventBuffer []SimplifiedEvent,
    sLivePathsByFolder map[string]map[string]struct{},
) {
    // Get all registered baseFolders
    w.destMu.RLock()
    baseFolders := make([]string, 0, len(w.activeDestinations))
    for _, folder := range w.activeDestinations {
        baseFolders = append(baseFolders, folder)
    }
    w.destMu.RUnlock()
    
    // Compute orphans for each baseFolder
    var allDeletes []SimplifiedEvent
    for _, baseFolder := range baseFolders {
        sLive := sLivePathsByFolder[baseFolder]
        if sLive == nil {
            sLive = make(map[string]struct{})
        }
        deletes := w.computeOrphanDeletes(repoConfig, baseFolder, sLive)
        allDeletes = append(allDeletes, deletes...)
    }
    
    // Combine buffered events with orphan deletes and commit
    if len(allDeletes) > 0 {
        combined := append(eventBuffer, allDeletes...)
        w.commitAndPush(repoConfig, combined)
    } else if len(eventBuffer) > 0 {
        w.commitAndPush(repoConfig, eventBuffer)
    }
}

// computeOrphanDeletes finds orphans within a specific baseFolder.
func (w *BranchWorker) computeOrphanDeletes(
    repoConfig *configv1alpha1.GitRepoConfig,
    baseFolder string,
    sLive map[string]struct{},
) []SimplifiedEvent {
    // List files in THIS worker's branch
    paths, err := w.listRepoYAMLPaths(w.ctx, repoConfig)
    if err != nil {
        w.Log.Error(err, "Failed to list repo files")
        return nil
    }
    
    // Filter to specified baseFolder
    var relevantPaths []string
    if baseFolder != "" {
        prefix := baseFolder + "/"
        for _, p := range paths {
            if strings.HasPrefix(p, prefix) {
                relevantPaths = append(relevantPaths, p)
            }
        }
    } else {
        relevantPaths = paths  // SEED_SYNC with empty baseFolder = all paths
    }
    
    // Find orphans
    var orphans []string
    for _, p := range relevantPaths {
        if _, ok := sLive[p]; !ok {
            orphans = append(orphans, p)
        }
    }
    
    // Convert to SimplifiedEvents with baseFolder
    events := make([]SimplifiedEvent, 0, len(orphans))
    for _, p := range orphans {
        id, ok := parseIdentifierFromPath(p)
        if !ok {
            continue
        }
        events = append(events, SimplifiedEvent{
            Object:     nil,
            Identifier: id,
            Operation:  "DELETE",
            UserInfo:   eventqueue.UserInfo{},
            BaseFolder: baseFolder,  // Keep folder context
        })
    }
    
    return events
}

// commitAndPush processes a batch of events.
// Events may have different baseFolders but all go to same branch.
func (w *BranchWorker) commitAndPush(
    repoConfig *configv1alpha1.GitRepoConfig,
    events []SimplifiedEvent,
) {
    log := w.Log.WithValues("eventCount", len(events))
    
    log.Info("Starting git commit and push",
        "branch", w.Branch)  // Branch from worker context
    
    // Get auth
    auth, err := w.getAuthFromSecret(w.ctx, repoConfig)
    if err != nil {
        log.Error(err, "Failed to get auth")
        return
    }
    
    // Clone to worker-specific path (one per branch for isolation)
    repoPath := filepath.Join("/tmp", "gitops-reverser-workers",
        w.GitRepoConfigNamespace, w.GitRepoConfigRef, w.Branch)
    
    repo, err := Clone(repoConfig.Spec.RepoURL, repoPath, auth)
    if err != nil {
        log.Error(err, "Failed to clone repository")
        return
    }
    
    // Checkout THIS worker's branch (explicit, no guessing!)
    if err := repo.Checkout(w.Branch); err != nil {
        log.Error(err, "Failed to checkout branch", "branch", w.Branch)
        return
    }
    
    // Convert to full events (add branch from worker context)
    fullEvents := w.convertToFullEvents(events)
    
    if err := repo.TryPushCommits(w.ctx, fullEvents); err != nil {
        log.Error(err, "Failed to push commits")
        return
    }
    
    log.Info("Successfully pushed commits")
}

// convertToFullEvents adds branch from worker context.
func (w *BranchWorker) convertToFullEvents(simplified []SimplifiedEvent) []eventqueue.Event {
    full := make([]eventqueue.Event, len(simplified))
    for i, s := range simplified {
        full[i] = eventqueue.Event{
            Object:     s.Object,
            Identifier: s.Identifier,
            Operation:  s.Operation,
            UserInfo:   s.UserInfo,
            Branch:     w.Branch,     // Worker context!
            BaseFolder: s.BaseFolder, // Event carries this
        }
    }
    return full
}
```

**Testing**:
- [ ] Worker processes events from multiple baseFolders
- [ ] Commits serialized (no parallel commits to same branch)
- [ ] Branch always explicit

---

#### 1.3: Create WorkerManager
**New File**: `internal/git/worker_manager.go`

```go
package git

import (
    "context"
    "fmt"
    "sync"
    
    "github.com/go-logr/logr"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkerManager manages BranchWorkers.
// Creates workers per (repo, branch), shared by multiple GitDestinations.
type WorkerManager struct {
    Client client.Client
    Log    logr.Logger
    
    mu      sync.RWMutex
    workers map[BranchKey]*BranchWorker
    ctx     context.Context
}

// NewWorkerManager creates a new worker manager.
func NewWorkerManager(client client.Client, log logr.Logger) *WorkerManager {
    return &WorkerManager{
        Client:  client,
        Log:     log,
        workers: make(map[BranchKey]*BranchWorker),
    }
}

// RegisterDestination ensures a worker exists for the destination's (repo, branch)
// and registers the destination with that worker.
func (m *WorkerManager) RegisterDestination(
    ctx context.Context,
    destName, destNamespace string,
    repoName, repoNamespace string,
    branch, baseFolder string,
) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    key := BranchKey{
        RepoNamespace: repoNamespace,
        RepoName:      repoName,
        Branch:        branch,
    }
    
    // Get or create worker for this (repo, branch)
    worker, exists := m.workers[key]
    if !exists {
        m.Log.Info("Creating new branch worker", "key", key.String())
        worker = NewBranchWorker(m.Client, m.Log.WithName("branch-worker"),
            repoName, repoNamespace, branch)
        
        if err := worker.Start(m.ctx); err != nil {
            return fmt.Errorf("failed to start worker for %s: %w", key, err)
        }
        
        m.workers[key] = worker
    }
    
    // Register this destination with the worker
    worker.RegisterDestination(destName, destNamespace, baseFolder)
    
    m.Log.Info("GitDestination registered with branch worker",
        "destination", fmt.Sprintf("%s/%s", destNamespace, destName),
        "workerKey", key.String(),
        "baseFolder", baseFolder)
    
    return nil
}

// UnregisterDestination removes a GitDestination from its worker.
// Destroys the worker if it was the last destination using it.
func (m *WorkerManager) UnregisterDestination(
    destName, destNamespace string,
    repoName, repoNamespace string,
    branch string,
) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    key := BranchKey{
        RepoNamespace: repoNamespace,
        RepoName:      repoName,
        Branch:        branch,
    }
    
    worker, exists := m.workers[key]
    if !exists {
        return nil // Worker already gone
    }
    
    // Unregister destination from worker
    isEmpty := worker.UnregisterDestination(destName, destNamespace)
    
    // If no more destinations use this worker, destroy it
    if isEmpty {
        m.Log.Info("Last destination unregistered, destroying worker", "key", key.String())
        worker.Stop()
        delete(m.workers, key)
    }
    
    return nil
}

// GetWorkerForDestination finds the worker for a destination's (repo, branch).
func (m *WorkerManager) GetWorkerForDestination(
    repoName, repoNamespace string,
    branch string,
) (*BranchWorker, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    key := BranchKey{
        RepoNamespace: repoNamespace,
        RepoName:      repoName,
        Branch:        branch,
    }
    
    worker, exists := m.workers[key]
    return worker, exists
}

// Start implements manager.Runnable.
func (m *WorkerManager) Start(ctx context.Context) error {
    m.ctx = ctx
    m.Log.Info("WorkerManager started")
    
    <-ctx.Done()
    
    m.Log.Info("WorkerManager shutting down")
    m.mu.Lock()
    defer m.mu.Unlock()
    
    for key, worker := range m.workers {
        m.Log.Info("Stopping worker for shutdown", "key", key.String())
        worker.Stop()
    }
    
    m.workers = make(map[BranchKey]*BranchWorker)
    m.Log.Info("WorkerManager stopped")
    return nil
}

// NeedLeaderElection ensures only leader manages workers.
func (m *WorkerManager) NeedLeaderElection() bool {
    return true
}
```

**Testing**:
- [ ] Multiple destinations can register with same worker
- [ ] Worker created on first destination registration
- [ ] Worker destroyed when last destination unregisters
- [ ] Thread safety

---

### Phase 2: Controller Integration

#### 2.1: Update GitDestination Controller
**File**: [`internal/controller/gitdestination_controller.go`](../../internal/controller/gitdestination_controller.go)

**Changes**:

1. **Add WorkerManager** (line 46-51):
```go
type GitDestinationReconciler struct {
    client.Client
    Scheme        *runtime.Scheme
    WorkerManager *git.WorkerManager  // NEW
}
```

2. **Register with worker when Ready** (after line 127):
```go
r.setCondition(&dest, metav1.ConditionTrue, GitDestinationReasonReady, msg)
dest.Status.ObservedGeneration = dest.Generation

// NEW: Register with branch worker
if r.WorkerManager != nil {
    if err := r.WorkerManager.RegisterDestination(
        ctx,
        dest.Name, dest.Namespace,
        dest.Spec.RepoRef.Name, repoNS,
        dest.Spec.Branch,
        dest.Spec.BaseFolder,
    ); err != nil {
        log.Error(err, "Failed to register destination with worker")
    } else {
        log.Info("Registered destination with branch worker",
            "repo", dest.Spec.RepoRef.Name,
            "branch", dest.Spec.Branch,
            "baseFolder", dest.Spec.BaseFolder)
    }
}
```

3. **Unregister when deleted** (after line 66):
```go
if client.IgnoreNotFound(err) == nil {
    log.Info("GitDestination not found, was likely deleted")
    
    // NEW: Need to unregister - but we need the spec values!
    // Fetch from cache/finalizer or accept best-effort cleanup
    if r.WorkerManager != nil {
        // This requires adding a finalizer to track deletion
        // OR accepting that worker cleanup happens when all destinations gone
        // For now, log warning
        log.Info("GitDestination deleted but cannot unregister without spec values - worker will clean up when idle")
    }
    
    return ctrl.Result{}, nil
}
```

**Dependencies**: Phase 1

**Testing**:
- [ ] Multiple destinations register with same worker
- [ ] Worker shared correctly
- [ ] Cleanup when destination deleted

---

#### 2.2: Add Finalizer for Cleanup
**File**: [`internal/controller/gitdestination_controller.go`](../../internal/controller/gitdestination_controller.go)

**Add finalizer** to properly unregister on deletion:

```go
const gitDestinationFinalizer = "configbutler.ai/worker-cleanup"

// In Reconcile, before validation:
if dest.ObjectMeta.DeletionTimestamp.IsZero() {
    // Add finalizer if not present
    if !controllerutil.ContainsFinalizer(&dest, gitDestinationFinalizer) {
        controllerutil.AddFinalizer(&dest, gitDestinationFinalizer)
        if err := r.Update(ctx, &dest); err != nil {
            return ctrl.Result{}, err
        }
    }
} else {
    // Deletion - unregister and remove finalizer
    if controllerutil.ContainsFinalizer(&dest, gitDestinationFinalizer) {
        if r.WorkerManager != nil {
            _ = r.WorkerManager.UnregisterDestination(
                dest.Name, dest.Namespace,
                dest.Spec.RepoRef.Name, repoNS,
                dest.Spec.Branch,
            )
        }
        
        controllerutil.RemoveFinalizer(&dest, gitDestinationFinalizer)
        if err := r.Update(ctx, &dest); err != nil {
            return ctrl.Result{}, err
        }
    }
    return ctrl.Result{}, nil
}
```

**Testing**:
- [ ] Finalizer added on creation
- [ ] Worker unregistered on deletion
- [ ] Finalizer removed after cleanup

---

### Phase 3: Rule Store Updates

#### 3.1: Store GitDestination Reference in Rules
**File**: [`internal/rulestore/store.go`](../../internal/rulestore/store.go:35-52)

```go
type CompiledRule struct {
    Source types.NamespacedName  // WatchRule name
    
    // NEW: GitDestination (for event routing)
    GitDestinationRef       string
    GitDestinationNamespace string
    
    // Resolved values (from GitDestination)
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    BaseFolder             string
    IsClusterScoped        bool
    ResourceRules          []CompiledResourceRule
}

// Same for CompiledClusterRule
```

---

#### 3.2: Update AddOrUpdateWatchRule
**File**: [`internal/rulestore/store.go`](../../internal/rulestore/store.go:120-155)

```go
func (s *RuleStore) AddOrUpdateWatchRule(
    rule configv1alpha1.WatchRule,
    gitDestinationName string,      // NEW
    gitDestinationNamespace string, // NEW
    gitRepoConfigName string,
    gitRepoConfigNamespace string,
    branch string,
    baseFolder string,
) {
    // Store GitDestination ref for event routing
    compiled := CompiledRule{
        Source:                  key,
        GitDestinationRef:       gitDestinationName,      // NEW
        GitDestinationNamespace: gitDestinationNamespace, // NEW
        GitRepoConfigRef:        gitRepoConfigName,
        GitRepoConfigNamespace:  gitRepoConfigNamespace,
        Branch:                  branch,
        BaseFolder:              baseFolder,
        // ...
    }
}
```

---

#### 3.3: Update Controller Calls
**File**: [`internal/controller/watchrule_controller.go`](../../internal/controller/watchrule_controller.go:189-190)

```go
// Line 189-190:
r.RuleStore.AddOrUpdateWatchRule(
    *watchRule,
    dest.Name, destNS,  // GitDestination ref
    grc.Name, grcNS,    // GitRepoConfig ref
    dest.Spec.Branch,
    dest.Spec.BaseFolder,
)
```

Same for ClusterWatchRule controller.

---

### Phase 4: Event Routing

#### 4.1: Create EventRouter
**New File**: `internal/git/event_router.go`

```go
package git

// EventRouter dispatches events to the correct BranchWorker.
type EventRouter struct {
    WorkerManager *WorkerManager
    Log           logr.Logger
}

// RouteEvent sends an event to the worker for (repo, branch).
// Destination info is used to lookup the worker, then event is queued.
func (r *EventRouter) RouteEvent(
    repoName, repoNamespace string,
    branch string,
    event SimplifiedEvent,
) error {
    worker, exists := r.WorkerManager.GetWorkerForDestination(
        repoName, repoNamespace, branch,
    )
    
    if !exists {
        return fmt.Errorf("no worker for repo=%s/%s branch=%s",
            repoNamespace, repoName, branch)
    }
    
    worker.Enqueue(event)
    return nil
}
```

**Testing**: Event routing to correct worker

---

### Phase 5: Watch Manager Updates

#### 5.1: Update Watch Manager Structure
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:55-65)

```go
type Manager struct {
    Client           client.Client
    Log              logr.Logger
    RuleStore        *rulestore.RuleStore
    EventRouter      *git.EventRouter  // NEW (replaces EventQueue)
    CorrelationStore *correlation.Store
    // ...
}
```

---

#### 5.2: Update Event Creation
**File**: [`internal/watch/informers.go`](../../internal/watch/informers.go:107-136)

```go
// WatchRule matches:
for _, rule := range wrRules {
    ev := git.SimplifiedEvent{
        Object:     sanitized.DeepCopy(),
        Identifier: id,
        Operation:  string(op),
        UserInfo:   userInfo,
        BaseFolder: rule.BaseFolder,  // From rule
    }
    
    // Route using (repo, branch) to find worker
    if err := m.EventRouter.RouteEvent(
        rule.GitRepoConfigRef,
        rule.GitRepoConfigNamespace,
        rule.Branch,
        ev,
    ); err != nil {
        m.Log.V(1).Info("Failed to route event", "error", err)
    }
}

// Same for ClusterWatchRule
```

---

#### 5.3: Track Destinations During Seed
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:565-601)

```go
// Track (repo, branch) combinations seen during seed
type BranchContext struct {
    RepoName      string
    RepoNamespace string
    Branch        string
}
branchKeys := make(map[BranchKey]struct{})

// When processing matches:
for _, r := range wrRules {
    branchKeys[BranchKey{
        RepoNamespace: r.GitRepoConfigNamespace,
        RepoName:      r.GitRepoConfigRef,
        Branch:        r.Branch,
    }] = struct{}{}
}
```

---

#### 5.4: Emit SEED_SYNC Per Branch
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:603-615)

```go
func (m *Manager) emitSeedSyncControls(branchKeys map[BranchKey]struct{}) {
    for key := range branchKeys {
        ev := git.SimplifiedEvent{
            Operation:  "SEED_SYNC",
            BaseFolder: "",  // Empty = all baseFolders for this branch
        }
        
        // Route to worker for this (repo, branch)
        if err := m.EventRouter.RouteEvent(
            key.RepoName, key.RepoNamespace, key.Branch, ev,
        ); err != nil {
            m.Log.Error(err, "Failed to route SEED_SYNC", "key", key.String())
        }
    }
}
```

---

## Edge Cases & Solutions

### Edge Case 1: Same Repo+Branch, Different Namespaces

```yaml
# Namespace: prod
GitDestination "prod-dest": repo=shared, branch=main, baseFolder=prod/

# Namespace: staging  
GitDestination "staging-dest": repo=shared, branch=main, baseFolder=staging/
```

**Issue**: Different namespaces, same repo+branch

**Solution**: Include namespace in BranchKey:
```go
type BranchKey struct {
    RepoNamespace string  // Differentiates cross-namespace repos
    RepoName      string
    Branch        string
}
```

**Result**: Two workers if repos in different namespaces, even if same name+branch

---

### Edge Case 2: Destination Spec Changes

```yaml
# User updates GitDestination from:
branch: main → branch: dev
```

**Solution**: Controller must:
1. Unregister from old worker (main)
2. Register with new worker (dev)

**Implementation**:
```go
// In GitDestination controller, detect spec changes:
if dest.Status.ObservedGeneration != dest.Generation {
    // Spec changed - need to re-register
    oldBranch := getPreviousBranch(dest)  // From status or annotation
    
    if oldBranch != "" && oldBranch != dest.Spec.Branch {
        // Unregister from old branch worker
        _ = r.WorkerManager.UnregisterDestination(...)
    }
    
    // Register with new branch worker
    _ = r.WorkerManager.RegisterDestination(...)
}
```

---

### Edge Case 3: Overlapping BaseFolders

```yaml
GitDestination "broad": repo=shared, branch=main, baseFolder=clusters/
GitDestination "narrow": repo=shared, branch=main, baseFolder=clusters/prod/
```

**Issue**: Both write to same branch, baseFolders overlap

**Solution**: This is ALLOWED - worker processes all events, file paths don't conflict:
- "broad" creates: `clusters/resource1.yaml`
- "narrow" creates: `clusters/prod/resource2.yaml`

**Orphan detection**: Each baseFolder tracked separately in `sLivePathsByFolder`

---

### Edge Case 4: Empty BaseFolder

```yaml
GitDestination "root": repo=shared, branch=main, baseFolder=""
```

**Meaning**: Write to repository root

**Solution**: 
- Worker accepts `baseFolder = ""`
- Path construction: if baseFolder empty, use identifier path directly
- Orphan detection: empty baseFolder = consider all paths in branch

---

## Implementation Order

```
Phase 1: Foundation
  ├─ 1.1: BranchKey & SimplifiedEvent types
  ├─ 1.2: BranchWorker implementation
  └─ 1.3: WorkerManager implementation

Phase 2: Controller Integration  
  ├─ 2.1: Add WorkerManager to GitDestination controller
  └─ 2.2: Add finalizer for cleanup

Phase 3: Rule Store
  ├─ 3.1: Add GitDestination refs to CompiledRule
  ├─ 3.2: Update AddOrUpdateWatchRule
  └─ 3.3: Update controller calls

Phase 4: Event Routing
  └─ 4.1: Create EventRouter

Phase 5: Watch Manager
  ├─ 5.1: Replace EventQueue with EventRouter
  ├─ 5.2: Update event creation
  ├─ 5.3: Track branch keys during seed
  └─ 5.4: Emit SEED_SYNC per branch

Phase 6: Main Setup
  └─ 6.1: Initialize WorkerManager, wire dependencies

Phase 7: Testing
  ├─ 7.1: Unit tests
  ├─ 7.2: Integration tests (multi-destination scenarios)
  └─ 7.3: E2E tests (edge cases)
```

---

## Validation Checklist

### Correctness
- [ ] Workers keyed by (repo, branch) not (destination)
- [ ] Multiple destinations can share one worker
- [ ] Worker tracks all active destinations (baseFolders)
- [ ] No "invalid reference name" errors
- [ ] Branch always explicit in worker context
- [ ] BaseFolder in events (not branch)
- [ ] SEED_SYNC processed correctly per branch
- [ ] Orphan detection scoped per baseFolder within branch

### Edge Cases
- [ ] Overlapping baseFolders handled correctly
- [ ] Empty baseFolder works (repository root)
- [ ] Cross-namespace repos differentiated
- [ ] Destination spec changes (branch update) handled
- [ ] Last destination unregistering destroys worker
- [ ] First destination registering creates worker

### Performance
- [ ] Workers shared efficiently (no duplicate workers)
- [ ] Parallel processing between different branches
- [ ] Serialized commits within same branch (no conflicts)
- [ ] Worker cleanup when destinations deleted

---

## Success Criteria

1. ✅ One worker per (repo, branch), shared by multiple destinations
2. ✅ Events have BaseFolder but NOT Branch
3. ✅ No merge conflicts between same-branch destinations
4. ✅ Parallel processing between different branches
5. ✅ SEED_SYNC and orphan detection work correctly
6. ✅ All edge cases handled
7. ✅ All tests pass
8. ✅ No "invalid reference name" errors

---

## Post-Implementation

### Documentation
- [ ] Architecture diagram showing worker sharing
- [ ] Document (repo, branch) as serialization boundary
- [ ] Explain baseFolder as isolation within branch
- [ ] Add edge case handling to CONTRIBUTING.md

### Metrics
- [ ] Track workers by (repo, branch) key
- [ ] Monitor destinations per worker
- [ ] Alert on worker creation/destruction

Ready for implementation!