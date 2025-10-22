> **📜 DOCUMENT STATUS: AUTHORITATIVE SPECIFICATION**
> **This is the definitive MVP specification. Implementation tracked in [DESIGN_STATUS.md](DESIGN_STATUS.md).**
> **Last Updated:** 2025-10-20

# GitOps Reverser: Cluster-as-Source-of-Truth — concise specification (MVP)

Scope
- Treat the live Kubernetes cluster as the source of truth for selected scopes
- Ingest state via List + Watch; admission webhooks are not used for object ingestion
- Write canonical YAML to Git under a baseFolder; one K8s object per file
- Keep configuration minimal; fixed batching bytes trigger = 1 MiB

Why both ValidatingWebhook and Watch (authoritative rationale)
- ValidatingWebhook: the reliable source of the admission username used for commit metadata. Retain permanently at /process-validating-webhook with FailurePolicy=Ignore and leader-only routing; see [cmd.main()](cmd/main.go:1) and [webhook.event_handler()](internal/webhook/event_handler.go:1)
- Watch: the durable confirmation that a change actually persisted into etcd; webhook calls can be dropped or rejected under load, but Watch streams reflect committed state
- Ordering caveat: Kubernetes provides different resourceVersion values for admission-time objects vs watch-time objects, and does not guarantee any cross-stream ordering. We never order across the two; we correlate via sanitization-based identity.

1. Minimal CRD surfaces (configbutler.ai/v1alpha1)
- GitRepoConfig (Namespaced) — repo connectivity and branch allowlist
  - spec.repoUrl: string
  - spec.allowedBranches: []string
  - spec.secretRef: LocalObjectReference (optional)
  - Status: conditions, observedGeneration
  - Reference: [api.gitrepoconfig types](api/v1alpha1/gitrepoconfig_types.go:1)
- GitDestination (Namespaced) — bind repo+branch+baseFolder (owned subtree)
  - spec.repoRef: NamespacedName (to GitRepoConfig)
  - spec.branch: string (∈ allowedBranches)
  - spec.baseFolder: string (POSIX-like, relative)
  - Status: conditions (e.g., Ready)
- WatchRule (Namespaced) — select namespaced resources in its own namespace
  - spec.destinationRef: NamespacedName (to GitDestination)
  - spec.rules[]: { operations?, apiGroups?, apiVersions?, resources }
- ClusterWatchRule (Cluster) — select cluster-scoped and/or namespaced resources
  - spec.destinationRef: NamespacedName (to GitDestination)
  - spec.rules[]: as above + scope=Cluster|Namespaced
Notes
- No accessPolicy, no objectSelector, and no namespaceSelector in MVP (kept simple on purpose)
- Defaults fill in desired-state resource selection when resources are omitted

2. Ingestion and discovery (watch-only for objects)
- Aggregate requested GVRs from rules; compute concrete GVRs; see [watch.Manager.ComputeRequestedGVRs()](internal/watch/manager.go:72)
- Filter by API discovery (list+watch only), scope-aware; see [watch.FilterDiscoverableGVRs()](internal/watch/discovery.go:44)
- Start dynamic informers for discoverable GVRs; handlers: [watch.startDynamicInformers()](internal/watch/informers.go:48), [watch.addHandlers()](internal/watch/informers.go:82), [watch.handleEvent()](internal/watch/informers.go:105)
- Watch config: resourceVersionMatch=NotOlderThan; allowWatchBookmarks=true; restart backoff 500ms..30s; re-list on Expired
- Periodic discovery refresh: 5m default (start new informers, stop removed ones)

3. Signal correlation (sanitization-based; in-memory KV with TTL/LRU)
- Sanitization first for both signals: produce stable YAML/spec via [sanitize.MarshalToOrderedYAML()](internal/sanitize/marshal.go:31) and drop fields in [sanitize.Sanitize()](internal/sanitize/sanitize.go:1)
- Keying: ResourceIdentifier (GVK/ns/name) + Operation + short hash of sanitized spec; identifier via [types.ResourceIdentifier.ToGitPath()](internal/types/identifier.go:62)
- Store (webhook path): on admission, Put(key → {username, ts}) into an in-memory KV (TTL ~60s, LRU bounded)
- Lookup (watch path): on watch event, sanitize, compute key, GetAndDelete; on hit, enrich event with username; on miss, use bot/UnknownUser
- No cross-stream ordering: resourceVersions differ and are not orderable; correlation uses identity+content within a small time window
- Metrics: enrich_hits_total, enrich_misses_total, kv_evictions_total (exported via [metrics.exporter](internal/metrics/exporter.go:1))
- Failure modes: dropped webhook → miss; duplicates → last-write-wins; churn → bounded by LRU and TTL

4. Reconciliation algorithm
- Seed
  - List() each selected GVR; capture resourceVersion
  - Sanitize and canonicalize object YAML; map to path /{baseFolder}/{identifierPath} via [types.ResourceIdentifier.ToGitPath()](internal/types/identifier.go:62)
  - Enqueue upsert events (semantic no-op if unchanged on disk)
- Orphans (per baseFolder)
  - S_live = paths from current List()
  - S_git = tracked *.yaml under baseFolder
  - Delete S_git − S_live (uncapped; Git history allows revert)
- Trail
  - Start Watch() from captured resourceVersion; sanitize; KV-enrich; enqueue upserts/deletes
  - On Expired: re-list, rebuild S_live, recompute deletes
- Idempotency: no semantic change ⇒ no commit

5. Batching and limits (fixed defaults, minimal configuration)
- Flush when any of:
  - maxFiles=200
  - maxBytes=1 MiB (bytes trigger fixed to 1 MiB)
  - maxWait=20s
- Workers: maxPerRepo=5; maxGlobal=24; workDir=/var/cache/gitops-reverser
- Watch: bookmarks=true; rvMatch=NotOlderThan; backoff=500ms..30s

6. Git policy (go-git)
- One worker per (repoURL, branch); dedicated clone at /var/cache/gitops-reverser/{hash(repoURL,branch)}
- Fast-forward push; on reject: fetch remote tip, reset --hard, reapply pending changes, push; see [git.Repo.TryPushCommits()](internal/git/git.go:181)
- No merges; rebase-like replay; commit trailers:
  - X-ConfigButler-ClusterUID, X-ConfigButler-ControllerNS, X-ConfigButler-ControllerName, X-ConfigButler-InstanceID

7. Observability (cardinality-safe)
- Metrics via [internal/metrics/exporter.go](internal/metrics/exporter.go:1):
  - objects_scanned_total, objects_written_total, files_deleted_total
  - commits_total, commit_bytes_total, rebase_retries_total
  - repo_branch_active_workers, repo_branch_queue_depth
  - enrich_hits_total, enrich_misses_total, kv_evictions_total
- Logs: identifiers, destination, commit SHAs; Events for major actions with enrichment result

8. Security and RBAC (chart)
- Selected resources: get, list, watch (desired-state baseline)
- events: create, patch
- configbutler.ai: watchrules, clusterwatchrules, gitrepoconfigs, gitdestinations (get, list, watch); status updates for rules (if any)
- secrets: get (repo credentials)
- Templates: [charts.git rbac](charts/gitops-reverser/templates/rbac.yaml:1)

9. Helm flags (minimal)
- --enable-watch-ingestion
- --discovery-refresh=5m, --watch-all=false, repeated --discovery-exclude=...
- --git-batch-max-files=200, --git-batch-max-bytes-mib=1, --git-batch-max-wait-sec=20
- --workers-max-global=24, --workers-max-per-repo=5
- --work-dir=/var/cache/gitops-reverser
- Values wiring: [charts.deployment](charts/gitops-reverser/templates/deployment.yaml:1), [charts.values](charts/gitops-reverser/values.yaml:1)

10. Testing and CI gates (mandatory)
- make lint (golangci-lint), make test (>90% coverage for new code), make test-e2e (Docker/Kind); see [Makefile](Makefile:1)
- Core scenarios:
  - Seed → writes + deletes; second run → no-op
  - Desired-state default filters: Pods/Events excluded; Deployments/Services included
  - Non-fast-forward retry path remains stable
- Correlation tests:
  - Unit: sanitize equivalence (webhook vs watch), key determinism, TTL/LRU behavior, no reliance on resourceVersion ordering
  - Integration: webhook put then watch hit under load; dropped webhook → miss and metrics; commit trailers reflect username on hits
  - E2E: high-rate updates; enrichment hit rate within TTL; stable throughput; no deadlocks

11. Out of scope (MVP)
- Ownership/conflict control of the target repo (e.g., Kubernetes Lease per repo-branch worker, repo ownership marker files, commit blocking on ownership), and any exclusiveMode
- CRD accessPolicy and policy-based authorization for referencing repos/destinations
- Selectors (objectSelector on WatchRule, namespaceSelector on ClusterWatchRule)
- Migration/compatibility concerns (alpha project; no migration path is maintained)

References (authoritative anchors)
- Manager lifecycle: [watch.Manager.Start()](internal/watch/manager.go:66), seed: [watch.Manager.seedSelectedResources()](internal/watch/manager.go:185)
- Informers: [watch.startDynamicInformers()](internal/watch/informers.go:48), [watch.addHandlers()](internal/watch/informers.go:82), [watch.handleEvent()](internal/watch/informers.go:105)
- Event queue and worker: dispatch: [git.Worker.dispatchEvents()](internal/git/worker.go:92), loop: [git.Worker.processRepoEvents()](internal/git/worker.go:178), buffer: [git.Worker.handleNewEvent()](internal/git/worker.go:300), ticker: [git.Worker.handleTicker()](internal/git/worker.go:323), commit: [git.Worker.commitAndPush()](internal/git/worker.go:338)
- Identifier mapping: [types.ResourceIdentifier.ToGitPath()](internal/types/identifier.go:62)
- Webhook assets: [charts.validating-webhook](charts/gitops-reverser/templates/validating-webhook.yaml:1), [config.webhook kustomization](config/webhook/kustomization.yaml:1)

Status marker (2025-10-20)
- ✅ Core architecture implemented: List+Watch, webhook correlation, Git operations
- ⚠️ Dynamic informers: Partial (starts at pod startup, needs controller integration)
- ✅ Fixed 1 MiB batching, uncapped deletes, 91.6% correlation test coverage
- 🚧 **In Progress:** Controller→WatchManager integration for dynamic reconciliation (see IMPLEMENTATION_ROADMAP.md)
- ✅ Single-pod MVP documented; multi-pod coordination is future work
- ✅ Ownership and access policy explicitly out of scope per MVP
