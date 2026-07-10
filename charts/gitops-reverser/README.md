# GitOps Reverser Helm Chart

GitOps Reverser enables synchronization from Kubernetes to one or more Git repositories.

## Quick Start

The official end-to-end quickstart lives in the repository root README:

- [`README.md`](../../README.md)

Use that guide for cert-manager, Valkey, Helm install, Git credentials, starter
`GitProvider`/`GitTarget`/`WatchRule` resources, and first-commit verification. Kube-apiserver audit
delivery is optional and can be added later for named commit authors.

This chart README stays focused on chart-specific installation, configuration, and operations.

For the chart's optional starter `quickstart` block, see [`docs/configuration.md`](../../docs/configuration.md).

## Features

- ✅ **Git synchronization**: Push Kubernetes changes back to Git repositories
- ✅ **Single-pod stability**: 1 replica by default while multi-pod support is in progress
- ✅ **Automatic CRD installation**: GitProvider, GitTarget, WatchRule, ClusterWatchRule, and CommitRequest CRDs installed automatically
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

```
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

Hardened settings for a controlled pilot or environment-specific production review:

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
| `replicaCount` | Number of controller replicas (can't be higher than 1 for now, sorry) | `1` |
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
| `controllerManager.additionalSensitiveResources` | Extra Secret-shaped resource types encrypted as `resource` or `group/resource` | `[]` |
| `auditService.type` | Service type for the dedicated audit Service | `NodePort` |
| `auditService.nodePort` | Fixed NodePort for the audit Service when `auditService.type=NodePort` | `30444` |
| `auditService.clusterIP` | Optional fixed ClusterIP for the dedicated audit Service | `""` |
| `quickstart.enabled` | Create starter `GitProvider`, `GitTarget`, and `WatchRule` resources | `false` |
| `quickstart.namespace` | Namespace for the starter quickstart resources (create it, and the git-creds Secret, before install) | `gitops-reverser-quickstart-demo` |
| `quickstart.createNamespace` | Let the chart create `quickstart.namespace` (Helm then owns it) | `false` |
| `quickstart.gitProvider.url` | Repository URL used by the starter `GitProvider` | `""` |
| `quickstart.gitProvider.secretRef.name` | Existing Secret name used by the starter `GitProvider` | `git-creds` |
| `quickstart.gitTarget.path` | Repository path used by the starter `GitTarget`; set `.` only to deliberately target the repo root | `live-cluster` |
| `quickstart.watchRule.rules` | Rules used by the starter `WatchRule` | `configmaps create/update/delete` |
| `queue.redis.addr` | Redis/Valkey endpoint (`host:port`). Optional but advised: empty runs `configured-author` with cold-replay on restart. Set it for warm-restart cursors and for the admission webhook to actually record CommitRequest authors (admission runs as a no-op without it); **required** only when `attribution.enabled=true` | `""` |
| `queue.redis.auth.existingSecret` | Name of a pre-created Secret holding the Redis password (only used when `queue.redis.addr` is set) | `""` |
| `queue.redis.auth.existingSecretKey` | Key within the Secret that holds the password | `password` |
| `queue.redis.auth.username` | Optional Redis ACL username | `""` |
| `queue.redis.db` | Redis logical database index. Redis offers 16; use `queue.redis.keyPrefix` to go past that | `0` |
| `queue.redis.keyPrefix` | Root of every key this release writes (watch cursors, attribution facts, command author records). Give each reverser its own prefix to share one Redis/Valkey between more reversers than `db` can separate. Changing it orphans the previous prefix's keys: cursors cold-replay once, which is safe. Allowed: `[A-Za-z0-9]`, `-`, `_`, `.`, `:` | `gitops-reverser` |
| `queue.redis.tls.enabled` | Enable TLS for Redis connection | `false` |
| `attribution.enabled` | Run audit ingress and name mirrored-resource commit authors from matching kube-apiserver audit facts | `false` |
| `attribution.ttl` | How long an attribution fact is retained waiting for the matching watch event to join it | `10m` |
| `attribution.grace` | Bounded per-event wait for a matching audit fact before a watch event ships as the committer | `3s` |
| `servers.admission.enabled` | Install the validate-operator-types admission webhook that captures CommitRequest authors (a form of author attribution). Enabled by default; a no-op until `queue.redis.addr` is set | `true` |
| `rbac.create` | Create the manager ClusterRole and its binding | `true` |
| `rbac.watchTypes.mode` | Which types a `WatchRule` may read. `any` grants cluster-wide read on everything — convenient, but the reverser can then read every Secret in the cluster. `selected` grants read on `rbac.watchTypes.selected` only, so the reverser cannot list or watch Secrets (it keeps `get` on named Secrets it is pointed at). See [`docs/rbac.md`](../../docs/rbac.md) | `any` |
| `rbac.watchTypes.selected` | Types to grant when `mode: selected`, as `{apiGroups, resources}` entries (verbs are always `get,list,watch`). Required and non-empty in that mode; `namespaces`, `customresourcedefinitions` and `apiservices` come from the manager role and must not be restated | `[]` |
| `servers.metrics.bindAddress` | Metrics listener bind address | `:8080` |
| `servers.metrics.tls.enabled` | Serve metrics with TLS | `false` |
| `servers.metrics.tls.certPath` | Metrics TLS certificate mount path | `/tmp/k8s-metrics-server/metrics-server-certs` |
| `servers.metrics.tls.secretNameOverride` | Override Secret name for metrics TLS cert/key | `<release>-metrics-server-cert` |
| `service.clusterIP` | Optional fixed ClusterIP for single controller Service | `""` |
| `service.ports.audit` | Service port for audit ingress when `attribution.enabled=true` | `9444` |
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

When `attribution.enabled=true`, `https://<service>:9444/audit-webhook` receives audit events from
kube-apiserver. The operator extracts a minimal attribution fact from each (auditID, user, verb,
resourceVersion, GVR, namespace, name, UID, status, timestamps) into the Redis attribution index
(populated only when audit attribution is enabled). When a Redis endpoint is configured it also stores
each GitTarget's watch resume cursors, so reconnects resume a normal watch from the last processed
resourceVersion when the apiserver can still serve that history. Object state itself comes from
Kubernetes **watch**, not from audit; audit only names the commit author.

When `attribution.enabled=false`, the audit webhook is not rendered or served and the product runs
configured-author: mirrored-resource commits are authored by the configured committer. `queue.redis.addr`
is optional here: set it for warm-restart resume cursors, or leave it empty and watches cold-replay
from scratch on restart.

Cluster ID path segments are rejected.

## Custom Resource Definitions (CRDs)

This chart automatically manages the following CRDs:

- **`gitproviders.configbutler.ai`** - Git repository connectivity and credentials
- **`gittargets.configbutler.ai`** - Branch/path and optional encryption configuration
- **`watchrules.configbutler.ai`** - Namespaced watch rules
- **`clusterwatchrules.configbutler.ai`** - Cluster-scoped watch rules
- **`commitrequests.configbutler.ai`** - One-shot "save now" signals that finalize the open commit window

### CRD Lifecycle

| Operation | Behavior |
|-----------|----------|
| `helm install` | ✅ CRDs installed automatically |
| `helm upgrade` | ✅ CRDs upgraded automatically |
| `helm uninstall` | ⚠️ CRDs NOT deleted (prevents data loss) |

To manually remove CRDs after uninstallation:

```bash
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai commitrequests.configbutler.ai
```

> ⚠️ **Warning**: Deleting CRDs will also delete all custom resources of those types!

## Verification

### Verify Installation

```bash
# Check pods (should see 1 replica)
kubectl get pods -n gitops-reverser

# Check CRDs
kubectl get crd | grep configbutler

```

For first-run GitOps Reverser usage, follow the root quickstart instead of duplicating those steps here.

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

```bash
# Uninstall chart
helm uninstall gitops-reverser --namespace gitops-reverser

# Delete namespace (optional)
kubectl delete namespace gitops-reverser

# Delete CRDs (optional, but removes all custom resources)
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai commitrequests.configbutler.ai
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

1. Push to `main` branch triggers release-please
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
