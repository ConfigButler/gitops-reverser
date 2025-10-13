# GitOps Reverser

GitOps Reverser is a Kubernetes operator that captures cluster changes (through a ValidatingWebhookConfiguration). The operator commits them to Git as yaml files, creating a file based reprentation of your current desired state. Every change becomes an annotated commit, giving you an audit trail.

# Why?

We love GitOps. It gives us a single source of truth, an audit trail, and safe, automated deployments. We also love the Kubernetes API. It's a universal, battle-tested control plane that gives us a powerful, declarative interface for managing any kind resources.

But today, we are forced to choose between them:

* The Pure GitOps Flow: Developers make changes exclusively in Git. This is safe but can be slow and cumbersome for simple configuration updates, often requiring developers to be YAML and Git experts.

* The API-First Flow: Developers or UIs make changes directly to the cluster. This is fast and interactive but breaks the "Git as the source of truth" principle, creating a "Wild West" of untracked changes.

How can we get the interactive, immediate feedback of an API without sacrificing the safety and transparency of a Git-based workflow?

GitOps-Reverser is the bridge. It's a lightweight Kubernetes admission webhook that implements a powerful "Reverse GitOps" pattern. It lets you use the Kubernetes API as the user-friendly "frontend" for a rock-solid GitOps backend.

# How?

GitOps-Reverser intercepts them and translates them into a safe, auditable Git workflow.

How It Works: The "Sandbox-First" Workflow
Propose a Change: A developer (or a UI) makes a configuration change using the standard Kubernetes API (kubectl edit, or a PATCH request).

Intercept & Fork: GitOps-Reverser intercepts the API call. Instead of applying it, it automatically creates a new Git branch and opens a Pull Request with the proposed change.

Test in Isolation: The new PR triggers a CI pipeline (using tools like Tekton) that spins up an ephemeral "sandbox" namespace. The proposed configuration is deployed and tested in complete isolation.

Review & Merge: The team reviews the PR, which now includes the successful test results. Once approved, the change is merged into the main branch.

Deploy with Confidence: A standard GitOps tool (like Flux or Argo CD) detects the merge and safely reconciles the change to your production environment.

The Benefits
The Best of Both Worlds: Get the interactive, user-friendly experience of an API and the safety, audit trail, and peer-review process of Git.

Enhanced Developer Experience: Developers can make changes in safe, temporary sandboxes without needing deep Git expertise for every small update.

Unlock Automation and AI: By creating a standard, API-driven contract for configuration, we pave the way for a future where AI agents and interoperable tools can safely and declaratively manage our systems.

In short, GitOps-Reverser lets you stop choosing between speed and safety, giving you a truly cloud-native way to manage your application's configuration.

## What It Does

When you make manual changes to your Kubernetes cluster (hotfixes, emergency patches, configuration updates), GitOps Reverser:
- Captures changes in real-time via admission webhooks
- Sanitizes and formats them as clean YAML manifests
- Commits them to your Git repository with detailed metadata
- Handles conflicts intelligently

## 🚨 Early Stage Software 🚨


> **Heads up!** This operator is currently in a very early stage of development. It is intended for evaluation and testing purposes in non-critical environments.
>
> Please be aware of the following:
>
> - The Custom Resource APIs (`GitRepoConfig`, `WatchRule`, etc.) are **not stable** and may have breaking changes in future releases.
> - The software has not been thoroughly tested and likely contains bugs.
> - It is **NOT yet recommended for production use**.
>
> I enthusiastically welcome feedback, bug reports, and contributions! Please feel free to open an issue to share your thoughts.

## Quick Start

### Prerequisites
- Kubernetes cluster
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

Cluster-scoped resources (Nodes, ClusterRoles, Namespaces, etc.) require ClusterWatchRule (docs to be added)

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

## Other tools

| **Tool** | **How it Works** | **Key Difference from GitOps Reverser** | 
|---|---|---|
| [RichardoC/kube-audit-rest](https://github.com/RichardoC/kube-audit-rest) | An admission webhook that receives audit events and exposes them over a REST API. | **Action vs. Transport:** `kube-audit-rest` is a transport layer. GitOps Reverser is an *action* layer that consumes the event and commits it to Git. | 
| https://github.com/robusta-dev/robusta | A broad observability and automation platform. | **Focused Tool vs. Broad Platform:** Robusta is a large platform. GitOps Reverser is a small, single-purpose utility focused on simplicity and low overhead. | 
| [bpineau/katafygio](https://github.com/bpineau/katafygio) | Periodically scans the cluster and dumps all resources to a Git repository. | **Event-Driven vs. Snapshot:** Katafygio is a backup tool. GitOps Reverser is event-driven, providing a real-time audit trail. | 

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for guidelines.

## License

Apache License 2.0

---

**Need Help?**

- 🐛 [Report Issues](https://github.com/ConfigButler/gitops-reverser/issues)
