# GitOps Reverser: Cluster-as-Source-of-Truth — consolidated specification (MVP)

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

End of specification.