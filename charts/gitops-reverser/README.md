# GitOps Reverser Helm Chart

GitOps Reverser enables synchronization from Kubernetes to one or more Git repositories.

## Quick Start

Follow the [quickstart](../../docs/quickstart.md) for installation and your first Git commit.

Use that guide for cert-manager, Helm install, Git credentials, starter
`GitProvider`/`GitTarget`/`WatchRule` resources, and first-commit verification. Audit delivery from
`kube-apiserver` is optional and can be added later for named commit authors.

This chart README stays focused on chart-specific installation, configuration, and operations.

For the chart's optional starter `quickstart` block, see [`docs/configuration.md`](../../docs/configuration.md).

## Features

- ✅ **Git synchronization**: Push Kubernetes changes back to Git repositories
- ✅ **Single-pod stability**: 1 replica by default while multi-pod support is in progress
- ✅ **Automatic CRD installation**: KubeProvider, GitProvider, GitBranch, GitTarget, WatchRule,
  ClusterWatchRule, CommitRequest, FluxClaim, and ArgoCDClaim CRDs installed automatically
- ✅ **Watch-based ingestion**: object state comes from the Kubernetes watch API
- ✅ **Optional audit attribution**: name commit authors from kube-apiserver audit events over HTTPS
- ✅ **Prometheus metrics**: Built-in monitoring support

## Prerequisites

- Kubernetes 1.36 (probably works for older versions, but not tested)
- cert-manager (for TLS certificates)
- **Valkey or Redis: optional but advised.** The default install runs without it in
  `configured-author` mode (watches cold-replay on restart). Add it to unlock warm-restart resume
  cursors, CommitRequest author capture (the admission webhook), and attributed-author mode; enabling
  any of those requires a non-empty `queue.redis.addr`.
- **Optional, attributed-author mode only:** kube-apiserver audit delivery, which adds
  mirrored-resource commit-author attribution. Without it the operator still mirrors state, authored by
  the configured committer.

## Installation

### From OCI Registry (Recommended)

Install the latest version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace
```

After installation, print the chart's post-install instructions with:

```bash
helm get notes gitops-reverser --namespace gitops-reverser
```

Install a specific version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --version 0.3.0 \
  --namespace gitops-reverser \
  --create-namespace
```

## Architecture

### Deployment Topology

The chart deploys 1 replica by default:

```text
┌─────────────────────────────────────────┐
│         Kubernetes API Server           │
└──────────────┬──────────────────────────┘
               │
               │ metrics requests
               ▼
┌──────────────────────────────────────────┐
│        gitops-reverser (Service)         │
│             Port: metrics(8080)          │
└──────────────┬───────────────────────────┘
               │
               ▼
        ┌─────────────┐
        │ Pod 1       │
        │ Controller  │
        │ Active      │
        └─────────────┘
               ▲
               │ optional audit requests
┌──────────────────────────────────────────┐
│      gitops-reverser-audit (Service)     │
│             Port: audit(9444)            │
└──────────────────────────────────────────┘
```

**Key Features:**

- **Single-pod operation**: minimal moving parts while HA work is deferred
- **Split Service topology**: metrics stay in-cluster while audit ingress is exposed separately only
  when `attribution.enabled=true`

## Configuration

### Common Configuration Examples

#### Minimal (Testing/Development)

Single replica:

```yaml
# minimal-values.yaml
replicaCount: 1
podDisruptionBudget:
  enabled: false
affinity: {}
```

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace \
  --values minimal-values.yaml
```

#### Hardened Single-Replica

Tighter defaults than the minimal install: a disruption budget, and the settings worth reviewing
against your own environment's policy before you rely on it.

```yaml
# hardened-values.yaml
replicaCount: 1

podDisruptionBudget:
  enabled: true
  minAvailable: 1

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 1000m
    memory: 1Gi

monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s

nodeSelector:
  node-role.kubernetes.io/worker: ""
```

### Configuration Reference

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas. One running replica is supported; a higher value is rejected while Helm renders the chart, rather than silently running two writers | `1` |
| `image.repository` | Container image repository | `ghcr.io/configbutler/gitops-reverser` |
| `env` | Extra container env vars, as Kubernetes `EnvVar` entries | `[]` |
| `volumes` / `volumeMounts` | Extra pod volumes and their mounts, appended as-is | `[]` |
| `servers.enableHTTP2` | Serve HTTP/2 on the TLS servers. Off by default: disabling it mitigates the HTTP/2 Rapid-Reset CVE class | `false` |
| `servers.audit.bindAddress` | host:port the audit ingress server binds to (`--audit-bind-address`) | `0.0.0.0:9444` |
| `servers.audit.port` | Audit container/Service port; must match the port in `bindAddress` | `9444` |
| `servers.audit.tls.enabled` | Serve audit ingress with TLS | `true` |
| `servers.audit.maxRequestBodyBytes` | Max accepted audit request size | `10485760` |
| `servers.audit.timeouts.read` | Audit-server read timeout | `15s` |
| `servers.audit.timeouts.write` | Audit-server write timeout | `30s` |
| `servers.audit.timeouts.idle` | Audit-server idle timeout | `60s` |
| `servers.audit.tls.secretNameOverride` | Override Secret name for audit TLS cert/key | `<release>-audit-server-cert` |
| `servers.receiver.enabled` | Serve the push receiver, over plain HTTP, for Git hosts' push webhooks to `GitReceiver.status.webhookPath`, and add a `<release>-receiver` Service; expose it with an Ingress or Gateway route of your own that terminates TLS. See [push receiver settings](../../docs/configuration.md#push-receiver-settings) | `false` |
| `servers.receiver.bindAddress` | host:port the push receiver binds to (`--receiver-bind-address`) | `:9292` |
| `servers.receiver.port` | Push receiver container port; must match the port in `bindAddress` | `9292` |
| `controllerManager.additionalSensitiveResources` | Extra Secret-shaped resource types encrypted as `resource` or `group/resource` | `[]` |
| `controllerManager.inClusterKubeProviders` | `KubeProvider`s, as `namespace/name`, that may use `spec.inCluster`, the operator's own identity (`--in-cluster-kube-providers`). Exact names, no wildcards. The provider the chart renders is added on its own when it uses `inCluster`, so list only the others. One that sets `inCluster` and is not listed reports `InClusterNotAuthorized` and is never used | `[]` |
| `controllerManager.watchNamespaces` | Config-plane namespaces this release reconciles (`--watch-namespaces`), so two releases can share one cluster. A release reconciles the `GitTarget`s, rules, providers and branches in these namespaces only, a `ClusterWatchRule` only when its `GitTarget` is in one of them, and scopes its admission webhook to them. List every namespace whose objects reference each other. See [two instances in one cluster](../../docs/configuration.md#two-instances-in-one-cluster) | `[]` (every namespace) |
| `controllerManager.insecureAllowGitHTTP` | Permit credentials with `http://` GitProvider URLs for trusted in-cluster development Git servers | `false` |
| `controllerManager.branchWorkerQueueDepth` | Event queue depth per branch worker. A full queue refuses the write (`git_queue_refusals_total`) instead of stalling the watch path; the watch delivers it again from its cursor, and only a cursor that expires first loses that write's own commit (and, under `prune.mode: OnEvent`, a delete's removal). Costs queue depth × payload of pod memory, which `branchBufferMaxSize` does **not** cover | `1000` |
| `controllerManager.branchBufferMaxSize` | Maximum in-memory event buffer per branch worker. Bounds the open commit window plus the writes retained for replay until a push succeeds. While the remote cannot be reached, retained writes that reach it close the worker to new writes, saves and resyncs until they are published; the watch delivers a refused event again | `8Mi` |
| `controllerManager.commitRequestTTL` | How long a `CommitRequest` is kept once it finishes. Written onto the request as the `configbutler.ai/delete-after` annotation; remove or edit that to keep one. `"0"` writes none | `48h` |
| `git.refreshInterval` | How often an idle `GitTarget` re-proves where its branch is on the Git remote. `"0"` disables it | `10m` |
| `applierAccess.flux` | Grant the operator `get`, `list` and `patch` on Flux `Kustomizations`, `get` and `patch` on `GitRepositories`, `list` on `HelmReleases` and `HelmCharts` (to find what else applies a source), and `create` on `SelfSubjectAccessReviews`, so it can hold the `Kustomization` a `FluxClaim` names suspended and resume it for each apply | `false` |
| `applierAccess.argocd` | Grant the operator `get`, `update` and `patch` on Argo CD `Applications`, and `create` on `SelfSubjectAccessReviews`, so it can hold the automated sync of the `Application` an `ArgoCDClaim` names off and start its syncs | `false` |
| `auditService.type` | Service type for the dedicated audit Service | `NodePort` |
| `auditService.nodePort` | Fixed NodePort for the audit Service when `auditService.type=NodePort` | `30444` |
| `auditService.clusterIP` | Optional fixed ClusterIP for the dedicated audit Service | `""` |
| `quickstart.enabled` | Create starter `GitProvider`, `GitBranch`, `GitTarget`, and `WatchRule` resources, and grant exactly `quickstart.namespace` its own namespace on the `KubeProvider` the chart renders. The `GitTarget` mirrors through that provider, so it requires `kubeProvider.create` | `false` |
| `quickstart.namespace` | Namespace for the starter quickstart resources (create it, and the git-creds Secret, before install) | `gitops-reverser-quickstart-demo` |
| `quickstart.createNamespace` | Let the chart create `quickstart.namespace` (Helm then owns it) | `false` |
| `quickstart.gitProvider.url` | Repository URL used by the starter `GitProvider` | `""` |
| `quickstart.gitProvider.secretRef.name` | Existing Secret name used by the starter `GitProvider` | `git-creds` |
| `quickstart.gitProvider.branchPatterns` | Branch patterns the quickstart namespace may claim through the starter `GitProvider` (the `branchPatterns` of a `from: Same` grant) | `["*"]` |
| `quickstart.gitBranch.branch` | Branch the starter `GitBranch` writes. Must match `quickstart.gitProvider.branchPatterns` | `main` |
| `quickstart.gitTarget.path` | Repository path used by the starter `GitTarget`; set `.` only to deliberately target the repo root | `live-cluster` |
| `quickstart.watchRule.rules` | Rules used by the starter `WatchRule` | `configmaps create/update/delete` |
| `queue.redis.addr` | Redis/Valkey endpoint (`host:port`). Optional but advised: empty means cold-replay on restart. Set it for warm-restart cursors and for the admission webhook to actually record CommitRequest authors (admission runs as a no-op without it); **required** when `attribution.enabled=true` unless `attribution.transport=memory` | `""` |
| `queue.redis.auth.existingSecret` | Name of a pre-created Secret holding the Redis password (only used when `queue.redis.addr` is set) | `""` |
| `queue.redis.auth.existingSecretKey` | Key within the Secret that holds the password | `password` |
| `queue.redis.auth.username` | Optional Redis ACL username | `""` |
| `queue.redis.db` | Redis logical database index. Redis offers 16; use `queue.redis.keyPrefix` to go past that | `0` |
| `queue.redis.keyPrefix` | Root of every key this release writes (watch cursors, attribution facts, command author records). Give each reverser its own prefix to share one Redis/Valkey between more reversers than `db` can separate. Changing it orphans the previous prefix's keys: cursors cold-replay once, which is safe. Allowed: `[A-Za-z0-9]`, `-`, `_`, `.`, `:` | `gitops-reverser` |
| `queue.redis.tls.enabled` | Enable TLS for Redis connection | `false` |
| `attribution.enabled` | Run audit ingress and name mirrored-resource commit authors from matching kube-apiserver audit facts | `false` |
| `attribution.transport` | Where attribution facts travel between the audit receiver and the watch side. `redis` appends them to Redis streams and needs `queue.redis.addr`; `memory` keeps them in an in-process ring, which needs no Redis but loses facts on a restart and is **refused at startup with `replicaCount > 1`** | `redis` |
| `attribution.ttl` | How long an attribution fact is retained waiting for the matching watch event to join it. Bounds stream retention and the in-memory index together, and is the horizon a restart replays from | `10m` |
| `attribution.grace` | Bounded per-event wait for a matching audit fact before a live change ships with an unresolved author | `3s` |
| `attribution.maxFactsPerType` | Cap on the facts held in memory for one (audit route, type), evicted oldest-first, so a burst on one noisy type cannot evict every other type's facts | `4096` |
| `attribution.maxFacts` | Cap on the facts held in memory across every type. Must be at least `maxFactsPerType`; overflow evicts from the type holding the most | `65536` |
| `attribution.collectionWindow` | How long after a `deletecollection` a removal in its scope may still be credited to it. It only has to cover audit batching plus clock skew, since the removal is attributed at delete-request time | `30s` |
| `attribution.collectionUIDCap` | How many object UIDs a `deletecollection` fact carries before the set is dropped and the join falls back to scope matching, which is already correct | `10000` |
| `attribution.auditRouteAnnotationKey` | Audit-event annotation naming the **audit route** each event belongs to. Empty keeps audit routes named (`/audit-webhook/<audit-route>`). Set it only for a control plane emitting **one shared audit stream** for several logical clusters: it enables the bare `/audit-webhook`, which reads the route per event. A `KubeProvider` joins a route via `spec.attribution.auditRoute` (default: `<namespace>.<name>`). An event with no annotation is rejected (counted and logged) and never credited to a fallback | `""` |
| `kubeProvider.create` | Render and own a `KubeProvider` named `kubeProvider.name` in the release namespace. Every `GitTarget` names its provider in `spec.kubeProviderRef`, and nothing falls back to this one. The **operator never creates one**, so without this you commit the object yourself. Chart-owned: turning it off makes Helm delete the provider it created, and a `GitTarget` referencing a missing provider is held unready (`KubeProviderNotFound`). `quickstart` requires it | `true` |
| `kubeProvider.name` | Name of the rendered provider. Its audit route is `<release namespace>.<name>`, the `/audit-webhook/<route>` path the API server posts to | `local` |
| `kubeProvider.secretRef.name` | Secret in the release namespace holding a kubeconfig for a **remote** cluster. Empty renders `spec.inCluster`, the operator's **own** identity, and lists the provider in `--in-cluster-kube-providers`. Immutable once created: delete and recreate to repoint | `""` |
| `kubeProvider.secretRef.key` | Key within that Secret. Empty reads `value` then `value.yaml` | `""` |
| `kubeProvider.allowedGitTargets` | Rendered as the provider's `spec.allowedGitTargets`: which namespaces' `GitTarget`s may mirror through it, and what. Each grant has a `consumerNamespaces` block (`from`: `Same`, `Names`, `Selector`, `All`), and `sourceNamespaces`, `clusterResources` or both beside it. **Unset, the field is omitted and the CRD defaults it to the release namespace mirroring its own**; a list is the whole policy, and `[]` admits nobody, the release namespace included: a refused `GitTarget` names `spec.allowedGitTargets`. `quickstart.enabled` adds a grant for the quickstart namespace, and [`values-playground.yaml`](values-playground.yaml) grants every namespace its own. With an `inCluster` provider a grant bypasses live namespace RBAC through the operator's identity | `null` |
| `kubeProvider.allowedApplierClaims` | Rendered as the provider's `spec.allowedApplierClaims`: which namespaces' `FluxClaim`s and `ArgoCDClaim`s may hold an applier through it (`consumerNamespaces`), and in which `applierNamespaces` it may live, such as `flux-system`. Admission to mirror is no admission to apply. Unset, the CRD defaults it to the release namespace holding appliers in its own; `[]` admits nobody | `null` |
| `servers.admission.enabled` | Install the validate-operator-types admission webhook that captures CommitRequest authors (a form of author attribution). Enabled by default; a no-op until `queue.redis.addr` is set | `true` |
| `rbac.create` | Create the manager ClusterRole and its binding | `true` |
| `rbac.watchTypes.mode` | Which types a `WatchRule` may read. `any` grants cluster-wide read on everything, including every Secret in the cluster. `selected` grants read on `rbac.watchTypes.selected` only, so the reverser cannot list or watch Secrets (it keeps `get` on named Secrets it is pointed at). See [`docs/rbac.md`](../../docs/rbac.md) | `any` |
| `rbac.watchTypes.selected` | Types to grant when `mode: selected`, as `{apiGroups, resources}` entries (verbs are always `get,list,watch`). Required and non-empty in that mode; `namespaces`, `customresourcedefinitions` and `apiservices` come from the manager role and must not be restated | `[]` |
| `servers.metrics.bindAddress` | Metrics listener bind address | `:8080` |
| `servers.metrics.tls.enabled` | Serve metrics with TLS | `false` |
| `servers.metrics.tls.certPath` | Metrics TLS certificate mount path | `/tmp/k8s-metrics-server/metrics-server-certs` |
| `servers.metrics.tls.secretNameOverride` | Override Secret name for metrics TLS cert/key | `<release>-metrics-server-cert` |
| `service.clusterIP` | Optional fixed ClusterIP for single controller Service | `""` |
| `service.ports.audit` | Service port for audit ingress when `attribution.enabled=true` | `9444` |
| `service.ports.receiver` | Port of the `<release>-receiver` Service when `servers.receiver.enabled=true` | `80` |
| `service.ports.metrics` | Service port for metrics | `8080` |
| `servers.healthProbe.bindAddress` | Liveness/readiness probe bind address (`--health-probe-bind-address`) | `:8081` |
| `servers.admission.port` | Admission webhook container/Service port | `9443` |
| `servers.admission.timeoutSeconds` | Admission webhook timeout (failurePolicy is Ignore) | `2` |
| `servers.admission.tls.certManager` | Mint the admission serving cert via cert-manager (false = BYO via `secretNameOverride`) | `true` |
| `servers.admission.tls.secretNameOverride` | Override Secret name for the admission serving cert | `<release>-admission-server-cert` |
| `certManager.enabled` | Use cert-manager to mint the chart's serving/client certs | `true` |
| `certManager.issuer.name` | Name of the (shared) cert-manager issuer | `selfsigned-issuer` |
| `certManager.issuer.create` | Create the self-signed issuer (set false to reuse an existing one) | `true` |
| `servers.metrics.tls.certManager` | Mint the metrics serving cert via cert-manager (used when `servers.metrics.tls.enabled`) | `true` |
| `servers.audit.tls.certManager` | Let the chart mint audit TLS Secrets (server + kube-apiserver client) via cert-manager when `attribution.enabled=true` | `true` |
| `servers.audit.tls.rootCA.secretNameOverride` | Override Secret name for the audit root CA | `<release>-audit-root-ca` |
| `servers.audit.tls.client.duration` | Lifetime of the kube-apiserver audit client cert; long by default to avoid repeated manual kube-apiserver reconfiguration | `87600h` |
| `servers.audit.tls.client.renewBefore` | Renew the kube-apiserver audit client cert before expiry; shorter values increase control-plane maintenance frequency | `720h` |
| `servers.audit.tls.client.secretNameOverride` | Override Secret name for the kube-apiserver audit client cert | `<release>-audit-client-cert` |
| `auditKubeconfig.insecureSkipTLSVerify` | Render Helm notes with `insecure-skip-tls-verify: true` for the generated audit kubeconfig example | `false` |
| `podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `true` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `256Mi` |
| `resources.limits.cpu` | CPU limit | `1000m` |
| `resources.limits.memory` | Memory limit | `1Gi` |

See [`values.yaml`](values.yaml) for complete configuration options.

### Audit Webhook URL Contract

When `attribution.enabled=true`, `https://<service>:9444/audit-webhook/<audit-route>`
receives audit events from `kube-apiserver`. The route is
`KubeProvider.spec.attribution.auditRoute` and defaults to `<namespace>.<name>`.
Audit routes are **named**, such as `/audit-webhook/gitops-reverser.local` for the chart's provider
in a `gitops-reverser` release namespace. The bare `/audit-webhook` is rejected with **400** unless
`attribution.auditRouteAnnotationKey` is set, which turns it into the shared-stream endpoint that
resolves each event's source cluster from that annotation. The operator extracts a minimal attribution
fact from each (auditID, user, verb, resourceVersion, GVR, namespace, name, UID, status, timestamps)
and appends it to the per-type fact log the watch side follows
(written only when audit attribution is enabled). When a Redis endpoint is configured it also stores
each GitTarget's watch resume cursors, so reconnects resume a normal watch from the last processed
resourceVersion when the apiserver can still serve that history. Object state itself comes from
Kubernetes **watch**, not from audit; audit only names the commit author.

When `attribution.enabled=false`, the audit webhook is not rendered or served and the product runs
configured-author: mirrored-resource commits are authored by the configured committer. `queue.redis.addr`
is optional here: set it for warm-restart resume cursors, or leave it empty and watches cold-replay
from scratch on restart.

## Custom Resource Definitions (CRDs)

This chart automatically manages the following CRDs:

- **`kubeproviders.configbutler.ai`** - Source-cluster connections and namespace access policy
- **`gitproviders.configbutler.ai`** - Git repository connectivity and credentials
- **`gitbranches.configbutler.ai`** - One branch of a repository, and which `GitProvider` writes it
- **`gitreceivers.configbutler.ai`** - Push webhook endpoints that wake the matching `GitProvider`s
- **`gittargets.configbutler.ai`** - Branch/path and optional encryption configuration
- **`watchrules.configbutler.ai`** - Namespaced watch rules
- **`clusterwatchrules.configbutler.ai`** - Cluster-scoped watch rules
- **`commitrequests.configbutler.ai`** - One-shot "save now" signals that finalize the open commit window
- **`fluxclaims.configbutler.ai`** - Applier claims holding a Flux `Kustomization` for bi-directional GitOps
- **`argocdclaims.configbutler.ai`** - Applier claims holding an Argo CD `Application` for bi-directional GitOps

### CRD Lifecycle

| Operation | Behavior |
|-----------|----------|
| `helm install` | ✅ CRDs installed automatically |
| `helm upgrade` | ⚠️ CRDs NOT upgraded (Helm never upgrades `crds/`): apply the release's `crds.yaml` first, as below |
| `helm uninstall` | ⚠️ CRDs NOT deleted (prevents data loss) |

Before `helm upgrade`, apply the new release's CRDs, so new kinds and changed schemas are served
before the controller that needs them starts:

```bash
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/download/<tag>/crds.yaml
```

To manually remove CRDs after uninstallation:

```bash
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai commitrequests.configbutler.ai fluxclaims.configbutler.ai argocdclaims.configbutler.ai kubeproviders.configbutler.ai gitbranches.configbutler.ai gitreceivers.configbutler.ai
```

Delete the applier claims while the operator still runs, before this ([Uninstallation](#uninstallation)).

> ⚠️ **Warning**: Deleting CRDs will also delete all custom resources of those types!

## Verification

### Verify Installation

```bash
# Check pods (should see 1 replica)
kubectl get pods -n gitops-reverser

# Check CRDs
kubectl get crd | grep configbutler

```

For first-run GitOps Reverser usage, follow the quickstart linked above.

### View Logs

```bash
# All pods
kubectl logs -n gitops-reverser -l app.kubernetes.io/name=gitops-reverser -f

```

### Access Metrics

```bash
kubectl port-forward -n gitops-reverser svc/gitops-reverser 8080:8080
curl http://localhost:8080/metrics
# If metrics TLS is enabled:
# curl -k https://localhost:8080/metrics
```

## Upgrading

### Standard Upgrade

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser
```

### Upgrade with New Values

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --values new-values.yaml
```

### Migration from Previous Versions

If upgrading from earlier chart versions:

- Single-replica is the default during the current simplified topology phase
- Leader election remains enabled for safe future multi-pod evolution
- Health probe port changed to 8081
- Certificate secret names are auto-generated

## Uninstallation

**Hand Flux its `Kustomization`s back first, while the operator still runs.** A `FluxClaim` that
holds its `Kustomization` carries the finalizer `configbutler.ai/flux-claim`, and only the running
operator removes it, once it has handed the `Kustomization` back. One left after `helm uninstall`
keeps its namespace and the `fluxclaims` CRD from being deleted, and its `Kustomization` stays
suspended. Delete your `GitTarget`s, or the applier claims, before uninstalling, and wait until they are
gone:

```bash
kubectl delete fluxclaims,argocdclaims --all --all-namespaces --wait
```

An applier claim that never held its `Kustomization`, such as one no `GitTarget` names, carries no
finalizer. An `ArgoCDClaim` never carries one.

```bash
# Uninstall chart
helm uninstall gitops-reverser --namespace gitops-reverser

# Delete namespace (optional)
kubectl delete namespace gitops-reverser

# Delete CRDs (optional, but removes all custom resources)
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai commitrequests.configbutler.ai fluxclaims.configbutler.ai argocdclaims.configbutler.ai kubeproviders.configbutler.ai gitbranches.configbutler.ai gitreceivers.configbutler.ai
```

## Troubleshooting

### Audit Certificate Issues

Check certificate status:

```bash
kubectl get certificate -n gitops-reverser
kubectl describe certificate gitops-reverser-audit-server-cert -n gitops-reverser
```

If cert-manager is not working:

```bash
# Check cert-manager logs
kubectl logs -n cert-manager -l app=cert-manager -f

# Restart cert-manager
kubectl rollout restart deployment cert-manager -n cert-manager
```

### Pods Not Scheduling

If pods are pending due to anti-affinity rules:

```bash
# Check node count
kubectl get nodes

# If you have only 1 node, keep a single replica or disable affinity
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --set replicaCount=1 \
  --set affinity=null
```

### View Controller Events

```bash
kubectl get events -n gitops-reverser --sort-by='.lastTimestamp'
```

## Advanced Configuration

### Using External Certificate Provider

If not using cert-manager:

```yaml
certManager:
  enabled: false
```

Provide the audit TLS Secrets yourself. At minimum, the audit server certificate Secret must match
`servers.audit.tls.secretNameOverride` (or the default `<release>-audit-server-cert`), and the
audit root CA Secret must match `servers.audit.tls.rootCA.secretNameOverride` so the controller can
verify kube-apiserver client certificates.

Example server certificate Secret:

```bash
kubectl create secret tls gitops-reverser-audit-server-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n gitops-reverser
```

### Custom Resource Limits

For clusters with high resource usage:

```yaml
resources:
  requests:
    cpu: 200m
    memory: 256Mi
  limits:
    cpu: 2000m
    memory: 1Gi
```

## Release Strategy

New versions are automatically released via GitHub Actions:

1. Push to `main` branch applier claims release-please
2. Docker images built for `linux/amd64` and `linux/arm64`
3. Helm chart packaged and pushed to `ghcr.io`
4. Release notes include installation instructions

Check available versions:

```bash
# View latest release
helm show chart oci://ghcr.io/configbutler/charts/gitops-reverser

# List releases
curl -s https://api.github.com/repos/ConfigButler/gitops-reverser/releases | jq -r '.[].tag_name'
```

## Support & Contributing

- **Issues**: [GitHub Issues](https://github.com/ConfigButler/gitops-reverser/issues)
- **Documentation**: [Project README and quickstart](https://github.com/ConfigButler/gitops-reverser)
- **Security model**: [docs/security-model.md](https://github.com/ConfigButler/gitops-reverser/blob/main/docs/security-model.md)
- **Contributing**: See [CONTRIBUTING.md](https://github.com/ConfigButler/gitops-reverser/blob/main/CONTRIBUTING.md)

## License

This chart is licensed under the same license as the GitOps Reverser project.
