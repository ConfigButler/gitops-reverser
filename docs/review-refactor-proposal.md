# GitOps Reverser Architecture

This document outlines the architecture for a Kubernetes operator that captures resource configurations and stores them as sanitized audit logs in Git.

## Core Concepts

### Purpose

Intercept Kubernetes resource changes using admission webhooks and store them as clean, declarative YAML in Git repositories.

### Key Requirements

1. **Generic**: Handle any Kubernetes resource (core types and CRDs) without prior knowledge
2. **Sanitized**: Store declarative intent, not operational state (no status, server-managed fields)
3. **Auditable**: Maintain complete history with user attribution
4. **Efficient**: Handle race conditions and conflicts intelligently

## Architecture Components

### 1. Resource Identification

Git file paths mirror Kubernetes API structure:

**Format**: `<group>/<version>/<resources>/<namespace>/<name>.yaml`

**Examples:**
- Deployment: `apps/v1/deployments/production/my-app.yaml`
- Pod (core): `v1/pods/default/nginx-pod.yaml`
- Custom Resource: `example.com/v1/myapps/production/my-app.yaml`

**Source**: Admission request provides all components:
- Group: `req.Resource.Group`
- Version: `req.Resource.Version`
- Resource: `req.Resource.Resource` (plural form)
- Namespace: `req.Namespace`
- Name: `req.Name`

### 2. Manifest Sanitization

**Goal**: Store "what" (desired state), discard "how" (operational state)

**Removed Fields:**
- `status` - Operational state
- `metadata.uid` - Server-generated
- `metadata.resourceVersion` - Server-generated
- `metadata.generation` - Server-generated
- `metadata.creationTimestamp` - Server-generated
- `metadata.managedFields` - Server-generated
- `kubectl.kubernetes.io/last-applied-configuration` - Annotation
- Other server-managed annotations

**Implementation**: See [`internal/sanitize/`](../internal/sanitize/) package

### 3. Watch Rules

**WatchRule**: Namespace-scoped CRD that defines which resources to capture

**Key Features:**
- **Operation filtering**: Watch only CREATE, UPDATE, or DELETE
- **API group filtering**: Target specific groups (core, apps, custom)
- **Version filtering**: Watch specific API versions
- **Label filtering**: Include/exclude based on labels
- **Namespace isolation**: Can only watch resources in its own namespace

**Example:**
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: production-configs
  namespace: production
spec:
  gitRepoConfigRef: audit-repo
  objectSelector:
    matchExpressions:
    - key: environment
      operator: In
      values: [production]
  rules:
  - operations: [CREATE, UPDATE]
    apiGroups: [""]
    resources: [configmaps, secrets]
```

**Security Model:**
- WatchRule watches **only namespace-scoped resources** in **its own namespace**
- Enforces multi-tenancy (team-a cannot watch team-b resources)
- RBAC controls WatchRule creation per namespace

**Future**: ClusterWatchRule for cluster-scoped resources (Nodes, ClusterRoles)

### 4. Git Operations

**Conflict Resolution**: "Last Writer Wins"

- Cluster change newer than Git → Cluster wins (push succeeds)
- Git change newer than cluster → Git wins (push backs off)

**Race Condition Handling:**
- Retry with exponential backoff
- Re-evaluate and re-generate on conflicts
- Eventual consistency guaranteed

**Implementation**: See [`internal/git/`](../internal/git/) package

### 5. Event Processing

**Flow:**
```
Webhook → Event Queue → Git Worker → Git Repository
```

1. **Webhook** captures admission requests
2. **Event Queue** buffers and batches changes
3. **Git Worker** processes queue, handles conflicts
4. **Git Repository** stores final YAML

## Component Details

### Admission Webhook

**Path**: `/validate-v1-event`
**Type**: ValidatingWebhook (non-mutating)
**Failure Policy**: Ignore (don't block cluster operations)

**Processing:**
1. Decode admission request
2. Extract resource metadata
3. Match against WatchRules
4. Sanitize object
5. Enqueue event

**Implementation**: [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)

### Rule Store

In-memory cache of compiled WatchRules for efficient matching.

**Features:**
- Thread-safe concurrent access
- O(n) matching complexity (where n = number of rules)
- Namespace-aware filtering
- Operation/group/version filtering

**Implementation**: [`internal/rulestore/store.go`](../internal/rulestore/store.go)

### Controllers

**GitRepoConfigReconciler**: Validates Git repository access
**WatchRuleReconciler**: Validates WatchRule configuration and GitRepoConfig references

**Implementation**: [`internal/controller/`](../internal/controller/)

## YAML Formatting

**Two-pass approach** for clean, ordered YAML:

1. **Decode** to `map[string]interface{}` for flexibility
2. **Extract** typed metadata into `PartialObjectMeta`
3. **Clean** map by removing unwanted fields
4. **Re-assemble** into ordered struct:
   ```go
   type FinalGitOpsObject struct {
       APIVersion string
       Kind       string
       Metadata   PartialObjectMeta
       Payload    map[string]interface{} `json:",inline"`
   }
   ```
5. **Marshal** to YAML

**Result**: Clean YAML with conventional field ordering (apiVersion, kind, metadata, spec/data)

**Implementation**: [`internal/sanitize/marshal.go`](../internal/sanitize/marshal.go)

## Security Considerations

### Git Credentials

Store SSH keys in Kubernetes Secrets:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: git-credentials
type: Opaque
data:
  ssh-privatekey: <base64-encoded-key>
  known_hosts: <base64-encoded-hosts>
```

### RBAC

Controller requires:
- Cluster-wide read access (for webhooks)
- Webhook configuration permissions
- Namespace-scoped access to GitRepoConfig/WatchRule CRDs

### TLS Certificates

Webhook communication requires HTTPS. Certificates managed by cert-manager automatically.

## Testing

- **Unit Tests**: Core logic validation (>90% coverage required)
- **Integration Tests**: Git operations, race conditions
- **E2E Tests**: Full workflow with real Git repository (Gitea)

See [`TESTING.md`](../TESTING.md) for details.

## References

- WatchRule API: [`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go)
- Event Handler: [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)
- Sanitization: [`internal/sanitize/`](../internal/sanitize/)
- Git Worker: [`internal/git/worker.go`](../internal/git/worker.go)
