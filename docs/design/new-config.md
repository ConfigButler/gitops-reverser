# Design Strategy: GitOps Reverser Operator

From Prototype to Ecosystem-Standard API

## 1. Executive Summary

The current API correctly separates concerns between "Rules" and "Git Configuration," but relies on ambiguous naming and shared structures that create validation conflicts and security risks.

The proposed future state aligns with the Kubernetes Resource Model (KRM) standards, adopting strictly typed references and clearer terminology ("Targets" and "Providers"). Crucially, it introduces a Polymorphic Architecture that allows users to seamlessly use either native Git configurations or existing FluxCD resources, positioning the operator as a first-class citizen in the modern GitOps stack.

## 2. Current State Analysis

### The Hierarchy

Currently, the operator uses a custom chain of resources with generic names.

*   **ClusterWatchRule / WatchRule (The Policy)**
    *   Ref: `DestinationRef` (generic name, unclear target).
*   **GitDestination (The Branch Context)**
    *   Ref: `RepoRef` (points to Config).
*   **GitRepoConfig (The Connection)**
    *   Ref: `SecretRef` (flat LocalObjectReference).

### Identified Pain Points

*   **Ambiguous Referencing:** The shared `NamespacedName` struct is used for both local and cluster-wide rules, making it impossible to enforce strict validation (e.g., namespace is required for one but forbidden for the other).
*   **Implicit Types:** References rely on the field name (e.g., `RepoRef`) rather than an explicit kind, preventing future extensibility (e.g., supporting GitLab or Bitbucket specific kinds).
*   **Naming Confusion:** `GitDestination` implies a location, but it actually defines writing logic (branching).
*   **Isolation Risk:** Cross-namespace references are not explicitly gated, allowing potential privilege escalation paths.

## 3. Advised Future State

### New Object Hierarchy

We will rename resources to better reflect their function in the Kubernetes ecosystem.

| Current Name | New Name | Role |
| :--- | :--- | :--- |
| `GitRepoConfig` | **GitProvider** | Infrastructure. Defines the connection (URL), Auth, and Security constraints. |
| `GitDestination` | **GitTarget** | Logic. Defines *where* to write (Target Branch, Folder) and links to a Provider. |
| `WatchRule` | **WatchRule** | Policy. Defines *what* to watch and links to a Target. |

### Core Architectural Shift: Polymorphic Providers

The most significant upgrade is allowing the `GitTarget` to reference multiple types of providers. This enables the "Bring Your Own Flux" feature.

The `GitTarget` will ask: "Where do I get the git credentials?"

*   **Option A (Standard):** Points to a local `GitProvider`.
*   **Option B (Ecosystem):** Points to a Flux `GitRepository`.

## 4. Detailed API Specifications

### A. The Connection Layer (Provider)

**Primary Object:** `GitProvider`
Designed for users who do not use Flux. Simple and effective.

**Updates:** Includes `AllowedBranches` and `PushStrategy` from the original `GitRepoConfig` to ensure security and performance controls are retained.

```go
// GitProvider defines a connection to a git host.
type GitProviderSpec struct {
    // URL of the repository (HTTP/SSH)
    URL string `json:"url"`
    
    // SecretRef for authentication credentials
    SecretRef corev1.LocalObjectReference `json:"secretRef"`

    // AllowedBranches restricts which branches can be written to.
    // +optional
    AllowedBranches []string `json:"allowedBranches,omitempty"`

    // Push defines the strategy for pushing commits (batching).
    // +optional
    Push *PushStrategy `json:"push,omitempty"`
}

type PushStrategy struct {
    // Interval is the maximum time to wait before pushing queued commits.
    // +optional
    Interval *string `json:"interval,omitempty"`

    // MaxCommits is the maximum number of commits to queue before pushing.
    // +optional
    MaxCommits *int `json:"maxCommits,omitempty"`
}
```

### B. The Logic Layer (The Target)

**Primary Object:** `GitTarget`
This object manages the logic of "Writing." It is the bridge between your rules and the git provider.

**Crucial Change:** The `providerRef` is now polymorphic.

```go
type GitTargetSpec struct {
    // ProviderRef points to the source of credentials/URL.
    // It supports:
    // 1. Kind: GitProvider, Group: <your-group> (Native)
    // 2. Kind: GitRepository, Group: source.toolkit.fluxcd.io (Flux)
    ProviderRef GitProviderReference `json:"providerRef"`

    // The target branch for the audit log.
    // +kubebuilder:default="main"
    Branch string `json:"branch"`

    // The target folder.
    BaseFolder string `json:"baseFolder"`
}

type GitProviderReference struct {
    // Group is the API Group of the referent.
    // Defaults to "configbutler.ai" if not specified.
    // +optional
    Group string `json:"group,omitempty"`

    // +kubebuilder:validation:Enum=GitProvider;GitRepository
    // +kubebuilder:default="GitProvider"
    Kind string `json:"kind"`

    Name string `json:"name"`
}
```

### C. The Policy Layer (The Rules)

We split the reference types to enforce strict tenancy safety.

#### 1. WatchRule (Local Scope)

Must only point to a `GitTarget` in the same namespace.

```go
type WatchRuleSpec struct {
    // SinkRef must be local. No namespace field allowed.
    SinkRef LocalTargetReference `json:"sinkRef"`
    Rules   []Rule               `json:"rules"`
}

type LocalTargetReference struct {
    // API Group of the referent.
    // +kubebuilder:default="configbutler.ai"
    Group string `json:"group,omitempty"`

    // +kubebuilder:default=GitTarget
    Kind string `json:"kind"`
    Name string `json:"name"`
}
```

#### 2. ClusterWatchRule (Global Scope)

Must explicitly point to a `GitTarget` in a specific namespace.

```go
type ClusterWatchRuleSpec struct {
    // SinkRef must include namespace.
    SinkRef NamespacedTargetReference `json:"sinkRef"`
    Rules   []Rule                    `json:"rules"`
}

type NamespacedTargetReference struct {
    // API Group of the referent.
    // +kubebuilder:default="configbutler.ai"
    Group string `json:"group,omitempty"`

    // +kubebuilder:default=GitTarget
    Kind      string `json:"kind"`
    Name      string `json:"name"`
    
    // Required because ClusterWatchRule has no namespace.
    // +required
    Namespace string `json:"namespace"`
}
```

## 5. Status & Conditions (Robust Implementation)

We implement a robust Status struct compatible with `kstatus` and Flux, following Kubernetes best practices (state-based conditions, positive polarity).

### Constants & Types

```go
import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
    // TypeReady is the summary condition - check this first for overall health
    // True: GitTarget is properly configured and operational
    TypeReady = "Ready"

    // TypeAvailable indicates repository accessibility
    // True: Git repository is accessible and operations can proceed
    TypeAvailable = "Available"

    // TypeActive indicates worker operational state
    // True: BranchWorker is running and can process events
    TypeActive = "Active"

    // TypeSynced indicates synchronization state with Git
    // True: All events have been successfully pushed to Git
    TypeSynced = "Synced"
)

// Condition Reasons
const (
    // Ready reasons
    ReasonReady                  = "Ready"
    ReasonValidating             = "Validating"
    ReasonBranchNotAllowed       = "BranchNotAllowed"
    ReasonInvalidConfiguration   = "InvalidConfiguration"

    // Available reasons
    ReasonAvailable              = "Available"
    ReasonAuthenticationFailed   = "AuthenticationFailed"
    ReasonRepositoryNotFound     = "RepositoryNotFound"
    ReasonNetworkError           = "NetworkError"
    ReasonGitOperationFailed     = "GitOperationFailed"
    ReasonChecking               = "Checking"

    // Active reasons
    ReasonActive                 = "Active"
    ReasonIdle                   = "Idle"
    ReasonWorkerNotStarted       = "WorkerNotStarted"
    ReasonWorkerStopped          = "WorkerStopped"

    // Synced reasons
    ReasonSynced                 = "Synced"
    ReasonSyncInProgress         = "SyncInProgress"
    ReasonSyncFailed             = "SyncFailed"
    ReasonEventsQueued           = "EventsQueued"
)
```

### GitTarget Status Struct

```go
type GitTargetStatus struct {
    // Conditions represent the latest available observations of an object's state
    // Types: Ready (summary), Available, Active, Synced
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

    // ObservedGeneration is the last generation that was reconciled
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

### Helper Methods (Duck Typing & Logic)

```go
// GetConditions returns the conditions of the GitTarget.
func (s *GitTarget) GetConditions() []metav1.Condition {
    return s.Status.Conditions
}

// SetConditions sets the conditions of the GitTarget.
func (s *GitTarget) SetConditions(conditions []metav1.Condition) {
    s.Status.Conditions = conditions
}

// updateReadyCondition sets the Ready condition based on other conditions.
// Ready is True only when:
// 1. Configuration is valid (implied by reaching this point without early return)
// 2. Available is True
// 3. Active is True (or Unknown if worker starting)
func (r *GitTargetReconciler) updateReadyCondition(target *GitTarget) {
    available := meta.FindStatusCondition(target.Status.Conditions, TypeAvailable)
    active := meta.FindStatusCondition(target.Status.Conditions, TypeActive)

    if available != nil && available.Status == metav1.ConditionTrue &&
       active != nil && (active.Status == metav1.ConditionTrue || active.Status == metav1.ConditionUnknown) {
        meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
            Type:    TypeReady,
            Status:  metav1.ConditionTrue,
            Reason:  ReasonReady,
            Message: "GitTarget is operational",
        })
    } else {
        // Logic to determine specific failure reason would go here
        // For now, default to generic not ready if not explicitly set otherwise
        if meta.FindStatusCondition(target.Status.Conditions, TypeReady) == nil {
             meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
                Type:    TypeReady,
                Status:  metav1.ConditionFalse,
                Reason:  ReasonValidating,
                Message: "Waiting for checks to complete",
            })
        }
    }
}
```

## 6. Flux Compatibility Strategy

**Objective:** Allow users with existing Flux pipelines to use your operator without duplicating configuration.

### How it works

1.  **Detection:** Your controller checks if the user referenced `kind: GitRepository` (group: `source.toolkit.fluxcd.io`).
2.  **Dynamic Fetch:**
    *   **If Yes:** The controller fetches the Flux object. It reads `.spec.url` and `.spec.secretRef` from the Flux CRD.
    *   **If No:** It fetches your native `GitProvider`.
3.  **Write Action:** Your operator performs the actual git push. (Note: We reuse Flux's config, not its logic).

### Critical Note on Secrets
Flux `GitRepository` objects often reference **read-only** deploy keys. For this operator to *write* to the repo, the Secret referenced by the Flux `GitRepository` must have **write access**. This must be clearly documented.

## 7. Status & Connection Reporting Strategy

**Question:** How should we report connection state, especially when referencing a Flux resource?

**Strategy:** The `GitTarget` is the authority on "Write Access".

1.  **Do Not Copy Status:** We should not blindly copy the status from `GitProvider` or Flux `GitRepository`.
    *   Flux's status only confirms it can *read/sync* from the repo.
    *   Our requirement is *write* access.
2.  **Independent Verification:** The `GitTarget` controller must perform its own lightweight check (e.g., `git ls-remote` with credentials, or a dry-run push) to verify it has write permissions.
3.  **Reporting:**
    *   **Connection Check:**
        *   If the check succeeds: Set `Available=True` with `Reason=Available`. Populate `GitStatus` with branch info.
        *   If the check fails (e.g., read-only key): Set `Available=False` with `Reason=AuthenticationFailed` (or `NetworkError`, `RepositoryNotFound`) and a clear message.
    *   **Worker Status:**
        *   If the worker is running: Set `Active=True` with `Reason=Active`. Populate `WorkerStatus`.
    *   **Overall Health:**
        *   The `Ready` condition aggregates these states. It is `True` only if `Available=True` AND `Active=True`.

This approach ensures that `GitTarget` status is always a truthful reflection of the operator's ability to function, regardless of whether the underlying config comes from Flux or our own Provider.

## 8. Migration Steps

1.  **Refactor Types:**
    *   Create the `GitProvider` and `GitTarget` structs.
    *   Remove the shared `NamespacedName` struct in favor of `LocalTargetReference` and `NamespacedTargetReference`.
2.  **Add Markers:**
    *   Apply `+kubebuilder:validation:Enum` to all Kind fields.
    *   Apply `+kubebuilder:default` to simplify YAML for users.
3.  **Implement Polymorphism:**
    *   Update your Reconcile loop to switch logic based on `providerRef.Kind`.
    *   **Note:** Ensure you add RBAC permissions to your operator to get/list/watch Flux `GitRepositories`.
4.  **Status & Conditions:**
    *   Update `GitTarget` to populate `GitStatus` and `WorkerStatus` structs and use the new Condition helpers.
