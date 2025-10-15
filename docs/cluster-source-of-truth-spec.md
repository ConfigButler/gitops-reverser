# GitOps Reverser: Cluster-as-Source-of-Truth — consolidated specification (MVP)

> Update: Retain validating webhook for username capture (defer removal)
> 
> We will keep a minimal validating webhook operational to capture the admission username for commit metadata. Concretely:
> - Keep the validating webhook registered at path /process-validating-webhook
> - Keep Helm ValidatingWebhookConfiguration and Kustomize webhook manifests in place
> - Keep FailurePolicy=Ignore and leader-only service routing
> - Continue watch-based ingestion work behind --enable-watch-ingestion
> - Webhook decommission will happen only after watch-only parity and a separate migration step
This specification defines a watch-based, cluster-as-source-of-truth design for gitops-reverser. It replaces webhook ingestion, simplifies CRDs for a focused MVP, and keeps current Git behavior where possible. Rationale and future enhancements are captured in a final Considerations and Future improvements section.

References
- APIs: [api/v1alpha1/watchrule_types.go](api/v1alpha1/watchrule_types.go), [api/v1alpha1/clusterwatchrule_types.go](api/v1alpha1/clusterwatchrule_types.go), [api/v1alpha1/gitrepoconfig_types.go](api/v1alpha1/gitrepoconfig_types.go)
- Mapping: [internal/types/identifier.go](internal/types/identifier.go)
- Git: [internal/git/git.go](internal/git/git.go), [internal/git/worker.go](internal/git/worker.go)
- Queue: [internal/eventqueue/queue.go](internal/eventqueue/queue.go)
- Sanitization: [internal/sanitize/marshal.go](internal/sanitize/marshal.go)
- Controllers: [internal/controller/watchrule_controller.go](internal/controller/watchrule_controller.go), [internal/controller/clusterwatchrule_controller.go](internal/controller/clusterwatchrule_controller.go)
- Charts: [charts/gitops-reverser/templates/](charts/gitops-reverser/templates/)

1. Objectives
- Treat the live cluster as authoritative for selected scopes
- Ingest via Kubernetes List + Watch (no webhooks)
- Write deterministic, canonical YAML into Git under a destination baseFolder on specific branches
- Support multiple branches per repo; push directly (no PRs)
- Provide clear defaults focused on desired-state resources
- Maintain current Git behavior where possible for a low-risk MVP

2. API surfaces (CRDs)
Group/Version: configbutler.ai/v1alpha1

2.1 GitRepoConfig (Namespaced)
Purpose: Repository connectivity/auth and allowlisting for branches.
Spec (key)
- repoUrl: string (required)
- allowedBranches: []string (required)
- secretRef: LocalObjectReference (optional)
- push: PushStrategy (optional) with interval and maxCommits
Status
- conditions: []metav1.Condition
- observedGeneration: int64
Constraints and effects
- Destinations targeting this repo must choose branch ∈ allowedBranches

2.2 GitDestination (Namespaced)
Purpose: Writable target binding repo, branch, and an owned baseFolder.
Spec (key)
- repoRef: NamespacedName to GitRepoConfig (required)
- branch: string (required; must be allow‑listed by the referenced GitRepoConfig)
- baseFolder: string (required; root path owned in that branch)
- exclusiveMode: bool (default false)
Status
- conditions: []metav1.Condition (e.g., Ready, OwnershipConflict)
Behavior
- Ownership marker path (when exclusiveMode=true): {baseFolder}/.configbutler/owner.yaml
- Multiple destinations may share repo+branch; their baseFolders must not overlap

2.3 WatchRule (Namespaced)
Purpose: Select namespaced resources in its own namespace and write to a destination.
Spec (key)
- destinationRef: NamespacedName to GitDestination (required; default namespace = WatchRule namespace if omitted)
- rules: []ResourceRule (required; min 1)
  - operations: []OperationType (optional; CREATE|UPDATE|DELETE|*; empty = all)
  - apiGroups: []string (optional; ""=core, "*"=all; empty=all)
  - apiVersions: []string (optional; "*"=all; empty=all)
  - resources: []string (required; plural names; subresources kind/subresource; "*" for all; no prefix wildcards)
Status
- conditions: []metav1.Condition
Defaults and expectations
- If resources omitted in configuration (future defaulting), default to Desired‑state preset (namespaced kinds only)
- Restricted to its own namespace by design

2.4 ClusterWatchRule (Cluster)
Purpose: Select cluster‑scoped resources and/or namespaced resources across all namespaces.
Spec (key)
- destinationRef: NamespacedName to GitDestination (required; explicit namespace)
- rules: []ClusterResourceRule (required; min 1)
  - operations, apiGroups, apiVersions, resources: same semantics as WatchRule
  - scope: Cluster | Namespaced (required)
Status
- conditions: []metav1.Condition
Defaults and expectations
- If resources omitted in configuration (future defaulting), default to Desired‑state preset across chosen scope

Note on simplifications for MVP
- No accessPolicy on any CRD (GitRepoConfig, GitDestination, WatchRule, ClusterWatchRule)
- No objectSelector on WatchRule
- No namespaceSelector on ClusterWatchRule rules

3. Ingestion and discovery (watch‑only)
- Discovery enumerates server resources supporting list/watch
- Build shared informers per selected GVK (rule‑driven or watch‑all with built‑in exclusions)
- Watch config: resourceVersionMatch=NotOlderThan, allowWatchBookmarks=true; backoff 500ms..30s; on Expired, re‑list

3.1 Desired‑state preset v1.0 (MVP defaults)
Default include (examples)
- deployments.apps, statefulsets.apps, daemonsets.apps
- services, ingresses.networking.k8s.io, networkpolicies.networking.k8s.io
- poddisruptionbudgets.policy
- RBAC: roles.rbac.authorization.k8s.io, rolebindings.rbac.authorization.k8s.io, clusterroles.rbac.authorization.k8s.io, clusterrolebindings.rbac.authorization.k8s.io
- configmaps, secrets, serviceaccounts
- resourcequotas, limitranges, priorityclasses.scheduling.k8s.io
- customresourcedefinitions.apiextensions.k8s.io, apiservices.apiregistration.k8s.io
- storageclasses.storage.k8s.io (PVCs optional; not default)
Default exclude (built‑in, not configurable in MVP)
- pods; events (core, events.events.k8s.io); endpoints; endpointslices.discovery.k8s.io; leases.coordination.k8s.io
- controllerrevisions.apps; flowschemas.flowcontrol.apiserver.k8s.io; prioritylevelconfigurations.flowcontrol.apiserver.k8s.io
- jobs.batch; cronjobs.batch (excluded by default; noisy and lifecycle‑heavy)

3.2 Discovery defaults
- discoveryRefresh: 5m
- watchAll: false (opt‑in in future)
- informerCaps: maxGVKs=300, maxConcurrentInformers=50; client QPS=5, burst=10
- defaultExclusionList matches Default exclude above

3.3 CRD lifecycle and visibility (elaboration)
- Discovery is authoritative: only resources present in discovery with list/watch are processed
- If a CRD is removed
  - API discovery drops the GVK; S_live for that GVK is empty on next reconciliation
  - Files for that GVK become orphans and are deleted (subject to delete caps); K8s also deletes CRs when their CRD is removed
- If a CRD adds a new served version
  - On discovery refresh, the version is observed; list+watch start; canonical paths incorporate the version (group/version/…)
- If a CRD stops serving a version
  - Objects transition to a different served version (depending on conversion and applies); paths reflect the active version; old files orphan and delete
- If a rule references an unknown/non‑discoverable GVK
  - The controller logs a warning and does not attempt to list/watch until discovery exposes it; rules remain inert for that GVK

4. Reconciliation algorithm
Workers and checkouts
- Worker key: repo‑branch = hash(repoURL,branch)
- One dedicated clone per worker at /var/cache/gitops-reverser/{hash(repoURL,branch)}
- A worker handles all baseFolder trees for that repo‑branch; file ops are serialized

Startup seed (per selected GVK)
- List() to get items and resourceVersion
- Canonicalize and map each object to baseFolder + identifierPath from [internal/types/identifier.go](internal/types/identifier.go)
- Enqueue upsert events to the repo‑branch worker (semantic no‑op if unchanged)

Orphan detection (per baseFolder)
- S_live = set of paths from current List() results
- S_git = set of tracked file paths under baseFolder in the working tree (excluding ownership marker)
- S_orphan = S_git − S_live; enqueue deletes (respect deleteCapPerCycle)

Trailing changes
- Start Watch() from captured resourceVersion; enqueue upsert/delete events from deltas
- On Expired, re‑list and repeat S_orphan computation

Batching and limits
- Commit flush: maxFiles=200, maxBytes=10MiB, maxWait=20s
- deleteCapPerCycle: 500
- Concurrency caps per repo and globally (per prior defaults)

5. Git operations (go‑git policy; keep current behavior where possible)
- Maintain existing Git handling patterns where possible for a low‑risk MVP
- Push after staging commits
- On push reject (non fast‑forward)
  - fetch remote tip for branch
  - reset --hard to remote tip
  - recompute/reapply pending changes; stage; re‑commit
  - push again
- No merge tools are attempted; this emulates rebase by replaying local changes

6. Ownership and conflict handling
- Kubernetes Lease per repo‑branch worker (acquire before writes; renew periodically)
- Ownership marker per destination at {baseFolder}/.configbutler/owner.yaml
  - exclusiveMode=false: warn on mismatch and continue
  - exclusiveMode=true: refuse writes; set Ready=False on referencing rules
- Commit trailers on every commit: X‑ConfigButler‑ClusterUID, X‑ConfigButler‑ControllerNS, X‑ConfigButler‑ControllerName, X‑ConfigButler‑InstanceID

7. Observability
- Metrics: objects_scanned_total, objects_written_total, files_deleted_total, commits_total, commit_bytes_total, rebase_retries_total, ownership_conflicts_total, lease_acquire_failures_total, marker_conflicts_total, repo_branch_active_workers, repo_branch_queue_depth
- Logs: include object identifiers, destination, commit SHAs
- Events: major lifecycle actions and conflicts

8. Security and RBAC
- Read selected resources: get, list, watch
- coordination.k8s.io Leases: get, list, watch, create, update, patch, delete
- events: create, patch
- secrets: get (Git credentials)
- namespaces: get (if needed for future features; no selectors used in MVP)
- configbutler.ai CRDs: get, list, watch for watchrules, clusterwatchrules, gitrepoconfigs, gitdestinations; status get/update/patch where applicable
- Helm RBAC updates in [charts/gitops-reverser/templates/rbac.yaml](charts/gitops-reverser/templates/rbac.yaml)

9. Packaging (Helm) and removal of webhooks
- Remove webhook ingestion code: [internal/webhook/event_handler.go](internal/webhook/event_handler.go) and tests
- Remove kustomize webhook manifests: [config/webhook/](config/webhook/kustomization.yaml)
- Remove/disable chart templates: [charts/gitops-reverser/templates/validating-webhook.yaml](charts/gitops-reverser/templates/validating-webhook.yaml); adjust [charts/gitops-reverser/templates/certificates.yaml](charts/gitops-reverser/templates/certificates.yaml) if webhook‑only
- Update README and chart docs to describe watch‑based ingestion and defaults

10. Testing and acceptance
- Unit: identifier mapping stability; sanitization golden tests; discovery filtering
- Integration: snapshot writes and orphan deletions; idempotent re‑run; multi‑branch concurrency; lease behavior
- E2E: watch‑only flows with Kind; Desired‑state defaults (Jobs/CronJobs excluded)
- Acceptance: no webhooks installed; direct pushes; immediate deletes within caps; metrics emitted

11. Considerations and Future improvements (out of MVP scope)
- Advanced loop‑avoidance (Flux/Argo alignment and field ignore tuning)
- Reintroducing accessPolicy on GitRepoConfig/Destination and rule‑level authorization
- Rule‑level selectors: objectSelector on WatchRule; namespaceSelector on ClusterWatchRule
- Configurable exclusion lists (watchAll mode with overrides)
- Per‑GVK tuning for batching and concurrency
- PR‑first mode or conflict‑policy customization
- Defaulted‑field omission driven by schema for tighter semantic diffs

12. Implementation status and next steps (MVP)

This section documents the current implementation progress toward the watch-based, cluster-as-source-of-truth MVP, and outlines the next steps to complete the plan in this spec.

12.1 What is implemented

- Feature-gated watch ingestion
  - Controller flag to enable watch-based ingestion alongside the existing webhook path (default: disabled).
  - Files: [cmd/main.go](cmd/main.go), [internal/watch/manager.go](internal/watch/manager.go)

- Rule-driven GVR aggregation
  - Aggregates concrete GVRs (group, version, resource) from active WatchRule/ClusterWatchRule via the in-memory RuleStore.
  - Deduplicates, is scope-aware (Namespaced vs Cluster), and intentionally ignores wildcards and subresources for MVP.
  - File: [internal/watch/gvr.go](internal/watch/gvr.go)

- Discovery filtering
  - Filters requested GVRs using API discovery (list+watch only), and retains only resources matching the requested scope.
  - Resilient to partial discovery failures (common in clusters).
  - File: [internal/watch/discovery.go](internal/watch/discovery.go)

- Dynamic informers (best-effort)
  - Starts SharedInformers for discoverable GVRs; handles add/update/delete and enqueues sanitized objects into the existing git worker path.
  - Emits basic watcher metrics via the existing metrics exporter.
  - File: [internal/watch/informers.go](internal/watch/informers.go)

- Minimal polling path for early validation
  - Retained a periodic polling loop for ConfigMaps (core/v1) to validate end-to-end commit flow during transition to full informers.
  - File: [internal/watch/manager.go](internal/watch/manager.go)

- Identifier helper for watch events
  - Helper to build canonical REST-style storage paths for watch-based ingestion:
  - File: [internal/types/identifier.go](internal/types/identifier.go)

- Reuse of the existing pipeline
  - Existing queue and git worker remain authoritative for batching and committing:
  - Files: [internal/eventqueue/queue.go](internal/eventqueue/queue.go), [internal/git/worker.go](internal/git/worker.go)

- Validation status (mandatory pre-completion)
  - make lint → pass (0 issues)
  - make test → pass (all packages)
  - make test-e2e → pass (18/18; webhook path preserved; watch flag kept disabled by default)

12.2 How to run it (for experiments)

- Deploy as usual, with the existing webhook path still in place.
- To enable watch ingestion:
  - Add the controller argument: --enable-watch-ingestion
  - The manager will:
    - Aggregate requested GVRs from rules
    - Filter using discovery (list+watch, scope-aligned)
    - Start dynamic informers for discoverable GVRs
    - Retain the ConfigMap polling path for MVP validation

12.3 Next steps to complete the MVP (tracked against spec sections)

- Reconciliation path from spec §4
  - Startup seed (per GVK):
    - Perform List() to compute S_live for all selected resources
    - Map objects to canonical paths using [internal/types/identifier.go](internal/types/identifier.go)
    - Enqueue upserts (no-op if unchanged on disk)
  - Orphan detection (per baseFolder):
    - Compute S_git (tracked files under destination baseFolder, excluding ownership marker)
    - Compute S_orphan = S_git − S_live and enqueue deletes (respect delete caps)
  - Trailing changes:
    - Switch to informer Watch() deltas with resourceVersion continuity
    - On Expired, re-list and recompute S_orphan

- Desired-state defaults (spec §3.1, §3.2)
  - Introduce Desired-state include/exclude presets and discovery defaults
  - Apply built-in excludes for lifecycle-heavy/noisy resources (e.g., jobs.batch, cronjobs.batch)

- Batching and limits (spec §4, §5)
  - Implement commit flush caps: maxFiles, maxBytes, maxWait
  - Add deleteCapPerCycle for safe orphan clearing
  - Ensure rebase-like retry behavior on non-fast-forward remains unchanged

- Ownership and leases (spec §6)
  - Add per repo-branch leader Lease acquisition/renewal before writes
  - Implement per-destination marker at {baseFolder}/.configbutler/owner.yaml
  - Enforce exclusiveMode (warn vs refuse writes) semantics

- Observability (spec §7)
  - Extend metrics with objects_scanned_total, objects_written_total, files_deleted_total, commits_total, commit_bytes_total, rebase_retries_total, ownership_conflicts_total, lease_acquire_failures_total, marker_conflicts_total, repo_branch_active_workers, repo_branch_queue_depth
  - Continue reusing current OTEL exporter; add labels where appropriate

- Security and RBAC (spec §8)
  - Update ClusterRole(s) to include discovery, list, watch for all selected resources and Leases verbs
  - Adjust Helm RBAC templates: [charts/gitops-reverser/templates/rbac.yaml](charts/gitops-reverser/templates/rbac.yaml)

- Packaging and removal of webhooks (spec §9)
  - After watcher path is feature-complete and validated, remove:
    - Code: [internal/webhook/event_handler.go](internal/webhook/event_handler.go)
    - Kustomize manifests: config/webhook/*
    - Helm templates: [charts/gitops-reverser/templates/validating-webhook.yaml](charts/gitops-reverser/templates/validating-webhook.yaml)
  - Update certificates template if it was webhook-only

- Documentation and examples
  - Update README and chart docs to describe watch-only mode, defaults, and resource scoping
  - Provide sample WatchRule/ClusterWatchRule that exercise common desired-state resources

12.4 Risk management and rollout plan

- Keep webhook path until watcher path matches or exceeds parity for targeted scopes
- Use feature flag to enable gradual rollout and A/B testing
- Gate removal of webhooks on:
  - Successful e2e runs in watch-only mode
  - Metrics parity and stability under load
  - RBAC/chart/doc updates finalized

End of status section.

End of specification.
## Addendum: webhook retention and decommission sequencing

This specification keeps the validating webhook temporarily to capture admission usernames for commit metadata while the watch-based path reaches parity. Until explicit decommission gates are met:

- Retain:
  - [charts/gitops-reverser/templates/validating-webhook.yaml](charts/gitops-reverser/templates/validating-webhook.yaml)
  - [config/webhook/kustomization.yaml](config/webhook/kustomization.yaml)
  - Controller handler bound at /process-validating-webhook (see [cmd/main.go](cmd/main.go))
- Configure with FailurePolicy=Ignore and leader-only service routing
- Proceed with watch-based ingestion behind --enable-watch-ingestion
- Decommission steps described in §9 (“Packaging (Helm) and removal of webhooks”) are deferred and executed only after:
  - Watch-only parity for targeted scopes
  - E2E success in watch-only mode
  - RBAC/chart/docs finalized for watch-only

## MVP deltas and alignment notes

- Worker key correction
  - One worker per (repoURL, branch), not per baseFolder. A single worker serializes file ops across multiple baseFolder trees in that repo-branch. See policy reference in [internal/git/worker.go](internal/git/worker.go) and recap in this spec §4 and §5.
- Access policy scope (documentation vs enforcement)
  - This spec retains the descriptive surfaces for accessPolicy on GitRepoConfig/GitDestination for future capability, but MVP enforcement can be deferred in controllers. References: [api/v1alpha1/gitrepoconfig_types.go](api/v1alpha1/gitrepoconfig_types.go), [api/v1alpha1/watchrule_types.go](api/v1alpha1/watchrule_types.go), [api/v1alpha1/clusterwatchrule_types.go](api/v1alpha1/clusterwatchrule_types.go).
- Desired-state presets
  - The include/exclude presets and discovery defaults will be codified in [internal/watch/discovery.go](internal/watch/discovery.go) and [internal/watch/gvr.go](internal/watch/gvr.go). Defaults from §3.1/§3.2 remain authoritative for MVP.

## Implementation iteration marker — 2025-10-15 (UTC)

This iteration focused on documentation alignment and explicit rollout sequencing:

- Clarified webhook retention and the gates for decommission; packaging removal steps remain documented but deferred (see §9).
- Clarified worker identity as per-repo-branch, with multiple baseFolder trees handled by a single worker’s serialized queue.
- Reiterated MVP stance on accessPolicy (documented surfaces retained; enforcement can be phased).
- Cross-referenced Desired-state presets and discovery defaults to their implementation homes.

No code changes were required for this iteration; current implementation status from §12.1 remains:

- Feature-gated watch ingestion present: [cmd/main.go](cmd/main.go), [internal/watch/manager.go](internal/watch/manager.go)
- Rule-driven GVR aggregation: [internal/watch/gvr.go](internal/watch/gvr.go)
- Discovery filtering: [internal/watch/discovery.go](internal/watch/discovery.go)
- Dynamic informers: [internal/watch/informers.go](internal/watch/informers.go)
- Identifier mapping: [internal/types/identifier.go](internal/types/identifier.go)
- Queue and git worker reused: [internal/eventqueue/queue.go](internal/eventqueue/queue.go), [internal/git/worker.go](internal/git/worker.go)

Validation status (unchanged):
- make lint → pass
- make test → pass
- make test-e2e → pass (webhook path preserved; watch flag disabled by default)

Next concrete actions (tracked also in the plan document):
- Seed and orphan deletion: List to compute S_live; compute S_orphan and enqueue deletes with caps ([internal/watch/manager.go](internal/watch/manager.go), [internal/git/worker.go](internal/git/worker.go))
- Watch continuity and re-list on Expired ([internal/watch/informers.go](internal/watch/informers.go))
- Batch flush caps and sizes ([internal/git/worker.go](internal/git/worker.go))
- Repo-branch Lease + per-destination marker with exclusiveMode semantics ([internal/leader/leader.go](internal/leader/leader.go), [internal/git/worker.go](internal/git/worker.go))
- Metrics extension per §7 ([internal/metrics/exporter.go](internal/metrics/exporter.go))
- RBAC updates for Leases and selected resources ([charts/gitops-reverser/templates/rbac.yaml](charts/gitops-reverser/templates/rbac.yaml))

For cross-reference, the corresponding plan addendum and engineer checklist were added in: [docs/cluster-source-of-truth-plan.md](docs/cluster-source-of-truth-plan.md)
## Appendices (MVP, normative unless noted)

### Appendix A: Canonicalization and YAML rendering
Goal: produce stable, semantically minimal YAML to prevent churn.
- Drop/normalize (sanitize before diff):
  - metadata: managedFields, resourceVersion, uid, creationTimestamp, deletionTimestamp
  - status: drop entirely; observedGeneration under status is dropped
  - annotations/labels: only ignored if explicitly configured (MVP: no extra ignores)
- Defaulted fields:
  - Keep server-defaulted fields (MVP) to avoid cross-cluster/version drift
- Rendering:
  - Deterministic key order; stable list ordering where defined by API
  - One K8s object per file, .yaml suffix, final newline, LF endings
- Utilities: [internal/sanitize/marshal.go](internal/sanitize/marshal.go), [internal/sanitize/sanitize.go](internal/sanitize/sanitize.go)

### Appendix B: File layout and write policy
- Path template: /{baseFolder}/{group-or-core}/{version}/{resource}/{namespace?}/{name}.yaml  (see [internal/types/identifier.go](internal/types/identifier.go))
- Namespacing:
  - Namespaced resources include namespace segment; cluster-scoped omit it
- Write policy:
  - Semantic diff → unchanged => no-op
  - Atomic write via temp + rename; mkdir -p parents as needed
  - Always add trailing newline; preserve LF endings
- Orphans:
  - S_orphan = S_git − S_live per baseFolder, capped by deleteCapPerCycle

### Appendix C: Secret handling (MVP stance)
- Included by default; data preserved as base64 (as stored by Kubernetes)
- No redaction/encryption at rest in MVP
- Future: pluggable transforms (e.g., SOPS) via destination policy (non-normative)

### Appendix D: RBAC deltas required by watch ingestion
Update ClusterRole(s) in [charts/gitops-reverser/templates/rbac.yaml](charts/gitops-reverser/templates/rbac.yaml):
- Selected resources: get, list, watch (per discovery filters)
- Leases (coordination.k8s.io): get, list, watch, create, update, patch, delete
- Events (events.k8s.io/core): create, patch
- CRDs (configbutler.ai): get, list, watch for watchrules, clusterwatchrules, gitrepoconfigs, gitdestinations; status get/update/patch where applicable
- Secrets: get (Git credentials referenced by GitRepoConfig)

### Appendix E: Helm values for watch ingestion and caps
Add/confirm in [charts/gitops-reverser/values.yaml](charts/gitops-reverser/values.yaml):
- controller:
  - enableWatchIngestion: false
  - discovery:
    - refresh: 5m
    - watchAll: false
    - exclusions:
      - [ "pods", "events", "events.events.k8s.io", "leases.coordination.k8s.io", "endpoints", "endpointslices.discovery.k8s.io", "controllerrevisions.apps", "flowschemas.flowcontrol.apiserver.k8s.io", "prioritylevelconfigurations.flowcontrol.apiserver.k8s.io", "jobs.batch", "cronjobs.batch" ]
  - workers:
    - maxGlobal: 24
    - maxPerRepo: 5
  - git.batch:
    - maxFiles: 200
    - maxBytesMiB: 10
    - maxWaitSec: 20
  - deletes.capPerCycle: 500
  - leases:
    - renewSec: 8
    - leaseSec: 24
  - workDir: "/var/cache/gitops-reverser"
Chart wiring:
- Pass enableWatchIngestion as --enable-watch-ingestion
- Expose discovery caps/exclusions as args/env
- Document in [charts/gitops-reverser/README.md](charts/gitops-reverser/README.md)

### Appendix F: Test matrix and CI gates
Mandatory (per project rules):
- make lint; make test (>90% coverage for new code); make test-e2e
Unit:
- Identifier path determinism; sanitize golden outputs; discovery include/exclude logic
Integration:
- Startup seed → writes + orphan deletes; re-run → no-op
- Multi-branch workers and caps; non-fast-forward retry path (fetch/reset/reapply)
E2E:
- Watch-only ingestion with Desired-state defaults (Jobs/CronJobs excluded)
- Lease contention (single-writer); marker exclusiveMode on/off

### Appendix G: Metrics (cardinality-safe)
Emit via [internal/metrics/exporter.go](internal/metrics/exporter.go):
- objects_scanned_total{gvk,scope}
- objects_written_total{gvk,scope}
- files_deleted_total{destination}
- commits_total{repo,branch}
- commit_bytes_total{repo,branch}
- rebase_retries_total{repo,branch}
- ownership_conflicts_total{destination}
- lease_acquire_failures_total{worker}
- marker_conflicts_total{destination}
- repo_branch_active_workers{repo,branch}
- repo_branch_queue_depth{repo,branch}
Notes: avoid per-object labels; aggregate per destination/worker.

### Appendix H: Migration and rollout notes
- Webhook retained (FailurePolicy=Ignore; leader-only) until watch-only parity; see §9 for decommission steps
- Feature flag rollout via --enable-watch-ingestion
- Gates for removal: watch-only e2e success; metrics parity; RBAC/chart/docs finalized

## Documentation status marker (update 2)
As of 2025-10-15 (UTC):
- This spec now includes normative appendices (A–H) covering canonicalization, pathing, RBAC, Helm values, tests, and metrics.
- Addendum on webhook retention remains in place; decommission sequencing unchanged.
- Worker identity reaffirmed as per-repo-branch; multiple baseFolder trees serialized by the same worker.
- Next concrete actions are mirrored with file references in: [docs/cluster-source-of-truth-plan.md](docs/cluster-source-of-truth-plan.md)

### Appendix I: Status conditions (authoritative set for MVP)

Conditions appear on Status.conditions of CRDs with standard Kubernetes semantics (type, status, reason, message, lastTransitionTime, observedGeneration). Controllers SHOULD only flip condition status when the underlying truth changes.

Applies to:
- GitRepoConfig: Ready, AccessDenied (optional, future)
- GitDestination: Ready, OwnershipConflict, LeaseNotAcquired, MarkerConflict
- WatchRule: Ready, AccessDenied, WatchErrored, DiscoveryDegraded, RepoPushFailed
- ClusterWatchRule: Ready, AccessDenied, WatchErrored, DiscoveryDegraded, RepoPushFailed

Condition types
- Ready
  - status: True | False | Unknown
  - reason (examples): Initialized, Reconciling, Error, WaitingDependencies
- AccessDenied
  - status: True when referencing repo/destination is not authorized
  - reason: RepoPolicyDenied, DestinationPolicyDenied
- OwnershipConflict
  - status: True when exclusiveMode=true and repo marker differs from identity
  - reason: MarkerMismatch, AnotherOwnerActive
- LeaseNotAcquired
  - status: True when repo-branch worker lease could not be acquired
  - reason: LeaseHeldElsewhere, LeaseAPIError
- MarkerConflict
  - status: True when marker path operations fail or mismatch detected (even when exclusiveMode=false we may warn via events; condition optional for warning)
  - reason: MarkerWriteFailed, MarkerMismatch
- WatchErrored
  - status: True when one or more informers are failing (backing off)
  - reason: ResourceVersionExpired, APIServerError, RBACDenied
- DiscoveryDegraded
  - status: True when discovery failed partially; some GVRs skipped
  - reason: PartialDiscovery, NoListWatch, ScopeMismatch
- RepoPushFailed
  - status: True when last commit batch failed to push after retry policy
  - reason: AuthFailed, NonFastForwardAfterRetry, NetworkError

Notes
- Controllers SHOULD include concise message with primary failure cause and count of affected GVRs/resources.
- observedGeneration MUST be updated on each reconcile to avoid stale conditions.

### Appendix J: Minimal CRD examples (YAML)

GitRepoConfig (namespaced)
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: repo
  namespace: configbutler-system
spec:
  repoUrl: https://git.example.com/infrastructure/configs.git
  allowedBranches:
    - main
  secretRef:
    name: repo-cred
```

GitDestination (namespaced)
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitDestination
metadata:
  name: production
  namespace: configbutler-system
spec:
  repoRef:
    name: repo
    namespace: configbutler-system
  branch: main
  baseFolder: clusters/prod
  exclusiveMode: true
```

WatchRule (namespaced)
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: shop-desired
  namespace: shop
spec:
  destinationRef:
    name: production
    namespace: configbutler-system
  rules:
    - resources: ["deployments", "services", "configmaps"]
```

ClusterWatchRule (cluster-scoped)
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: cluster-rbac
spec:
  destinationRef:
    name: production
    namespace: configbutler-system
  rules:
    - scope: Cluster
      resources:
        - "clusterroles.rbac.authorization.k8s.io"
        - "clusterrolebindings.rbac.authorization.k8s.io"
```

Paths produced (baseFolder = clusters/prod)
- Deployment apps/v1 shop/web → /clusters/prod/apps/v1/deployments/shop/web.yaml
- Service v1 shop/api → /clusters/prod/core/v1/services/shop/api.yaml
- ClusterRole rbac.authorization.k8s.io/v1 viewer → /clusters/prod/rbac.authorization.k8s.io/v1/clusterroles/viewer.yaml

References
- Identifier mapping: [internal/types/identifier.go](internal/types/identifier.go)
- Sanitization and YAML: [internal/sanitize/marshal.go](internal/sanitize/marshal.go)

### Implementation iteration marker — update 3 (2025-10-15, UTC)

Scope of this iteration (documentation only)
- Added Appendix I (Status conditions) defining canonical condition types and reasons across CRDs.
- Added Appendix J (CRD examples) with minimal, copy-pasteable manifests and expected Git paths.
- No algorithmic changes; appendices complement A–H.

Current implementation state
- Watch ingestion path (flag-gated) present with GVR aggregation, discovery filter, and dynamic informers.
- Existing queue + git worker used for batching and commits.
- Webhook retained for username capture; decommission gated.

Next concrete actions (unchanged)
- Implement startup List-based seed + orphan deletion; enforce batch caps; add leases/marker; extend metrics and RBAC; validate watch-only e2e; then execute webhook decommission steps after gates are met.

Cross-reference for operator runbook and examples
- Extended guidance and failure remediation in plan: [docs/cluster-source-of-truth-plan.md](docs/cluster-source-of-truth-plan.md)
