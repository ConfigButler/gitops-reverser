# Multi-Cluster Audit Ingestion: Implications and Configuration Mapping

## 1. Purpose

This document describes what changes are implied by supporting audit ingestion from multiple clusters, how this maps to
current `WatchRule` / `ClusterWatchRule` usage, and what new cluster connectivity model is needed for initial
reconcile and CRD discovery.

## 2. Current Baseline (Today)

1. Audit ingress already supports a path contract with cluster identity:
`/audit-webhook/<cluster-id>` (`internal/webhook/audit_handler.go:231`, `charts/gitops-reverser/README.md:225`).
2. `WatchRule` is namespaced and scoped to resources in its own namespace (`api/v1alpha1/watchrule_types.go:57`).
3. `ClusterWatchRule` is cluster-scoped and can target both cluster-scoped and namespaced resources via `scope`
(`api/v1alpha1/clusterwatchrule_types.go:59`, `api/v1alpha1/clusterwatchrule_types.go:113`).
4. There is no first-class CRD yet for remote kube-apiserver connectivity (needed for seed reconcile and dynamic GVR
planning per source cluster).

## 3. Core Implications of Multi-Cluster Ingestion

### 3.1 Identity and Routing

- `clusterID` becomes a first-class identity dimension in every event and derived key.
- Rule matching must evaluate `{clusterID, gvr, namespace, name, operation}`.
- Dedupe keys and replay tools must include `clusterID` to avoid cross-cluster collisions.

### 3.2 Rule Semantics

- Current rule model is cluster-agnostic; multi-cluster requires source-cluster selection.
- Without a source selector, one rule could unintentionally match events from all clusters.

### 3.3 Security Model

- Audit ingress needs authenticated cluster identity, not just path parsing.
- Per-cluster policy and quotas are required (fairness and blast-radius control).
- Remote kube-api credentials become sensitive assets requiring rotation and least privilege.

### 3.4 Reconciliation and Discovery

- Initial snapshot reconcile must run against each source cluster API.
- New CRD installation discovery is per source cluster, not global.
- Failures are per cluster and should not block processing for healthy clusters.

### 3.5 Operational/HA Impact

- Metrics/alerts require `clusterID` labels with cardinality controls.
- Backpressure, dead-letter, and lag need per-cluster visibility.
- Noisy cluster isolation is required to protect global throughput.

## 4. Mapping to Existing Rule Model

### 4.1 Recommendation for V1 (Pragmatic)

Use `ClusterWatchRule` as the primary multi-cluster policy object with an optional source-cluster selector.
Keep `WatchRule` for single-cluster/local management-cluster use.

Why this is pragmatic:

1. `ClusterWatchRule` already supports both `Cluster` and `Namespaced` resource scopes.
2. It avoids redefining namespace semantics for remote clusters in a namespaced CRD.
3. It gives one place to express global policy and source-cluster targeting.

### 4.2 Proposed Field Additions

### A. `ClusterWatchRule` (recommended now)

Add optional source selector:

```yaml
spec:
  source:
    clusterIDs: ["prod-eu-1", "prod-us-1"]  # empty/omitted = all onboarded clusters
```

### B. `WatchRule` (optional future extension)

If needed later, add a constrained source selector:

```yaml
spec:
  source:
    clusterIDs: ["dev-eu-1"]
```

For initial multi-cluster rollout, avoid this extension unless there is a hard requirement for namespace-scoped,
team-owned cross-cluster policies.

### 4.3 Mapping Examples

| Current usage | Multi-cluster equivalent |
|---|---|
| `WatchRule` in namespace `team-a` for `configmaps` | Keep as-is for local cluster, or migrate to `ClusterWatchRule` with `scope: Namespaced` and `source.clusterIDs` |
| `ClusterWatchRule` for CRDs/nodes | Add `source.clusterIDs` to scope clusters explicitly |
| "Watch all namespaces" policy | `ClusterWatchRule` + `scope: Namespaced` + optional namespace filters + `source.clusterIDs` |

### 4.4 Example Translation (WatchRule -> ClusterWatchRule)

Current namespaced rule:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: configmaps-team-a
  namespace: team-a
spec:
  targetRef:
    name: team-a-target
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: [\"\"]
      apiVersions: [\"v1\"]
      resources: [configmaps]
```

Multi-cluster equivalent:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: configmaps-team-a-multi-cluster
spec:
  targetRef:
    name: team-a-target
    namespace: team-a
  source:
    clusterIDs: [\"prod-eu-1\", \"prod-us-1\"]
  rules:
    - scope: Namespaced
      operations: [CREATE, UPDATE, DELETE]
      apiGroups: [\"\"]
      apiVersions: [\"v1\"]
      resources: [configmaps]
```

### 4.5 GitTarget Provenance Model (Important)

By default, a single `GitTarget` should represent **one source cluster provenance**.

Why:

1. Audit trails remain clear ("this file came from cluster X").
2. Drift/debug workflows stay understandable.
3. Operational blast radius is reduced.

Recommendation:

1. Do not mix multiple source clusters into one `GitTarget` by default.
2. If mixing is ever needed, require explicit opt-in and enforce path partitioning per cluster (for example
`clusters/<clusterID>/...`).

## 5. Proposed New Connectivity Model

Multi-cluster audit ingestion is not enough by itself. You also need control-plane connectivity to each source cluster
for:

1. Initial reconcile (list current resources)
2. CRD discovery / GVR planning
3. Periodic drift checks and orphan detection

### 5.1 New CRD Proposal: `SourceCluster`

Introduce a dedicated cluster onboarding CRD, e.g. `SourceCluster`:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: SourceCluster
metadata:
  name: prod-eu-1
spec:
  clusterID: prod-eu-1
  auditIngress:
    authMode: mTLS # or BearerToken
    credentialSecretRef:
      name: sourcecluster-prod-eu-1-audit
  kubeAPI:
    mode: Direct # Direct | Proxy | Disabled
    kubeconfigSecretRef:
      name: sourcecluster-prod-eu-1-kubeconfig
    qps: 20
    burst: 40
  reconcile:
    enabled: true
  limits:
    maxEventsPerSecond: 500
status:
  conditions: []
```

### 5.2 Why Separate `SourceCluster` from Rules

- Rules express "what to capture".
- `SourceCluster` expresses "where and how to connect".
- Separation improves security, ownership, and rotation workflows.

### 5.3 Connectivity Modes for Kube API

1. `Direct`: central controller connects directly to remote kube-apiserver using kubeconfig/credential secret.
2. `Proxy`: source-cluster agent or gateway exposes constrained API for list/discovery operations.
3. `Disabled`: no kube-api access for this source; audit ingest only (no snapshot/discovery from that cluster).

For initial reconcile and CRD discovery, `Direct` or `Proxy` is required.

### 5.4 Deployment Topology Options

#### Option A: One GitOps Reverser per source cluster (simplest app model)

Model:

1. Deploy one controller per cluster (often inside that same cluster).
2. Configure kube-api endpoint at deployment level (default in-cluster, optional override).
3. No cluster-specific selection fields required in rule CRDs for that deployment.

Implications:

- Pros: lowest application complexity, clear ownership boundaries, simpler CRD model.
- Cons: fleet-level aggregation requires external orchestration; more deployments to manage.

Suggested deployment-level config knobs:

- `kubeAPI.mode=inCluster|override`
- `kubeAPI.server=https://...` (used when `override`)
- `kubeAPI.authSecretRef=...`
- `cluster.identity=<clusterID>` (used in emitted metadata)

#### Option B: Central hosting cluster with multi-cluster ingestion/control plane

Model:

1. One or more controllers run in a hosting cluster.
2. `SourceCluster` defines audit auth + kube-api connectivity for each source cluster.
3. Rules may target one or more `clusterIDs`.

Implications:

- Pros: centralized operations, single control plane, easier global governance.
- Cons: higher product complexity (cluster-aware routing, auth, fairness, isolation).

#### Option C: Hybrid

1. Local per-cluster deployments for most teams.
2. Central deployment only for selected clusters/use-cases.

This can share the same CRDs, with `SourceCluster` used only where centralized mode is enabled.

## 6. Event and Reconcile Flow with `SourceCluster`

```text
Source Cluster A/B/C
  -> POST /audit-webhook/<cluster-id>
  -> authenticate via SourceCluster credentials
  -> normalize event (clusterID required)
  -> rule match (ClusterWatchRule + source selector)
  -> enqueue to durable bus
  -> writer lease by repo+branch partition
  -> git commit/push

Startup / rule change
  -> list active SourceClusters
  -> create per-cluster discovery + snapshot jobs
  -> publish reconcile events tagged with clusterID
  -> same write pipeline + status projection
```

## 7. Security and RBAC Implications

1. `SourceCluster` CRUD should be restricted to platform admins.
2. Secrets for remote kube-api and audit auth should be namespace-local and tightly RBACed.
3. Add validation that `spec.clusterID` is unique and immutable.
4. Enforce explicit allow-listing of accepted `clusterID`s at ingress.

## 8. Failure Modes and Required Behavior

1. Source cluster audit down: continue ingest from other clusters.
2. Source cluster kube-api unreachable: skip snapshot/discovery for that cluster; keep live ingest if available.
3. Bad credentials for one cluster: mark that `SourceCluster` degraded; do not block others.
4. Per-cluster event spike: apply per-cluster quota and backpressure before global degradation.

## 9. Suggested Implementation Paths

### Path A (Per-cluster deployment first)

1. Add deployment-level kube-api connectivity config (`inCluster` default + optional override server/auth).
2. Keep `WatchRule`/`ClusterWatchRule` semantics unchanged within each deployment.
3. Enforce one-cluster-per-`GitTarget` provenance at deployment level.
4. Add fleet docs/automation for managing many deployments.

### Path B (Central multi-cluster control plane)

1. Add `SourceCluster` CRD and controller (status + connectivity checks).
2. Add ingress auth that maps request to registered `SourceCluster`.
3. Add `source.clusterIDs` selector to `ClusterWatchRule`.
4. Add cluster-aware matching in rule compiler and event identity.
5. Add per-cluster snapshot/discovery workers using `SourceCluster.kubeAPI` credentials.
6. Add per-cluster lag/quota metrics and alerts.
7. Optionally evaluate `WatchRule` source selector later.

### Recommendation

If you want to reduce rewrite risk, start with **Path A** and keep the application model simple.  
If centralized governance is the immediate requirement, implement **Path B** directly.

## 10. Direct Answer to "ClusterWatchRule only?"

For the first multi-cluster version: **yes, that is the cleanest approach**.

- Use `ClusterWatchRule` with optional `source.clusterIDs`.
- Keep `WatchRule` unchanged for local/simple namespaced use.
- Revisit extending `WatchRule` only if multi-tenant namespace-owned cross-cluster policy becomes a strong requirement.

Direct answer to your deployment thought:

1. Yes, allowing in-cluster deployment (local kube-api by default) is sensible and should be first-class.
2. Yes, allowing deployment-level kube-api override is useful for a one-reverser-per-cluster model.
3. In centralized mode, `SourceCluster` is still required for remote connectivity/auth.
