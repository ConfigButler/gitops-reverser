# Naming Brainstorming: Replacing "AuditSink"

The term "Audit" implies a specific use case (compliance/logging), but the operator is a general-purpose tool for exporting Kubernetes resources to Git (Reverse GitOps, Backup, Config Replication).

We need a name that reflects:
1.  **Function:** It is a destination for resources.
2.  **Scope:** It defines a specific slice (Branch + BaseFolder) of a Repository.
3.  **Relationship:** It links a Policy (`WatchRule`) to Infrastructure (`GitProvider`).

## Top Contenders

### 1. `GitTarget`
*   **Concept:** A specific target for the data flow.
*   **Pros:**
    *   Neutral and generic.
    *   Pairs well with "Source" (the Cluster) and "Target" (Git).
    *   "Target" is standard terminology in data integration.
*   **Cons:** Slightly generic, but in a `configbutler.ai` group, it's clear.
*   **Example:**
    ```yaml
    kind: GitTarget
    spec:
      providerRef: ...
      branch: main
      baseFolder: production/namespace-a
    ```

### 2. `GitSink`
*   **Concept:** A "Sink" is a destination for an event or data stream (standard K8s terminology, e.g., `AuditSink` in K8s audit logs).
*   **Pros:**
    *   Keeps the "Sink" semantics from the design (which is accurate for a data pipeline).
    *   Removes the "Audit" restriction.
    *   Clearly distinguishes from `GitProvider` (the connection) vs `GitSink` (the destination).
*   **Cons:** "Sink" can sometimes imply a "bit bucket" or one-way drop-off.

### 3. `ResourceExport` / `ExportTarget`
*   **Concept:** Emphasizes the *action* of exporting resources.
*   **Pros:**
    *   Very descriptive of the operator's behavior (Exporting state).
*   **Cons:** "Export" might sound like a one-time job rather than a continuous sync.

### 4. `ManifestLocation`
*   **Concept:** Defines *where* the manifests live.
*   **Pros:**
    *   Literal.
*   **Cons:** A bit passive. Doesn't imply the "writing" aspect as strongly.

## Comparison Table

| Name | Implication | Pros | Cons |
| :--- | :--- | :--- | :--- |
| **`AuditSink`** | Compliance, Logging | Standard K8s term | Too specific (ignores backup/sync use cases) |
| **`GitTarget`** | Destination, Goal | Clear, Neutral | Generic |
| **`GitSink`** | Data Stream Destination | Standard System term | "Sink" can be jargon |
| **`GitSpace`** | A partitioned area | Friendly | Vague |
| **`RepoSlice`** | A part of a repo | Accurate | Non-standard |

## Deep Dive: Cardinality & The "BranchSink" Alternative

You mentioned the internal `branchWorker` and the idea of a `BranchSink` that supports multiple folders. This brings up a key architectural decision: **Where is the folder defined?**

### Option A: The "Target" Model (Recommended)
**1 Object = 1 Folder** (Current Design)

*   **Structure:**
    *   `GitTarget` defines `Branch` + `BaseFolder`.
    *   `WatchRule` points to `GitTarget`.
*   **Pros:**
    *   **Encapsulation:** The `WatchRule` (Policy) doesn't need to know about file paths. It just knows "Send to Target X".
    *   **Abstraction:** You can move the folder in the `GitTarget` definition without updating every `WatchRule`.
    *   **Security:** You can RBAC who can create `GitTargets` (Admins defining paths) vs who can create `WatchRules` (Users defining what to watch).
*   **Cons:**
    *   More CRD instances if you have many folders.

### Option B: The "Branch" Model
**1 Object = 1 Branch**

*   **Structure:**
    *   `BranchSink` defines `Branch`.
    *   `WatchRule` points to `BranchSink` AND specifies `Folder`.
*   **Pros:**
    *   Fewer CRD instances (one per branch).
*   **Cons:**
    *   **Leaky Abstraction:** `WatchRule` now contains config data (file paths).
    *   **Refactoring Pain:** If you want to reorganize your git folder structure, you have to edit every `WatchRule`.
    *   **Validation:** Harder to enforce "Users can only write to /foo" if the folder is a string in the Rule.

### Conclusion
I recommend sticking with **Option A (`GitTarget` / `GitSink`)**.
Even though it creates more objects, it correctly separates **Infrastructure Configuration** (Where things go) from **Policy** (What things are watched). The Controller can still optimize this internally by grouping all Targets that share a branch into a single `branchWorker`.
