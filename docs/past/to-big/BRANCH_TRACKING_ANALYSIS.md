# Branch Tracking Analysis: Event Flow and Branch Loss

## Problem Statement

During e2e tests, we observe:
```
ERROR git-worker Failed to checkout branch {"repo": "gitrepoconfig-configmap-test", "error": "invalid reference name"}
```

This occurs because certain events lack Branch information when they reach [`commitAndPush()`](../../internal/git/worker.go:484).

## Event Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           EVENT SOURCES                                     │
└─────────────────────────────────────────────────────────────────────────────┘

1. WATCH PATH (has branch from rules)
   ┌──────────────────────────────────────┐
   │ Informer detects resource change     │
   │ ├─ handleEvent()                     │
   │ └─ Matches WatchRule/ClusterWatchRule│
   └──────────┬───────────────────────────┘
              │
              v
   ┌──────────────────────────────────────┐
   │ Create Event with:                   │
   │ ✓ Branch = rule.Branch               │ ← From matching rule
   │ ✓ GitRepoConfigRef                   │
   │ ✓ BaseFolder                         │
   └──────────┬───────────────────────────┘
              │
              v
         [Event Queue]


2. SEED SYNC PATH (missing branch!)
   ┌──────────────────────────────────────┐
   │ Watch Manager reconciliation         │
   │ └─ seedSelectedResources()           │
   └──────────┬───────────────────────────┘
              │
              v
   ┌──────────────────────────────────────┐
   │ List all resources matching rules    │
   │ └─ processListedObject()             │
   │    ├─ Enqueue UPDATE events          │
   │    │  ✓ Branch = rule.Branch         │ ← From matching rule
   │    └─ Track repoKeys for sync        │
   └──────────┬───────────────────────────┘
              │
              v
   ┌──────────────────────────────────────┐
   │ emitSeedSyncControls()               │
   │ └─ Create SEED_SYNC event            │
   │    ✗ NO Branch field!                │ ← PROBLEM #1
   │    ✓ GitRepoConfigRef only           │
   └──────────┬───────────────────────────┘
              │
              v
         [Event Queue]


3. ORPHAN DELETION PATH (missing branch!)
   ┌──────────────────────────────────────┐
   │ Worker processes SEED_SYNC           │
   │ └─ handleIncomingEvent()             │
   └──────────┬───────────────────────────┘
              │
              v
   ┌──────────────────────────────────────┐
   │ computeOrphanDeletes()               │
   │ ├─ List Git repo files               │
   │ ├─ Compare with S_live               │
   │ └─ Create DELETE events              │
   │    ✗ NO Branch field!                │ ← PROBLEM #2
   │    ✓ GitRepoConfigRef only           │
   └──────────┬───────────────────────────┘
              │
              v
         [Event Queue]
```

## Event Batching Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        EVENT QUEUE DISPATCH                                 │
└─────────────────────────────────────────────────────────────────────────────┘

Central Queue
     │
     │ Batch by: namespace/name of GitRepoConfig
     │           (NOT by branch!)
     v
┌──────────────────────────────────────────┐
│ Repo Queue: sut/my-gitrepoconfig         │
│                                          │
│ Event 1: Branch = "main"    (UPDATE)    │ ← From WatchRule
│ Event 2: Branch = "dev"     (UPDATE)    │ ← From different WatchRule
│ Event 3: Branch = ""        (SEED_SYNC) │ ← Control event
│ Event 4: Branch = ""        (DELETE)    │ ← Orphan deletion
└──────────────────────────────────────────┘
     │
     │ commitAndPush() extracts branch from events[0]
     │ ❌ If events[0].Branch == "" → "invalid reference name"
     v
┌──────────────────────────────────────────┐
│ Git Operations (needs single branch)    │
│ └─ Checkout(branch)                     │
└──────────────────────────────────────────┘
```

## The Fundamental Issue

**GitRepoConfig can be referenced by multiple GitDestinations with DIFFERENT branches:**

```yaml
# GitRepoConfig (shared)
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: shared-repo
spec:
  repoURL: https://github.com/org/repo.git
  allowedBranches: ["main", "dev", "staging"]

---
# GitDestination 1 → Branch: main
apiVersion: configbutler.ai/v1alpha1
kind: GitDestination
metadata:
  name: prod-dest
spec:
  repoRef: {name: shared-repo}
  branch: main              ← Different branch
  baseFolder: clusters/prod

---
# GitDestination 2 → Branch: dev
apiVersion: configbutler.ai/v1alpha1
kind: GitDestination
metadata:
  name: dev-dest
spec:
  repoRef: {name: shared-repo}
  branch: dev               ← Different branch
  baseFolder: clusters/dev
```

**Current Event Batching**: Events are batched by `GitRepoConfigRef` (namespace/name), mixing events from different branches in the same batch!

## Current Worker Architecture (from cmd/main.go)

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           MAIN.GO SETUP                                     │
└─────────────────────────────────────────────────────────────────────────────┘

Line 120: eventQueue := eventqueue.NewQueue()        ← ONE global queue
Line 173: gitWorker := &git.Worker{                  ← ONE worker instance
              EventQueue: eventQueue,
          }
Line 178: mgr.Add(gitWorker)

Worker.Start() → dispatchEvents() creates repo queues dynamically:
  queueKey = "namespace/gitRepoConfigName"           ← PROBLEM: No branch!
  
  For each unique queueKey:
    - Create buffered channel
    - Spawn goroutine → processRepoEvents()
```

## The REAL Architectural Issue

**Current Model**: Queue per GitRepoConfig
```
GitRepoConfig: "my-repo"
  ├─ GitDestination "prod" → branch: main, baseFolder: clusters/prod
  ├─ GitDestination "dev"  → branch: dev,  baseFolder: clusters/dev
  └─ GitDestination "test" → branch: test, baseFolder: clusters/test

Queue Key: "namespace/my-repo"  ← All 3 destinations mixed!
└─ Event 1: branch=main,  baseFolder=clusters/prod
└─ Event 2: branch=dev,   baseFolder=clusters/dev
└─ Event 3: branch=test,  baseFolder=clusters/test
└─ Event 4: branch=""     (SEED_SYNC) ← Which branch???
```

**Correct Model**: Queue per GitDestination (repo+branch+baseFolder)
```
GitDestination "prod" → Queue: "namespace/prod"
  ├─ All events: branch=main, baseFolder=clusters/prod
  └─ SEED_SYNC knows: branch=main, baseFolder=clusters/prod

GitDestination "dev" → Queue: "namespace/dev"  
  ├─ All events: branch=dev, baseFolder=clusters/dev
  └─ SEED_SYNC knows: branch=dev, baseFolder=clusters/dev
```

## Recommended Solution: Queue by GitDestination

### Changes Required

#### 1. Event Structure - Add GitDestination reference
**File**: [`internal/eventqueue/queue.go`](../../internal/eventqueue/queue.go)
```go
type Event struct {
    // ... existing fields ...
    
    // NEW: Track source GitDestination (provides branch+baseFolder context)
    GitDestinationRef       string
    GitDestinationNamespace string
    
    // KEEP: Still need these for backward compatibility and repo access
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string  // Redundant with GitDestination but kept for clarity
    BaseFolder             string  // Redundant with GitDestination but kept for clarity
}
```

#### 2. Event Creation - Populate GitDestination ref
**File**: [`internal/watch/informers.go`](../../internal/watch/informers.go) & [`internal/watch/manager.go`](../../internal/watch/manager.go)

Need to modify event creation in:
- `handleEvent()` (line 108-135 in informers.go)
- `enqueueMatches()` (line 228-254 in manager.go)

**Problem**: WatchRule/ClusterWatchRule don't currently store the GitDestination ref - only resolved values!

**Solution**: Change `CompiledRule` structure to track GitDestination source:

```go
// internal/rulestore/store.go
type CompiledRule struct {
    Source                 types.NamespacedName  // WatchRule name
    GitDestinationRef      string                // NEW: GitDestination name
    GitDestinationNamespace string               // NEW: GitDestination namespace
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Branch                 string
    BaseFolder             string
    // ... rest ...
}
```

#### 3. Controller Changes - Store GitDestination ref
**File**: [`internal/controller/watchrule_controller.go`](../../internal/controller/watchrule_controller.go:190)

```go
// OLD: 
r.RuleStore.AddOrUpdateWatchRule(*watchRule, grc.Name, grcNS, dest.Spec.Branch, dest.Spec.BaseFolder)

// NEW: Pass GitDestination ref too
r.RuleStore.AddOrUpdateWatchRule(*watchRule, 
    dest.Name, destNS,              // GitDestination ref
    grc.Name, grcNS,                 // GitRepoConfig ref  
    dest.Spec.Branch, 
    dest.Spec.BaseFolder)
```

#### 4. Queue Key - Change to GitDestination
**File**: [`internal/git/worker.go`](../../internal/git/worker.go:169)

```go
// OLD:
queueKey := event.GitRepoConfigNamespace + "/" + event.GitRepoConfigRef

// NEW:
queueKey := event.GitDestinationNamespace + "/" + event.GitDestinationRef
```

#### 5. SEED_SYNC - Track per GitDestination
**File**: [`internal/watch/manager.go`](../../internal/watch/manager.go:356-365)

```go
// OLD: Track by GitRepoConfig
repoKeys := make(map[k8stypes.NamespacedName]struct{})

// NEW: Track by GitDestination (has branch context)
destKeys := make(map[k8stypes.NamespacedName]DestinationContext)

type DestinationContext struct {
    GitDestinationRef       string
    GitDestinationNamespace string
    Branch                  string
    BaseFolder              string
}
```

### Benefits

- ✅ **Natural branch separation**: One queue per GitDestination = one branch context
- ✅ **SEED_SYNC inherently knows its branch**: Destination provides full context
- ✅ **Orphan detection properly scoped**: To specific branch/baseFolder
- ✅ **No guessing or fallbacks**: All events have complete context
- ✅ **Better isolation**: Different deployment targets completely separated
- ✅ **Clearer semantics**: Queue represents a logical deployment target

### Drawbacks

- Changes multiple components (event structure, controllers, worker, watch manager)
- Need comprehensive testing to ensure no regressions
- Migration requires careful coordination

## Alternative: Hybrid Approach

Keep GitRepoConfig-based queuing but always include GitDestination reference in events for context. Then split batches by branch before git operations. This is less clean but lower risk.

## Decision Point

**Recommended**: Implement GitDestination-based queueing (Option A above). This properly models the domain: each GitDestination represents a specific deployment target (repo+branch+folder), which should have its own isolated processing queue.

Should I proceed with this implementation?