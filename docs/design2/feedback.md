# Feedback on Design Strategy: GitOps Reverser Operator

## Analysis of Current State

The "Current State Analysis" in `new-config.md` is accurate and aligns with the codebase in `api/v1alpha1/`.

*   **Ambiguous Referencing:** Confirmed. `NamespacedName` is used for both `ClusterWatchRule` (where namespace is logically required) and `WatchRule` (where it defaults to local). This prevents strict validation at the API level.
*   **Implicit Types:** Confirmed. `RepoRef` in `GitDestination` is just a `NamespacedName`, relying on field naming conventions rather than explicit types.
*   **Naming Confusion:** Confirmed. `GitDestination` mixes "where to write" (branch/folder) with "connection" (repo ref), while `GitRepoConfig` handles the actual connection. The proposed split into `AuditSink` (logic) and `GitProvider` (connection) is much clearer.

## Evaluation of Advised Future State

The proposed design represents a significant maturity step for the operator.

### Strengths

1.  **Polymorphism & Flux Integration:**
    *   The ability for `AuditSink` to reference either a native `GitProvider` or a Flux `GitRepository` is a major adoption enabler. It allows users to reuse existing GitOps configurations without duplication.
    *   Using `ProviderRef` with `Kind` and `APIGroup` is the correct Kubernetes-native approach.

2.  **Strict Reference Types:**
    *   Splitting `NamespacedName` into `LocalSinkReference` and `NamespacedSinkReference` is excellent.
    *   It enforces security boundaries at the schema level: `WatchRule` can *only* reference local sinks, preventing cross-namespace privilege escalation by design.

3.  **Clearer Terminology:**
    *   `AuditSink` and `GitProvider` are standard, intuitive terms that better describe the resource roles.

### Recommendations & Improvements

1.  **Retain Security & Performance Controls:**
    *   The current `GitRepoConfig` includes `AllowedBranches` (for security) and `PushStrategy` (for performance/batching).
    *   The proposed `GitProvider` spec in the design document omits these.
    *   **Recommendation:** Ensure `GitProvider` retains `AllowedBranches` and `PushStrategy`. For Flux `GitRepository` references, we might need to define how these settings are handled (perhaps on the `AuditSink` itself if they are logic-related, or accepted as defaults).

2.  **Flux Secret Handling:**
    *   Flux `GitRepository` objects reference a Secret. The operator will need RBAC permissions to read these Secrets.
    *   **Note:** Flux often uses read-only deploy keys. For this operator to *write* to the repo, the Secret referenced by the Flux `GitRepository` must have **write access**. This needs to be clearly documented, as users might try to reuse a read-only Flux secret and fail.

3.  **Status Subresources:**
    *   The design mentions `AuditSink` status.
    *   **Recommendation:** Ensure `GitProvider` also has a standard Status subresource (`Conditions` like `Ready`, `ConnectionVerified`) to help users debug connection issues independent of the sink.

4.  **Migration Strategy:**
    *   The migration plan is sound.
    *   **Recommendation:** Implement a conversion webhook or a CLI tool to help users migrate their existing `GitDestination`/`GitRepoConfig` resources to `AuditSink`/`GitProvider`.

## Conclusion

The proposed design is strongly endorsed. It solves the structural issues of the current API and positions the operator for wider ecosystem integration.
