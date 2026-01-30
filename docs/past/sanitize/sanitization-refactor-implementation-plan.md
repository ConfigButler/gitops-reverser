# Sanitization & YAML Ordering Implementation Plan

## Executive Summary

This document provides a detailed implementation plan to enhance the GitOps Reverser's resource sanitization and YAML generation capabilities.

### Why This Refactor?

**Primary Goals**:
1. **Guarantee conventional YAML field ordering** - GitOps workflows require clean, consistently-formatted manifests where fields appear in standard Kubernetes order (`apiVersion`, `kind`, `metadata`, then payload)
2. **API-aligned Git paths** - Match Kubernetes REST API structure (`{group}/{version}/{resource}/{namespace}/{name}`) for intuitive navigation
3. **Type safety** - Replace raw maps with proper types for compile-time verification
4. **Comprehensive sanitization** - Remove all server-generated and cluster-specific fields, including nested fields like `spec.clusterIP`
5. **Code maintainability** - Single source of truth for resource identification and path generation

**Current Limitations**:
- [`internal/sanitize/sanitize.go`](../internal/sanitize/sanitize.go): Cannot guarantee YAML field ordering when marshaling `map[string]interface{}`
- [`internal/git/git.go:660-668`](../internal/git/git.go#L660-668): Git paths don't include group/version, use `namespaces/{ns}/{resource}/{name}` format
- [`internal/eventqueue/queue.go`](../internal/eventqueue/queue.go#L12-23): Scattered resource identification across multiple fields
- Limited nested field sanitization (e.g., `spec.clusterIP` not addressed)

**Expected Outcomes**:
- ✅ Guaranteed field order in committed YAML files
- ✅ API-aligned Git repository structure
- ✅ Comprehensive field removal matching industry best practices
- ✅ Smaller, cleaner codebase with better type safety
- ✅ Easier testing and maintenance

**Timeline**: 12-17 hours total across 4 phases

---

## Context & Current State

### Current Implementation

**Existing Sanitization** ([`internal/sanitize/sanitize.go`](../internal/sanitize/sanitize.go)):
- ✅ Uses `unstructured.Unstructured` (Kubernetes standard type)
- ✅ Removes server-generated metadata fields
- ✅ Preserves core desired state fields (`spec`, `data`, etc.)
- ⚠️ **Cannot guarantee YAML field ordering** - Go maps are unordered
- ⚠️ Limited nested field sanitization
- ⚠️ Uses raw `map[string]interface{}` for metadata

**Current Git Path Generation** ([`internal/git/git.go:660-668`](../internal/git/git.go#L660-668)):
```go
// Current format: namespaces/{namespace}/{resource}/{name}.yaml
// Missing: group and version information
func GetFilePath(obj *unstructured.Unstructured, resourcePlural string) string {
    if obj.GetNamespace() != "" {
        return fmt.Sprintf("namespaces/%s/%s/%s.yaml", obj.GetNamespace(), resourcePlural, obj.GetName())
    }
    return fmt.Sprintf("cluster-scoped/%s/%s.yaml", resourcePlural, obj.GetName())
}
```

**Current Event Structure** ([`internal/eventqueue/queue.go`](../internal/eventqueue/queue.go#L12-23)):
```go
type Event struct {
    Object                 *unstructured.Unstructured
    Request                admission.Request  // Entire request object
    ResourcePlural         string             // Duplicated from request
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
}
```

### Chosen Approach: Adapt Kyverno + Structured Types

After researching industry solutions (Flux, Kyverno, Argo CD, OPA Gatekeeper), we've chosen:

**Option 2: Adapt Kyverno's approach** (lightweight) + Custom ordering layer

**Rationale**:
- ✅ Kyverno has proven, battle-tested field removal logic
- ✅ Apache 2.0 license (compatible with our project)
- ✅ Uses `unstructured.Unstructured` (same as us)
- ✅ Lightweight - no heavy dependencies
- ✅ Can adapt their metadata cleaning function directly
- ✅ We maintain control over YAML field ordering

**What we'll adapt from Kyverno**:
- Their `excludeAutoGenMetadata()` function removes exactly what we need
- Metadata cleaning patterns for common operational annotations
- Resource-specific sanitization approaches

**What we'll add**:
- Custom marshaling layer to guarantee field order
- Type-safe structures (`ResourceIdentifier`, `PartialObjectMeta`)
- API-aligned path generation

---

## Explicit Field Removal List

This comprehensive list defines **exactly** which fields must be removed during sanitization. The goal is to store only declarative intent, removing all server-generated, runtime, and cluster-specific state.

### Metadata Fields to Remove (All Resources)

These fields are **always removed** regardless of resource type:

```yaml
metadata:
  uid: "..."                    # ❌ Server-generated unique identifier
  resourceVersion: "..."        # ❌ Server-managed version number
  generation: ...               # ❌ Server-managed generation counter
  creationTimestamp: "..."      # ❌ Server-set creation time
  deletionTimestamp: "..."      # ❌ Deletion in progress timestamp
  deletionGracePeriodSeconds: ...  # ❌ Deletion grace period
  selfLink: "..."              # ❌ Deprecated, but may exist
  managedFields: [...]         # ❌ Server-Side Apply tracking
  ownerReferences: [...]       # ❌ Server-managed ownership (can be user-set, but often auto-generated)
```

**Note on ownerReferences**: While sometimes user-specified, they're often auto-generated by controllers. We remove them by default to avoid circular dependencies in GitOps repos.

### Annotations to Remove (All Resources)

```yaml
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "..."  # ❌ kubectl tracking
    control-plane.alpha.kubernetes.io/leader: "..."          # ❌ Leader election
    deployment.kubernetes.io/revision: "..."                 # ❌ Deployment tracking
    autoscaling.alpha.kubernetes.io/conditions: "..."        # ❌ HPA internal state
    autoscaling.alpha.kubernetes.io/current-metrics: "..."   # ❌ HPA metrics
```

### Status Fields (All Resources)

The entire `status` object is **always removed** for all resources:

```yaml
status: {...}  # ❌ Entire object removed - represents observed state, not desired
```

### Service-Specific Fields

```yaml
spec:
  clusterIP: "10.96.0.1"           # ❌ Cluster-assigned IP address
  clusterIPs: ["10.96.0.1"]        # ❌ Cluster-assigned IPs (dual-stack)
  healthCheckNodePort: 30000       # ❌ Auto-assigned port for LoadBalancer
  ipFamilies: ["IPv4"]             # ❌ Auto-determined IP families
  ipFamilyPolicy: "SingleStack"    # ❌ Auto-determined policy
  internalTrafficPolicy: "Cluster" # ❌ May be auto-set by admission
```

**Rationale**: These fields are assigned by the cluster based on available IP pools and cannot be portably declared across clusters.

### Pod-Specific Fields

```yaml
spec:
  nodeName: "worker-node-1"              # ❌ Scheduler-assigned node
  serviceAccountName: "default"          # ❌ If auto-injected (keep if explicitly set)
  
status:
  hostIP: "192.168.1.10"                 # ❌ Node's IP address
  podIP: "10.244.1.5"                    # ❌ Pod's assigned IP
  podIPs: [{ip: "10.244.1.5"}]          # ❌ Pod's IPs (dual-stack)
  phase: "Running"                       # ❌ Current phase
  conditions: [...]                      # ❌ Current conditions
  containerStatuses: [...]               # ❌ Container runtime state
```

**Rationale**: Runtime assignments and state that vary between clusters and over time.

### PersistentVolumeClaim-Specific Fields

```yaml
spec:
  volumeName: "pvc-abc123"               # ❌ Bound PV name (dynamic provisioning)
  volumeMode: "Filesystem"               # ❌ May be auto-set by storage class
  
status:
  phase: "Bound"                         # ❌ Current binding state
  accessModes: [...]                     # ❌ Actual access modes from PV
  capacity: {...}                        # ❌ Actual capacity from PV
```

### Node-Specific Fields

```yaml
status:
  addresses:                              # ❌ Node network addresses
  - type: "InternalIP"
    address: "192.168.1.10"
  capacity: {...}                         # ❌ Node capacity
  allocatable: {...}                      # ❌ Allocatable resources
  conditions: [...]                       # ❌ Node health state
  nodeInfo: {...}                         # ❌ Runtime information
  images: [...]                           # ❌ Cached images
```

### Deployment/StatefulSet/DaemonSet-Specific

```yaml
status:
  replicas: 3                            # ❌ Current replica count
  readyReplicas: 3                       # ❌ Ready replicas
  availableReplicas: 3                   # ❌ Available replicas
  observedGeneration: 5                  # ❌ Last observed generation
  conditions: [...]                      # ❌ Deployment conditions
  collisionCount: 0                      # ❌ Collision tracking
```

### Job/CronJob-Specific

```yaml
status:
  active: 1                              # ❌ Active pods count
  succeeded: 5                           # ❌ Succeeded count
  failed: 0                              # ❌ Failed count
  startTime: "..."                       # ❌ Job start time
  completionTime: "..."                  # ❌ Job completion
  conditions: [...]                      # ❌ Job conditions
```

### Custom Resource Definitions (CRDs)

For CRDs, apply generic rules:
- ✅ **KEEP**: `spec.*` (entire spec is user-defined intent)
- ❌ **REMOVE**: `status.*` (entire status is observed state)
- ❌ **REMOVE**: Metadata fields (as listed above)

### ConfigMap & Secret Considerations

**Important**: ConfigMaps and Secrets use different fields for different data types. **Both must be preserved** as they serve distinct purposes and can coexist in a single resource.

| Field | Purpose | Content Type | Example Use Case |
|-------|---------|--------------|------------------|
| `data` | Text content | UTF-8 strings | Config files, JSON, YAML |
| `binaryData` | Binary content | Base64-encoded binary | Images, certificates, compiled files |

```yaml
# ConfigMaps: Keep BOTH data fields
data:
  config.json: |                         # ✅ KEEP - text data
    {"key": "value"}
  app.yaml: |                            # ✅ KEEP - text configuration
    setting: true

binaryData:
  logo.png: "iVBORw0KG..."               # ✅ KEEP - binary data (base64)
  cert.pem: "LS0tLS1CRU..."              # ✅ KEEP - binary certificate

# Secrets: Similar structure
data:
  username: "YWRtaW4="                   # ✅ KEEP - base64-encoded value
  password: "cGFzc3dvcmQ="               # ✅ KEEP - base64-encoded value

# Exception: Do NOT store auto-generated service account tokens
# Check for: kubernetes.io/service-account-token type
```

**Why preserve both**:
- User's declarative intent includes both text and binary data
- They cannot replace each other (different content types)
- Removing either would lose user data
- Both are part of desired state, not server-generated

**Note on Secrets `stringData`**:
- `stringData` is a write-only convenience field that converts to `data`
- Not stored in etcd, so won't appear in admission webhook objects
- No need to handle separately

---

## Implementation Plan

### Phase 1: Type System Setup (5-7 hours)

**Goal**: Establish type-safe foundations for resource identification and metadata handling.

#### Step 1.1: Create ResourceIdentifier Type (2 hours)

**Location**: Create new file `internal/types/identifier.go`

**Implementation**:
```go
package types

import (
    "fmt"
    "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ResourceIdentifier encapsulates all information needed to uniquely identify
// a Kubernetes resource and generate its Git storage path following the
// Kubernetes REST API structure: {group}/{version}/{resource}/{namespace}/{name}
type ResourceIdentifier struct {
    Group     string // e.g., "apps", "" for core resources
    Version   string // e.g., "v1"
    Resource  string // Plural form, e.g., "deployments", "pods"
    Namespace string // Empty string for cluster-scoped resources
    Name      string // Resource name
}

// FromAdmissionRequest extracts a ResourceIdentifier from an admission.Request
func FromAdmissionRequest(req admission.Request) ResourceIdentifier {
    return ResourceIdentifier{
        Group:     req.Resource.Group,
        Version:   req.Resource.Version,
        Resource:  req.Resource.Resource,
        Namespace: req.Namespace,
        Name:      req.Name,
    }
}

// ToGitPath generates the Git repository file path following Kubernetes API structure
func (r ResourceIdentifier) ToGitPath() string {
    var basePath string
    
    if r.Group == "" {
        // Core resources (no group)
        basePath = r.Version
    } else {
        basePath = fmt.Sprintf("%s/%s", r.Group, r.Version)
    }
    
    if r.Namespace != "" {
        // Namespaced resource
        return fmt.Sprintf("%s/%s/%s/%s.yaml", basePath, r.Resource, r.Namespace, r.Name)
    }
    
    // Cluster-scoped resource
    return fmt.Sprintf("%s/%s/%s.yaml", basePath, r.Resource, r.Name)
}

// IsClusterScoped returns true if the resource is cluster-scoped
func (r ResourceIdentifier) IsClusterScoped() bool {
    return r.Namespace == ""
}

// String returns a human-readable representation
func (r ResourceIdentifier) String() string {
    if r.Group == "" {
        return fmt.Sprintf("%s/%s/%s", r.Version, r.Resource, r.Name)
    }
    return fmt.Sprintf("%s/%s/%s/%s", r.Group, r.Version, r.Resource, r.Name)
}
```

**Path Format Examples**:

| Resource Type | Current Path | New API-Aligned Path |
|--------------|--------------|----------------------|
| Deployment | `namespaces/production/deployments/app.yaml` | `apps/v1/deployments/production/app.yaml` |
| Pod | `namespaces/default/pods/nginx.yaml` | `v1/pods/default/nginx.yaml` |
| ConfigMap | `namespaces/default/configmaps/config.yaml` | `v1/configmaps/default/config.yaml` |
| ClusterRole | `cluster-scoped/clusterroles/admin.yaml` | `rbac.authorization.k8s.io/v1/clusterroles/admin.yaml` |
| Custom CRD | `namespaces/prod/myapps/instance.yaml` | `example.com/v1alpha1/myapps/prod/instance.yaml` |

**Testing Strategy**:
- Create `internal/types/identifier_test.go`
- Test cases:
  - Core namespaced resources (Pod, ConfigMap)
  - Core cluster-scoped resources (Node, ClusterRole)
  - Non-core namespaced resources (Deployment, StatefulSet)
  - Non-core cluster-scoped resources (CustomResourceDefinition)
  - Custom CRDs with different group patterns
  - Edge cases: empty strings, special characters in names
  - String() representation for logging
  - FromAdmissionRequest() extraction accuracy

**Files to create**:
- `internal/types/identifier.go` (~80 lines)
- `internal/types/identifier_test.go` (~150 lines)

#### Step 1.2: Create PartialObjectMeta Type (1-2 hours)

**Location**: Create new file `internal/sanitize/types.go`

**Implementation**:
```go
package sanitize

import (
    "strings"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PartialObjectMeta defines the subset of ObjectMeta fields we preserve
// in GitOps storage. This explicitly documents our sanitization policy.
type PartialObjectMeta struct {
    Name        string            `json:"name,omitempty" yaml:"name,omitempty"`
    Namespace   string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
    Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
    Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// FromUnstructured extracts PartialObjectMeta from an unstructured object
func (p *PartialObjectMeta) FromUnstructured(obj *unstructured.Unstructured) {
    p.Name = obj.GetName()
    p.Namespace = obj.GetNamespace()
    p.Labels = obj.GetLabels()
    p.Annotations = cleanAnnotations(obj.GetAnnotations())
}

// cleanAnnotations removes operational annotations
// Adapted from Kyverno's approach for cleaning system-managed annotations
func cleanAnnotations(annotations map[string]string) map[string]string {
    if annotations == nil {
        return nil
    }
    
    cleaned := make(map[string]string)
    for k, v := range annotations {
        // Skip kubectl and system operational annotations
        if strings.HasPrefix(k, "kubectl.kubernetes.io/") {
            continue
        }
        if strings.HasPrefix(k, "control-plane.alpha.kubernetes.io/") {
            continue
        }
        if strings.HasPrefix(k, "deployment.kubernetes.io/") {
            continue
        }
        if strings.HasPrefix(k, "autoscaling.alpha.kubernetes.io/") {
            continue
        }
        cleaned[k] = v
    }
    
    if len(cleaned) == 0 {
        return nil
    }
    return cleaned
}
```

**Testing Strategy**:
- Create `internal/sanitize/types_test.go`
- Test cases:
  - FromUnstructured() with various objects
  - Annotation cleaning with kubectl annotations
  - Annotation cleaning with system annotations
  - Empty/nil annotations handling
  - Labels preservation
  - Namespace handling (empty for cluster-scoped)

**Files to create**:
- `internal/sanitize/types.go` (~70 lines)
- `internal/sanitize/types_test.go` (~120 lines)

#### Step 1.3: Update Event Structure (1-2 hours)

**Location**: `internal/eventqueue/queue.go`

**Changes**:
```go
package eventqueue

import (
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Event represents a single resource change to be processed.
type Event struct {
    // Object is the sanitized Kubernetes object.
    Object *unstructured.Unstructured
    
    // Identifier contains all resource identification information
    Identifier types.ResourceIdentifier
    
    // Operation is the admission operation (CREATE, UPDATE, DELETE)
    Operation string
    
    // UserInfo contains relevant user information for commit messages
    UserInfo UserInfo
    
    // GitRepoConfigRef is the name of the GitRepoConfig to use for this event.
    GitRepoConfigRef string
    
    // GitRepoConfigNamespace is the namespace of the GitRepoConfig to use for this event.
    GitRepoConfigNamespace string
}

// UserInfo contains relevant user information for commit messages
type UserInfo struct {
    Username string
    UID      string
}
```

**Testing Strategy**:
- Update `internal/eventqueue/queue_test.go`
- Test cases:
  - Event creation with new structure
  - Queue operations with updated Event type
  - Verify Identifier field properly set
  - UserInfo extraction

**Files to modify**:
- `internal/eventqueue/queue.go` (~10 lines changed)
- `internal/eventqueue/queue_test.go` (~30 lines updated)

#### Step 1.4: Update Webhook Handler (1 hour)

**Location**: `internal/webhook/event_handler.go`

**Changes**:
```go
// Around line 106-114, replace event creation:
event := eventqueue.Event{
    Object:     sanitizedObj,
    Identifier: types.FromAdmissionRequest(req),
    Operation:  string(req.Operation),
    UserInfo: eventqueue.UserInfo{
        Username: req.UserInfo.Username,
        UID:      req.UserInfo.UID,
    },
    GitRepoConfigRef:       rule.GitRepoConfigRef,
    GitRepoConfigNamespace: rule.Source.Namespace,
}
```

**Testing Strategy**:
- Update `internal/webhook/event_handler_test.go`
- Test cases:
  - Identifier correctly extracted from admission request
  - Operation field properly set
  - UserInfo correctly populated
  - Verify event enqueueing with new structure

**Files to modify**:
- `internal/webhook/event_handler.go` (~15 lines changed)
- `internal/webhook/event_handler_test.go` (~40 lines updated)

#### Step 1.5: Update Git Operations (1-2 hours)

**Location**: `internal/git/git.go`

**Changes**:

1. **Remove old GetFilePath function** (lines 660-668):
```go
// DELETE this entire function
func GetFilePath(obj *unstructured.Unstructured, resourcePlural string) string {
    // ...
}
```

2. **Update generateLocalCommits** (line 226):
```go
// Replace:
filePath := GetFilePath(event.Object, event.ResourcePlural)

// With:
filePath := event.Identifier.ToGitPath()
```

3. **Update GetCommitMessage** (lines 670-678):
```go
func GetCommitMessage(event eventqueue.Event) string {
    return fmt.Sprintf("[%s] %s by user/%s",
        event.Operation,
        event.Identifier.String(),
        event.UserInfo.Username,
    )
}
```

**Testing Strategy**:
- Update `internal/git/git_test.go`
- Test cases:
  - Path generation for various resource types
  - Verify API-aligned path format
  - Commit message formatting
  - Integration test with full event flow

**Files to modify**:
- `internal/git/git.go` (~20 lines changed, ~10 lines removed)
- `internal/git/git_test.go` (~60 lines updated)

#### Step 1.6: Improve Webhook Logging (30 minutes)

**Goal**: Simplify webhook logging to provide clear, concise information about incoming events.

**Location**: `internal/webhook/event_handler.go`

**Current Logging** (lines 38-50, 87-100):
```go
// Multiple verbose log statements scattered throughout
log.Info("Received admission request", "operation", req.Operation, "kind", req.Kind.Kind, "name", req.Name, "namespace", req.Namespace)
// ...
log.Info("Checking for matching rules", "kind", obj.GetKind(), "resourcePlural", resourcePlural, "name", obj.GetName(), "namespace", obj.GetNamespace(), "matchingRulesCount", len(matchingRules))
```

**New Consolidated Logging**:
```go
// After matching rules (around line 103):
if len(matchingRules) > 0 {
    log.Info(
        fmt.Sprintf("Received %s for %s: matched %d watchrule(s)",
            req.Operation,
            types.FromAdmissionRequest(req).String(),
            len(matchingRules)),
    )
    // ... rest of event processing
}
// No log output when no rules matched (reduces verbosity)
```

**Benefits**:
- **Single line per webhook call** - Easy to grep and monitor
- **Uses ResourceIdentifier.String()** - Consistent, readable format
- **Shows match count** - Quick visibility into rule effectiveness
- **Silent when no match** - Reduces log noise (system resources, etc.)

**Example log outputs**:
```
Received CREATE for apps/v1/deployments/my-app: matched 2 watchrule(s)
Received UPDATE for v1/configmaps/app-config: matched 1 watchrule(s)
Received DELETE for rbac.authorization.k8s.io/v1/clusterroles/admin: matched 1 watchrule(s)
```

**Testing Strategy**:
- Update `internal/webhook/event_handler_test.go`
- Test cases:
  - Verify log output format for CREATE/UPDATE/DELETE
  - Verify no log output when no rules match
  - Verify ResourceIdentifier.String() used correctly
  - Test with various resource types

**Files to modify**:
- `internal/webhook/event_handler.go` (~15 lines changed, ~10 lines removed)
- `internal/webhook/event_handler_test.go` (~20 lines updated)

#### Step 1.7: Enhance Metrics with Match Labels (30 minutes)

**Goal**: Add detailed metrics to track incoming events and distinguish between matched and unmatched webhook calls.

**Location**: `internal/metrics/exporter.go` and `internal/webhook/event_handler.go`

**Current Metrics** (from `internal/metrics/exporter.go`):
```go
EventsReceivedTotal  // Basic counter, no labels
EventsProcessedTotal // Only counts matched events
```

**New/Enhanced Metrics**:
```go
// Replace EventsReceivedTotal with labeled version
var (
    WebhookEventsTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "gitops_reverser_webhook_events_total",
            Help: "Total number of webhook events received",
        },
        []string{
            "operation",      // CREATE, UPDATE, DELETE
            "group",          // e.g., "apps", "" for core
            "version",        // e.g., "v1"
            "resource",       // e.g., "deployments", "pods"
            "matched",        // "true" or "false" - whether any watchrule matched
        },
    )
    
    WebhookMatchedRulesTotal = promauto.NewHistogram(
        prometheus.HistogramOpts{
            Name: "gitops_reverser_webhook_matched_rules",
            Help: "Number of watchrules matched per webhook event",
            Buckets: []float64{0, 1, 2, 3, 5, 10},
        },
    )
)
```

**Usage in webhook handler**:
```go
// After matching rules (around line 103):
identifier := types.FromAdmissionRequest(req)
matchCount := len(matchingRules)

// Record metrics
metrics.WebhookEventsTotal.WithLabelValues(
    string(req.Operation),
    identifier.Group,
    identifier.Version,
    identifier.Resource,
    strconv.FormatBool(matchCount > 0),
).Inc()

metrics.WebhookMatchedRulesTotal.Observe(float64(matchCount))
```

**Benefits**:
- **Track match rate** - See which resources are/aren't matched by watchrules
- **Identify noise** - Find resources generating unmatched events
- **Operation breakdown** - Understand CREATE vs UPDATE vs DELETE patterns
- **Resource insights** - See which resource types are most active
- **Rule effectiveness** - Histogram shows typical match counts

**Useful Queries**:
```promql
# Total events by match status
sum by (matched) (gitops_reverser_webhook_events_total)

# Match rate percentage
sum(gitops_reverser_webhook_events_total{matched="true"}) / sum(gitops_reverser_webhook_events_total) * 100

# Most active unmatched resources
topk(10, sum by (resource) (gitops_reverser_webhook_events_total{matched="false"}))

# Average rules matched per event
rate(gitops_reverser_webhook_matched_rules_sum[5m]) / rate(gitops_reverser_webhook_matched_rules_count[5m])
```

**Testing Strategy**:
- Update `internal/metrics/exporter_test.go`
- Update `internal/webhook/event_handler_test.go`
- Test cases:
  - Verify metrics recorded with correct labels
  - Test matched vs unmatched event tracking
  - Verify histogram buckets appropriate
  - Test with various operations and resource types

**Files to modify**:
- `internal/metrics/exporter.go` (~30 lines added)
- `internal/webhook/event_handler.go` (~10 lines added)
- `internal/metrics/exporter_test.go` (~40 lines added)
- `internal/webhook/event_handler_test.go` (~30 lines updated)

---

### Phase 2: Enhanced Sanitization (2-3 hours)

**Goal**: Implement comprehensive field removal adapted from Kyverno's approach, including nested fields.

#### Step 2.1: Adapt Kyverno's Metadata Cleaning (1 hour)

**Location**: `internal/sanitize/sanitize.go`

**Add function** (adapted from Kyverno):
```go
// removeAutoGenMetadata removes server-generated metadata fields
// Adapted from Kyverno's excludeAutoGenMetadata function
func removeAutoGenMetadata(metadata map[string]interface{}) {
    // Fields that are always removed
    delete(metadata, "uid")
    delete(metadata, "resourceVersion")
    delete(metadata, "generation")
    delete(metadata, "creationTimestamp")
    delete(metadata, "deletionTimestamp")
    delete(metadata, "deletionGracePeriodSeconds")
    delete(metadata, "selfLink")
    delete(metadata, "managedFields")
    delete(metadata, "ownerReferences")
}
```

**Update Sanitize() function**:
```go
func Sanitize(obj *unstructured.Unstructured) *unstructured.Unstructured {
    sanitized := &unstructured.Unstructured{Object: make(map[string]interface{})}
    
    setCoreIdentityFields(sanitized, obj)
    setCleanMetadata(sanitized, obj)
    preserveFields(sanitized, obj, []string{"spec", "data", "binaryData"})
    preserveTopLevelFields(sanitized, obj)
    
    // NEW: Remove auto-generated metadata
    if metadata, found, _ := unstructured.NestedMap(sanitized.Object, "metadata"); found {
        removeAutoGenMetadata(metadata)
        _ = unstructured.SetNestedMap(sanitized.Object, metadata, "metadata")
    }
    
    // NEW: Remove nested server-generated fields
    removeNestedServerFields(sanitized)
    
    return sanitized
}
```

**Testing Strategy**:
- Update `internal/sanitize/sanitize_test.go`
- Test cases:
  - Verify all auto-gen metadata fields removed
  - Check preserved fields remain intact
  - Test with various resource types
  - Verify nested field removal

**Files to modify**:
- `internal/sanitize/sanitize.go` (~30 lines added)
- `internal/sanitize/sanitize_test.go` (~50 lines added)

#### Step 2.2: Add Nested Field Sanitization (1-2 hours)

**Location**: `internal/sanitize/sanitize.go`

**Add function**:
```go
// removeNestedServerFields removes nested server-generated fields from spec
// based on resource kind
func removeNestedServerFields(obj *unstructured.Unstructured) {
    kind := obj.GetKind()
    
    switch kind {
    case "Service":
        removeServiceFields(obj)
    case "Pod":
        removePodFields(obj)
    case "PersistentVolumeClaim":
        removePVCFields(obj)
    // Add more as needed
    }
}

func removeServiceFields(obj *unstructured.Unstructured) {
    unstructured.RemoveNestedField(obj.Object, "spec", "clusterIP")
    unstructured.RemoveNestedField(obj.Object, "spec", "clusterIPs")
    unstructured.RemoveNestedField(obj.Object, "spec", "healthCheckNodePort")
    unstructured.RemoveNestedField(obj.Object, "spec", "ipFamilies")
    unstructured.RemoveNestedField(obj.Object, "spec", "ipFamilyPolicy")
    unstructured.RemoveNestedField(obj.Object, "spec", "internalTrafficPolicy")
}

func removePodFields(obj *unstructured.Unstructured) {
    unstructured.RemoveNestedField(obj.Object, "spec", "nodeName")
    // Add more Pod-specific fields as needed
}

func removePVCFields(obj *unstructured.Unstructured) {
    unstructured.RemoveNestedField(obj.Object, "spec", "volumeName")
    unstructured.RemoveNestedField(obj.Object, "spec", "volumeMode")
}
```

**Testing Strategy**:
- Add tests in `internal/sanitize/sanitize_test.go`
- Test cases for each resource type:
  - Service: clusterIP, clusterIPs removed
  - Pod: nodeName removed
  - PVC: volumeName removed
  - Verify other spec fields preserved
  - Test with real-world manifests

**Files to modify**:
- `internal/sanitize/sanitize.go` (~60 lines added)
- `internal/sanitize/sanitize_test.go` (~80 lines added)

---

### Phase 3: Ordered YAML Marshaling (2-3 hours)

**Goal**: Create marshaling function that guarantees conventional field order using PartialObjectMeta.

#### Step 3.1: Create Ordered Marshaling (2-3 hours)

**Location**: Create `internal/sanitize/marshal.go`

**Implementation**:
```go
package sanitize

import (
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "sigs.k8s.io/yaml"
)

// OrderedObject ensures conventional field ordering for YAML output.
// The order of struct fields dictates YAML output order.
type OrderedObject struct {
    APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
    Kind       string            `json:"kind" yaml:"kind"`
    Metadata   PartialObjectMeta `json:"metadata" yaml:"metadata"`
    Payload    map[string]interface{} `json:",inline" yaml:",inline"`
}

// MarshalToOrderedYAML converts an unstructured object to YAML with guaranteed field order.
// Field order: apiVersion, kind, metadata, then payload (spec, data, rules, etc.)
func MarshalToOrderedYAML(obj *unstructured.Unstructured) ([]byte, error) {
    // Extract type-safe metadata
    var metadata PartialObjectMeta
    metadata.FromUnstructured(obj)
    
    // Extract payload (everything except apiVersion, kind, metadata, status)
    payload := make(map[string]interface{})
    for k, v := range obj.Object {
        if k != "apiVersion" && k != "kind" && k != "metadata" && k != "status" {
            payload[k] = v
        }
    }
    
    // Build ordered structure
    ordered := OrderedObject{
        APIVersion: obj.GetAPIVersion(),
        Kind:       obj.GetKind(),
        Metadata:   metadata,
        Payload:    payload,
    }
    
    return yaml.Marshal(ordered)
}
```

**Testing Strategy**:
- Create `internal/sanitize/marshal_test.go`
- Test cases:
  - Verify field order in YAML output (apiVersion first, kind second, metadata third)
  - Test with different resource types (Pod, Service, Deployment, CRD)
  - Verify payload fields come after metadata
  - Test empty metadata handling
  - Verify YAML is valid Kubernetes format
  - Round-trip test: marshal then unmarshal

**Example test**:
```go
func TestMarshalToOrderedYAML_FieldOrder(t *testing.T) {
    obj := &unstructured.Unstructured{}
    obj.SetAPIVersion("apps/v1")
    obj.SetKind("Deployment")
    obj.SetName("test")
    obj.SetNamespace("default")
    
    yamlBytes, err := MarshalToOrderedYAML(obj)
    require.NoError(t, err)
    
    yamlStr := string(yamlBytes)
    
    // Verify field order by checking line positions
    apiVersionPos := strings.Index(yamlStr, "apiVersion:")
    kindPos := strings.Index(yamlStr, "kind:")
    metadataPos := strings.Index(yamlStr, "metadata:")
    
    assert.True(t, apiVersionPos < kindPos, "apiVersion should come before kind")
    assert.True(t, kindPos < metadataPos, "kind should come before metadata")
}
```

**Files to create**:
- `internal/sanitize/marshal.go` (~60 lines)
- `internal/sanitize/marshal_test.go` (~180 lines)

---

### Phase 4: Integration & Testing (2-3 hours)

**Goal**: Integrate all components and perform comprehensive end-to-end testing.

#### Step 4.1: Update Git Package Integration (1 hour)

**Location**: `internal/git/git.go`

**Change marshaling** (line ~301):
```go
// Replace:
content, err := yaml.Marshal(event.Object.Object)

// With:
content, err := sanitize.MarshalToOrderedYAML(event.Object)
```

**Add import**:
```go
import (
    // ... existing imports
    "github.com/ConfigButler/gitops-reverser/internal/sanitize"
)
```

**Testing Strategy**:
- Update `internal/git/git_test.go`
- Test cases:
  - Verify YAML in Git has correct field order
  - Test commit creation with new marshaling
  - Verify file paths match API structure
  - Integration test: webhook → sanitize → marshal → commit

**Files to modify**:
- `internal/git/git.go` (~5 lines changed)
- `internal/git/git_test.go` (~40 lines added)

#### Step 4.2: End-to-End Testing (1-2 hours)

**Create integration test**: `test/integration/sanitization_test.go`

**Test scenarios**:
1. **Full workflow test**:
   - Create admission request
   - Extract identifier
   - Sanitize object
   - Marshal to YAML
   - Verify field order
   - Verify Git path

2. **Resource-specific tests**:
   - Service: verify clusterIP removed
   - Pod: verify nodeName and IPs removed
   - Deployment: verify status removed
   - Custom CRD: verify status removed, spec preserved

3. **Path generation tests**:
   - Core resources (Pod, ConfigMap)
   - Extensions (Deployment, StatefulSet)
   - RBAC (ClusterRole, Role)
   - Custom CRDs with various group names

4. **YAML validation**:
   - Parse generated YAML with kubectl
   - Verify it's valid Kubernetes format
   - Check no server-generated fields present

**Testing Strategy**:
- Use table-driven tests for multiple resource types
- Create realistic test fixtures from actual Kubernetes objects
- Verify against the explicit field removal list
- Compare output with expected sanitized YAML

**Files to create**:
- `test/integration/sanitization_test.go` (~200 lines)
- `test/fixtures/` directory with sample manifests

#### Step 4.3: Update E2E Tests (30 minutes)

**Location**: `test/e2e/e2e_test.go`

**Add tests**:
- Verify Git repository has API-aligned structure
- Check committed YAML files have correct field order
- Validate no server-generated fields in Git

**Testing Strategy**:
- Run against real Kubernetes cluster
- Create resources and verify Git commits
- Check file paths match expectations
- Verify YAML quality

**Files to modify**:
- `test/e2e/e2e_test.go` (~60 lines added)

---

## Migration Strategy

### No Migration Required

**Current Status**: This project has not been released to external users yet. Therefore:

- ✅ **No feature flags needed** - We can implement the new path format directly
- ✅ **No migration scripts needed** - No existing user repositories to migrate
- ✅ **Clean slate implementation** - Apply new structure from the start

### Breaking Changes for Future Reference

When this project is eventually released to external users, the following breaking changes should be documented:

**Git Repository Structure**:
- Path format: `{group}/{version}/{resource}/{namespace}/{name}` (API-aligned)
- Example: `apps/v1/deployments/production/app.yaml`

**If migration becomes necessary in the future**, consider:
1. **Feature flag**: Environment variable for backward compatibility period
2. **Migration script**: Tool to reorganize existing Git repositories
3. **Documentation**: Clear migration guide with before/after examples
4. **Rollback plan**: Instructions for reverting if needed

**Note**: Since we're implementing this before first release, users will adopt the new structure from day one, avoiding future migration complexity.

---

## Validation Checklist

Before marking implementation complete, verify:

- [ ] `make fmt` passes
- [ ] `make lint` passes
- [ ] `make test` passes with >90% coverage for new code
- [ ] `make test-e2e` passes
- [ ] Manual YAML inspection shows correct field order:
  ```yaml
  apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: example
    namespace: default
  spec:
    replicas: 3
  ```
- [ ] Git paths match API structure (e.g., `apps/v1/deployments/prod/app.yaml`)
- [ ] Nested fields removed (e.g., `spec.clusterIP` in Services)
- [ ] Operational annotations removed
- [ ] Works with CRDs and built-in resources
- [ ] Type safety verified (no raw map operations where types exist)

---

## Timeline Summary

| Phase | Description | Time Estimate |
|-------|-------------|---------------|
| Phase 1 | Type System Setup + Logging & Metrics | 6-8 hours |
| Phase 2 | Enhanced Sanitization | 2-3 hours |
| Phase 3 | Ordered YAML Marshaling | 2-3 hours |
| Phase 4 | Integration & Testing | 2-3 hours |
| **Total** | | **12-17 hours** |

**Phase 1 Breakdown**:
- Step 1.1: ResourceIdentifier Type (2 hours)
- Step 1.2: PartialObjectMeta Type (1-2 hours)
- Step 1.3: Update Event Structure (1-2 hours)
- Step 1.4: Update Webhook Handler (1 hour)
- Step 1.5: Update Git Operations (1-2 hours)
- Step 1.6: Improve Webhook Logging (30 minutes)
- Step 1.7: Enhance Metrics with Match Labels (30 minutes)

---

## Considered Alternatives

During research, we evaluated several approaches:

### Alternative 1: Import Argo CD's normalize package
```go
import "github.com/argoproj/argo-cd/v2/util/kube"
normalized := kube.NormalizeObject(obj)
```

**Pros**: Most mature normalization logic, comprehensive resource handling
**Cons**: Heavy dependency tree, includes more than we need, less control
**Decision**: Rejected - Too heavyweight for our needs

### Alternative 2: Adapt Kyverno's approach ✅ **CHOSEN**
```go
// Copy their metadata cleaning logic
// Add our YAML ordering layer
```

**Pros**: Lightweight, proven logic, minimal dependencies, Apache 2.0 license
**Cons**: Need to adapt rather than import directly
**Decision**: **Selected** - Best balance of proven logic and control

### Alternative 3: Pure custom implementation
```go
// Build everything from scratch
```

**Pros**: Full control, no external patterns to adapt
**Cons**: Reinventing the wheel, less battle-tested
**Decision**: Rejected - Kyverno's approach is already proven

### Alternative 4: Use Flux/Kustomize normalization
```go
import "sigs.k8s.io/kustomize/api/resource"
```

**Pros**: Standard tool in GitOps ecosystem
**Cons**: Doesn't solve field ordering, focuses on different use case
**Decision**: Rejected - Doesn't meet our ordering requirement

---

## References

- Current sanitize implementation: [`internal/sanitize/sanitize.go`](../internal/sanitize/sanitize.go)
- Current git operations: [`internal/git/git.go`](../internal/git/git.go)
- Kyverno metadata cleaning: https://github.com/kyverno/kyverno/blob/main/pkg/utils/kube/resource.go
- Argo CD normalization: https://github.com/argoproj/argo-cd/blob/master/util/kube/normalize.go
- Kubernetes unstructured: https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1/unstructured
- YAML library: https://pkg.go.dev/sigs.k8s.io/yaml

---

**Document Status**: Final Implementation Plan  
**Created**: 2025-10-09  
**Author**: Kilo Code Architect Mode  
**Next Action**: Approval and implementation