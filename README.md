# GitOps Reverser

**Automatically capture manual Kubernetes changes in Git**

GitOps Reverser is a Kubernetes operator that captures manual cluster changes and commits them to Git, creating a complete audit trail.

## What It Does

When you make manual changes to your Kubernetes cluster (hotfixes, emergency patches, configuration updates), GitOps Reverser:
- Captures changes in real-time via admission webhooks
- Sanitizes and formats them as clean YAML manifests
- Commits them to your Git repository with detailed metadata
- Handles conflicts intelligently

## Quick Start

### Prerequisites
- Kubernetes cluster (v1.11.3+)
- kubectl configured
- Git repository for storing changes
- cert-manager installed for webhook TLS

### Installation

```bash
# Install cert-manager (if not already installed)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

# Install GitOps Reverser
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/latest/download/install.yaml
```

### Setup Git Access

```bash
# 1. Generate SSH key
ssh-keygen -t ed25519 -C "gitops-reverser" -f ~/.ssh/gitops-reverser -N ""

# 2. Add public key to your Git repository as a deploy key with write access
cat ~/.ssh/gitops-reverser.pub

# 3. Create Kubernetes secret
ssh-keyscan github.com > /tmp/known_hosts
kubectl create secret generic git-credentials \
  --namespace gitops-reverser-system \
  --from-file=ssh-privatekey=$HOME/.ssh/gitops-reverser \
  --from-file=known_hosts=/tmp/known_hosts
```

> 📖 Complete GitHub setup guide: [`docs/GITHUB_SETUP_GUIDE.md`](docs/GITHUB_SETUP_GUIDE.md)

### Configure Repository

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: audit-repo
  namespace: gitops-reverser-system
spec:
  url: "git@github.com:yourorg/k8s-audit.git"
  branch: "main"
  secretRef:
    name: git-credentials
```

### Create Watch Rules

#### Example 1: Watch ConfigMaps and Secrets

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: config-audit
  namespace: production
spec:
  gitRepoConfigRef: audit-repo
  rules:
  - resources: [configmaps, secrets]
```

#### Example 2: Watch Only CREATE and UPDATE Operations

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: app-deployments
  namespace: production
spec:
  gitRepoConfigRef: audit-repo
  rules:
  - operations: [CREATE, UPDATE]  # Ignore DELETE
    apiGroups: [apps]
    apiVersions: [v1]
    resources: [deployments, statefulsets]
```

#### Example 3: Filter by Labels

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: production-only
  namespace: production
spec:
  gitRepoConfigRef: audit-repo
  objectSelector:
    matchExpressions:
    - key: environment
      operator: In
      values: [production]
    - key: gitops-reverser.io/ignore
      operator: DoesNotExist
  rules:
  - resources: ["*"]
```

#### Example 4: Watch Custom Resources

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: my-crds
  namespace: production
spec:
  gitRepoConfigRef: audit-repo
  rules:
  - apiGroups: [example.com]
    resources: [myapps, databases]
```

### Apply Configuration

```bash
kubectl apply -f gitrepoconfig.yaml
kubectl apply -f watchrule.yaml
```

## WatchRule Reference

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `gitRepoConfigRef` | string | Yes | Name of GitRepoConfig (must be in same namespace) |
| `objectSelector` | LabelSelector | No | Filter resources by labels |
| `rules[]` | []ResourceRule | Yes | Resources to watch (logical OR) |

### ResourceRule Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `resources` | []string | Yes | Resource types (e.g., `pods`, `deployments`) |
| `operations` | []OperationType | No | Operations to watch: `CREATE`, `UPDATE`, `DELETE`, `*` |
| `apiGroups` | []string | No | API groups (`""` = core, `apps`, etc.) |
| `apiVersions` | []string | No | API versions (`v1`, `v1beta1`, etc.) |

### Wildcards

- `"*"` - Matches all resources
- `"pods"` - Exact match (case-insensitive)
- `"pods/*"` - All pod subresources (pods/log, pods/status)
- `"pods/log"` - Specific subresource

> ⚠️ Prefix/suffix wildcards like `pod*` or `*.example.com` are NOT supported

## Repository Structure

Changes are organized in your Git repository as:

```
<repo-root>/
├── <apiGroup>/<version>/<resources>/<namespace>/<name>.yaml
│
├── v1/configmaps/production/app-config.yaml
├── v1/secrets/production/db-credentials.yaml
├── apps/v1/deployments/production/web-app.yaml
└── example.com/v1/myapps/production/my-custom-resource.yaml
```

Each commit includes detailed metadata:
```
[CREATE] ConfigMap/app-config in ns/production by admin@company.com

- Resource: ConfigMap/app-config
- Namespace: production
- Operation: CREATE
- User: admin@company.com
- Timestamp: 2025-01-31T18:30:00Z
```

## Security Model

### Namespace Isolation

WatchRule is namespace-scoped and can **only watch resources in its own namespace**:

```yaml
# This WatchRule in namespace 'team-a'...
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: my-rule
  namespace: team-a  # ← Can ONLY watch resources in team-a
spec:
  gitRepoConfigRef: my-repo
  rules:
  - resources: [pods, configmaps]
```

**Benefits:**
- Multi-tenancy: Teams can only watch their own resources
- Least privilege: No cross-namespace visibility
- RBAC integration: Kubernetes RBAC controls WatchRule creation per namespace

### Cluster-Scoped Resources

Cluster-scoped resources (Nodes, ClusterRoles, Namespaces, etc.) require ClusterWatchRule:

> 🚧 **ClusterWatchRule is not yet implemented**. Currently, only namespace-scoped resources are supported.

## Important: Avoiding Infinite Loops

**Do NOT run traditional GitOps tools (Argo CD/Flux) and GitOps Reverser on the same resources in fully automated mode.**

This creates an infinite loop:
1. GitOps Reverser sees manual change → pushes to Git
2. Argo CD sees Git changed → "corrects" cluster
3. GitOps Reverser sees "correction" → pushes to Git
4. Loop repeats endlessly

### Recommended Usage Patterns

1. **Audit-Only**: Capture changes without automated enforcement
2. **Human-in-the-Loop**: Allow hotfixes, capture in Git, humans review
3. **Drift Detection**: Use commits as input for alerting systems
4. **Hybrid GitOps**: Run traditional GitOps on infrastructure, GitOps Reverser on configurations

## Monitoring

GitOps Reverser exports OpenTelemetry metrics:

- `gitops_reverser_events_received_total` - Webhook events received
- `gitops_reverser_events_processed_total` - Events successfully processed
- `gitops_reverser_git_operations_total` - Git operations count
- `gitops_reverser_git_push_duration_seconds` - Git push duration
- `gitops_reverser_git_commit_queue_size` - Event queue size

## Development

```bash
# Run unit tests
make test

# Run linter
make lint

# Run e2e tests
make test-e2e

# Build locally
make build
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`TESTING.md`](TESTING.md) for detailed development information.

## Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Kubernetes    │    │  GitOps Reverser │    │   Git Repository│
│     Cluster     │───▶│    Operator      │───▶│                 │
│                 │    │                  │    │  <group>/<ver>/ │
│ Manual Changes  │    │ • Webhooks       │    │  ├─ pods/       │
│ Admin Actions   │    │ • Sanitization   │    │  ├─ configmaps/ │
│ Hotfixes        │    │ • Conflict Mgmt  │    │  └─ ...         │
└─────────────────┘    └──────────────────┘    └─────────────────┘
```

### Key Components

- **Admission Webhooks**: Capture changes in real-time
- **Event Queue**: Buffer and batch changes for efficient processing
- **Git Worker**: Handle repository operations with conflict resolution
- **Sanitization Engine**: Clean manifests (remove status, server-managed fields)
- **Rule Engine**: Configure which resources to track with fine-grained filters

## Troubleshooting

### Git Authentication Failures

```bash
# Check controller logs
kubectl logs -n gitops-reverser-system deployment/gitops-reverser-controller

# Verify secret exists
kubectl get secret git-credentials -n gitops-reverser-system
```

### Webhook Not Triggering

```bash
# Check webhook registration
kubectl get validatingwebhookconfigurations

# Check webhook service endpoints
kubectl get endpoints gitops-reverser-webhook-service -n gitops-reverser-system
```

### No Events in Queue

```bash
# Check if WatchRule is ready
kubectl get watchrule -A

# View WatchRule status
kubectl describe watchrule <name> -n <namespace>
```

## Contributing

We welcome contributions! Key areas:

- ClusterWatchRule implementation (for cluster-scoped resources)
- Enhanced filtering and transformation rules
- Multi-repository support
- Integration with GitOps tools

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for guidelines.

## License

Apache License 2.0 - See [LICENSE](LICENSE) for details.

---

**Need Help?**

- 📖 [Documentation](https://github.com/ConfigButler/gitops-reverser/wiki)
- 🐛 [Report Issues](https://github.com/ConfigButler/gitops-reverser/issues)
- 💬 [Discussions](https://github.com/ConfigButler/gitops-reverser/discussions)
