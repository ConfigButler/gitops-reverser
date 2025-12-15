# Implementation Plan: GitProvider & GitTarget

## Strategy: New Types vs. Rename

Given the significant structural changes (polymorphism, strict references), we recommend **creating new types** rather than renaming existing ones.

### Why not just Rename?
While VS Code's "Rename Symbol" is efficient for Go code, it introduces risks for Kubernetes Operators:
1.  **Data Loss:** Renaming a CRD (`GitRepoConfig` -> `GitProvider`) is seen by Kubernetes as deleting the old resource and creating a new one. All existing data in the cluster would be lost unless manually backed up and migrated.
2.  **Breaking Changes:** The schema changes (e.g., `SecretRef` structure, `ProviderRef` polymorphism) are not backward compatible.
3.  **Safety:** keeping the old types allows for a "blue/green" migration where you can verify the new logic works before deleting the old resources.

## Step-by-Step Implementation

### Phase 1: API Definition (Code Mode)
1.  **Create Files:**
    *   `api/v1alpha1/gitprovider_types.go`
    *   `api/v1alpha1/gittarget_types.go`
2.  **Define Structs:**
    *   Implement `GitProvider` (based on `GitRepoConfig` but cleaned up).
    *   Implement `GitTarget` (based on `GitDestination` but with `GitProviderReference`).
    *   Implement `GitProviderReference` with the new `Group` (string) and `Kind` fields.
3.  **Generate:**
    *   Run `make generate` (DeepCopy methods).
    *   Run `make manifests` (CRD YAMLs).

### Phase 2: Controller Implementation
1.  **Scaffold Controllers:**
    *   `internal/controller/gitprovider_controller.go`
    *   `internal/controller/gittarget_controller.go`
2.  **Port Logic:**
    *   Copy connection logic from `GitRepoConfig` to `GitProvider`.
    *   Copy write logic from `GitDestination` to `GitTarget`.
3.  **Implement Polymorphism:**
    *   In `GitTarget` controller, add logic to resolve `ProviderRef`:
        *   If `Kind == "GitProvider"`, look up local `GitProvider`.
        *   If `Kind == "GitRepository"`, look up Flux resource (dynamic client).

### Phase 3: Cleanup
1.  **Deprecate:** Mark `GitRepoConfig` and `GitDestination` as deprecated in GoDoc.
2.  **Remove:** In a future release, remove the old types and controllers.

## VS Code Efficiency Tips
*   **Split Editor:** Open `gitrepoconfig_types.go` on the left and `gitprovider_types.go` on the right. Copy-paste fields and bulk-rename using `Ctrl+D` (Multi-cursor).
*   **Go Interface Extraction:** If logic is shared, use VS Code to "Extract Method" to a shared `internal/git/` package so both old and new controllers can use it.
