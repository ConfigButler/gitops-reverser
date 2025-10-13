# WatchRule Enhancement: Webhook-like Filtering Implementation

**Status**: ✅ Phase 1 Complete (WatchRule), ClusterWatchRule Postponed
**Created**: 2025-10-09
**Updated**: 2025-10-09
**No Backward Compatibility Required**: This is a breaking change as there are no production users yet.

---

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Security Model & Resource Scope](#security-model--resource-scope)
3. [Custom Types vs Kubernetes Types](#custom-types-vs-kubernetes-types)
4. [Wildcard Behavior](#wildcard-behavior)
5. [Enhanced API Design](#enhanced-api-design)
6. [Implementation Changes](#implementation-changes)
7. [Testing Strategy](#testing-strategy)
8. [Implementation Checklist](#implementation-checklist)

---

## Executive Summary

This enhancement adds webhook-like filtering to GitOps Reverser's watch system:

1. **✅ WatchRule** (namespace-scoped): Watches namespaced resources within a single namespace - **IMPLEMENTED**
2. **🚧 ClusterWatchRule** (cluster-scoped): Watches cluster-scoped resources - **POSTPONED**

The WatchRule enhancement provides powerful, familiar filtering capabilities inspired by Kubernetes admission control patterns while maintaining clear security boundaries.

### Why Enhance WatchRule?

**Current limitations**:
1. No operation filtering - watches all CREATE/UPDATE/DELETE
2. No API group filtering - can't distinguish `pods` (core) from `pods.custom.io`
3. No scope filtering - wastes CPU on irrelevant resources
4. No version filtering - can't prefer stable APIs
5. Only has excludeLabels - no positive inclusion

**Benefits of enhancement**:
1. **Performance**: Scope filtering avoids unnecessary matching
2. **Precision**: API group qualification prevents ambiguity
3. **Flexibility**: Operation filtering (e.g., ignore deletes)
4. **Familiarity**: Same semantics as ValidatingWebhookConfiguration
5. **Type Safety**: Reuses well-tested Kubernetes types
6. **Future-proof**: Aligns with Kubernetes admission control patterns

---

## Security Model & Resource Scope

### Two CRDs for Clear Security Boundaries

We implement **two separate CRDs** to enforce security and prevent privilege escalation:

#### 1. WatchRule (Namespace-Scoped)

**Scope**: Watches **only namespaced resources in its own namespace**

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: my-watch-rule
  namespace: team-a  # <-- This WatchRule ONLY watches resources in team-a namespace
spec:
  gitRepoConfigRef: team-a-repo
  rules:
  - resources: [pods, configmaps]
```

**Security benefits**:
- **Multi-tenancy**: Teams can only watch their own resources
- **Least privilege**: No cross-namespace visibility
- **RBAC integration**: Kubernetes RBAC controls WatchRule creation per namespace
- **Audit trail**: Clear namespace boundaries for compliance

#### 2. ClusterWatchRule (Cluster-Scoped)

**Scope**: Watches **only cluster-scoped resources**

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: watch-cluster-resources  # No namespace - cluster-scoped
spec:
  gitRepoConfigRef: platform-repo
  gitRepoConfigNamespace: gitops-system  # Where to find the GitRepoConfig
  rules:
  - resources: [nodes, clusterroles, customresourcedefinitions]
```

### Why Separate CRDs?

**Security & least privilege**:
- **Explicit RBAC**: Creating ClusterWatchRule requires cluster-admin permissions
- **No privilege escalation**: Teams with namespace access can't watch cluster resources
- **Clear audit trail**: Cluster-level watching is explicitly tracked
- **Multi-tenancy**: Each namespace manages its own WatchRules independently

**No cross-namespace watching**: WatchRule in namespace A cannot watch resources in namespace B.

**GitRepoConfig reference**:
- WatchRule: References GitRepoConfig in same namespace
- ClusterWatchRule: Must specify GitRepoConfigNamespace explicitly

---

## Custom Types vs Kubernetes Types

### Decision 1: Use Custom Types (Not admissionv1)

Since we have **separate CRDs for namespace vs cluster scope**, the Kubernetes `ScopeType` field becomes **redundant and confusing**. Instead, we define our own types:

### Decision 2: Use ObjectSelector (Not IncludeLabels + ExcludeLabels)

**Question**: Should we have both `includeLabels` and `excludeLabels`?

**Answer**: **No**. Use a single `objectSelector` field instead, following Kubernetes best practices.

**Rationale**:
1. **Kubernetes standard**: ValidatingWebhookConfiguration uses `objectSelector`, not separate include/exclude fields
2. **More expressive**: LabelSelector supports all operations (In, NotIn, Exists, DoesNotExist)
3. **Less confusing**: One field with clear semantics vs two fields with interaction rules
4. **Simpler implementation**: One matching function instead of two with precedence logic
5. **Composable**: Can express complex logic in single selector

**Comparison**:

```yaml
# ❌ BAD: Separate fields (not Kubernetes standard)
includeLabels:
  matchLabels:
    app: myapp
excludeLabels:
  matchLabels:
    ignore: "true"

# ✅ GOOD: Single objectSelector (Kubernetes standard)
objectSelector:
  matchExpressions:
  - key: app
    operator: In
    values: [myapp]
  - key: ignore
    operator: DoesNotExist
```

**Benefits**:
- Same semantics as ValidatingWebhookConfiguration
- Users familiar with admission webhooks understand immediately
- No need to document precedence rules
- More powerful: can use NotIn for "not in this list" logic

### Decision 3: Remove Custom Wildcard Support

Since we have **separate CRDs for namespace vs cluster scope**, the Kubernetes `ScopeType` field becomes **redundant and confusing**. Instead, we define our own types:

**Rationale**:
1. **Scope is implicit**: WatchRule = namespaced only, ClusterWatchRule = cluster only
2. **Simpler API**: No need for users to specify scope (it's determined by CRD type)
3. **Type safety**: Can't accidentally specify wrong scope
4. **Clarity**: Resource watching behavior is clear from CRD name
5. **Still familiar**: Semantics similar to Kubernetes admission control

### Custom Type Definitions

```go
// pkg/apis/configbutler/v1alpha1/types.go

package v1alpha1

// OperationType specifies the type of operation that triggers a watch event.
type OperationType string

const (
    // OperationCreate matches resource creation events
    OperationCreate OperationType = "CREATE"
    // OperationUpdate matches resource update events
    OperationUpdate OperationType = "UPDATE"
    // OperationDelete matches resource deletion events
    OperationDelete OperationType = "DELETE"
    // OperationAll matches all operation types
    OperationAll OperationType = "*"
)

// These types mirror Kubernetes admission control semantics but are
// tailored for our specific use case without unnecessary fields.
```

### Comparison with Kubernetes Types

**Kubernetes admissionv1 types** (ValidatingWebhookConfiguration):
```go
type Rule struct {
    APIGroups   []string      // ✓ Keep - needed for filtering
    APIVersions []string      // ✓ Keep - needed for filtering
    Resources   []string      // ✓ Keep - needed for filtering
    Scope       *ScopeType    // ✗ Remove - implicit from CRD type
}

type ScopeType string
const (
    ClusterScope    ScopeType = "Cluster"
    NamespacedScope ScopeType = "Namespaced"
    AllScopes       ScopeType = "*"
)
```

**Our custom types**:
```go
type ResourceRule struct {
    Operations  []OperationType // Our custom type
    APIGroups   []string        // Keep same as Kubernetes
    APIVersions []string        // Keep same as Kubernetes
    Resources   []string        // Keep same as Kubernetes
    // No Scope field - implicit from WatchRule vs ClusterWatchRule
}
```

## Wildcard Behavior

```go
import (
    admissionv1 "k8s.io/api/admissionregistration/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Core webhook rule structure
type RuleWithOperations struct {
    Operations []OperationType  // Which operations trigger the rule
    Rule                        // Embedded filtering fields
}

type Rule struct {
    APIGroups   []string      // API groups to match (empty string = core API)
    APIVersions []string      // API versions to match
    Resources   []string      // Resource types to match
    Scope       *ScopeType    // Optional: "Cluster", "Namespaced", or "*"
}

// Operation types
type OperationType string
const (
    Create  OperationType = "CREATE"
    Update  OperationType = "UPDATE"
    Delete  OperationType = "DELETE"
    Connect OperationType = "CONNECT"
    All     OperationType = "*"
)

// Scope types
type ScopeType string
const (
    ClusterScope    ScopeType = "Cluster"      // Only cluster-scoped resources
    NamespacedScope ScopeType = "Namespaced"   // Only namespaced resources
    AllScopes       ScopeType = "*"            // Both (default)
)
```

### Wildcard Behavior (Aligned with ValidatingWebhookConfiguration)

To maintain consistency with Kubernetes admission control, we support the **same wildcard semantics** as ValidatingWebhookConfiguration:

1. **Full wildcard**: `"*"` matches all resources
2. **Exact match**: `"pods"` matches only pods (case-insensitive)
3. **Subresource wildcard**: `"pods/*"` matches all pod subresources (e.g., `pods/log`, `pods/status`)
4. **Specific subresource**: `"pods/log"` matches only the log subresource

**NOT supported** (differs from current implementation):
- ❌ Prefix wildcards: `"*.example.com"` - **REMOVE THIS**
- ❌ Suffix wildcards: `"pod*"` - **REMOVE THIS**
- ❌ Middle wildcards: `"p*d"`
- ❌ Multiple wildcards: `"*foo*bar*"`
- ❌ Regex patterns

**Rationale**: These custom wildcard features go beyond Kubernetes standards. Removing them:
- Maintains consistency with ValidatingWebhookConfiguration
- Simplifies implementation and testing
- Prevents user confusion (same semantics as admission webhooks)
- For group-qualified resources, use exact matches: `"myapps.example.com"` (not `"*.example.com"`)

**Examples**:
```yaml
rules:
- resources: ["*"]                           # Matches all resources
- resources: ["pods", "services"]            # Exact matches only
- resources: ["pods/*"]                      # All pod subresources
- resources: ["pods/log", "pods/status"]     # Specific subresources
- resources: ["myapps.example.com"]          # Exact match for CRD (no wildcard)
```

**Migration note**: If your tests use `"ingress*"` or `"*.example.com"`, update to exact matches or `"*"`.

---

## Enhanced API Design

### Proposed WatchRule API

### WatchRule (Namespace-Scoped)

**File**: [`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go)

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OperationType specifies the type of operation that triggers a watch event.
// +kubebuilder:validation:Enum=CREATE;UPDATE;DELETE;*
type OperationType string

const (
    OperationCreate OperationType = "CREATE"
    OperationUpdate OperationType = "UPDATE"
    OperationDelete OperationType = "DELETE"
    OperationAll    OperationType = "*"
)

// WatchRuleSpec defines the desired state of WatchRule.
// WatchRule watches resources ONLY within its own namespace.
type WatchRuleSpec struct {
    // GitRepoConfigRef is the name of the GitRepoConfig to use for this rule.
    // +required
    GitRepoConfigRef string `json:"gitRepoConfigRef"`
    
    // ObjectSelector filters resources by labels (like objectSelector in webhooks).
    // If specified, only resources matching this selector are watched.
    // Use matchExpressions with NotIn/DoesNotExist operators to exclude resources.
    // +optional
    ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`
    
    // Rules define which resources to watch within this namespace.
    // Multiple rules create a logical OR - a resource matching ANY rule is watched.
    // +required
    // +kubebuilder:validation:MinItems=1
    Rules []ResourceRule `json:"rules"`
}

// ResourceRule defines a set of namespaced resources to watch.
// This follows Kubernetes admission control semantics but simplified for our use case.
type ResourceRule struct {
    // Operations to watch. If empty, watches all operations.
    // +optional
    Operations []OperationType `json:"operations,omitempty"`
    
    // APIGroups to match. Empty string ("") matches the core API group.
    // If empty, matches all API groups.
    // Wildcards supported: "*" matches all groups.
    // +optional
    APIGroups []string `json:"apiGroups,omitempty"`
    
    // APIVersions to match. If empty, matches all versions.
    // Wildcards supported: "*" matches all versions.
    // +optional
    APIVersions []string `json:"apiVersions,omitempty"`
    
    // Resources to match (plural names like "pods", "configmaps").
    // This field is required and determines which resource types trigger this rule.
    // Wildcards supported: "*", "prefix*", "*suffix"
    // Examples:
    //   - "pods" (exact match)
    //   - "myapps.example.com" (exact match for CRD - no wildcards)
    //   - "pods/*" (matches all pod subresources)
    // +required
    // +kubebuilder:validation:MinItems=1
    Resources []string `json:"resources"`
}

// WatchRuleStatus defines the observed state of WatchRule.
type WatchRuleStatus struct {
    // Conditions represent the latest available observations of an object's state
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="GitRepoConfig",type=string,JSONPath=`.spec.gitRepoConfigRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WatchRule watches namespaced resources within its own namespace.
// For cluster-scoped resources, use ClusterWatchRule instead.
type WatchRule struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    
    Spec   WatchRuleSpec   `json:"spec"`
    Status WatchRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WatchRuleList contains a list of WatchRule.
type WatchRuleList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []WatchRule `json:"items"`
}

func init() {
    SchemeBuilder.Register(&WatchRule{}, &WatchRuleList{})
}
```

### ClusterWatchRule (Cluster-Scoped)

**File**: [`api/v1alpha1/clusterwatchrule_types.go`](../api/v1alpha1/clusterwatchrule_types.go)

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
    // GitRepoConfigRef is the name of the GitRepoConfig to use.
    // +required
    GitRepoConfigRef string `json:"gitRepoConfigRef"`
    
    // GitRepoConfigNamespace is the namespace containing the GitRepoConfig.
    // This is required because ClusterWatchRule is cluster-scoped and needs
    // to know where to find the namespace-scoped GitRepoConfig.
    // +required
    GitRepoConfigNamespace string `json:"gitRepoConfigNamespace"`
    
    // ObjectSelector filters resources by labels (follows objectSelector semantics).
    // If specified, only resources matching this selector are watched.
    // Use matchExpressions with NotIn/DoesNotExist to exclude resources.
    // +optional
    ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`
    
    // Rules define which cluster-scoped resources to watch.
    // Multiple rules create a logical OR.
    // +required
    // +kubebuilder:validation:MinItems=1
    Rules []ResourceRule `json:"rules"`
}

// ClusterWatchRuleStatus defines the observed state of ClusterWatchRule.
type ClusterWatchRuleStatus struct {
    // Conditions represent the latest available observations
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="GitRepoConfig",type=string,JSONPath=`.spec.gitRepoConfigRef`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.gitRepoConfigNamespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWatchRule watches cluster-scoped resources (Nodes, ClusterRoles, etc).
// Requires cluster-admin RBAC permissions to create.
type ClusterWatchRule struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    
    Spec   ClusterWatchRuleSpec   `json:"spec"`
    Status ClusterWatchRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterWatchRuleList contains a list of ClusterWatchRule.
type ClusterWatchRuleList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []ClusterWatchRule `json:"items"`
}

func init() {
    SchemeBuilder.Register(&ClusterWatchRule{}, &ClusterWatchRuleList{})
}
```

### Example WatchRule (Namespace-Scoped)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: production-app-config
  namespace: team-a  # Watches ONLY resources in team-a namespace
spec:
  gitRepoConfigRef: team-a-repo
  
  # Filter resources by labels (follows Kubernetes objectSelector semantics)
  objectSelector:
    matchExpressions:
    - key: app
      operator: In
      values: [production-app]
    - key: gitops-reverser.io/ignore
      operator: DoesNotExist
  
  rules:
  # Watch only CREATE and UPDATE for config resources
  - operations: [CREATE, UPDATE]  # Ignore DELETE operations
    apiGroups: [""]                # Core API group
    apiVersions: ["v1"]
    resources: [configmaps, secrets]
    scope: Namespaced              # Performance optimization
    
  # Watch all operations for app resources
  - operations: [CREATE, UPDATE, DELETE]
    apiGroups: [apps]
    apiVersions: ["v1"]
    resources: [deployments, statefulsets, daemonsets]
    scope: Namespaced
    
  # Watch custom resources
  - apiGroups: [example.com]
    apiVersions: ["v1", "v1alpha1"]
    resources: [myapps]
```

### Example ClusterWatchRule (Cluster-Scoped)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: platform-infrastructure
spec:
  gitRepoConfigRef: platform-repo
  gitRepoConfigNamespace: gitops-system  # Where to find GitRepoConfig
  
  # Filter resources by labels
  objectSelector:
    matchExpressions:
    - key: gitops-reverser.io/ignore
      operator: DoesNotExist
  
  rules:
  # Watch cluster infrastructure
  - operations: [CREATE, UPDATE, DELETE]
    apiGroups: [""]
    apiVersions: ["v1"]
    resources: [nodes, persistentvolumes]
    
  # Watch RBAC resources
  - operations: [CREATE, UPDATE, DELETE]
    apiGroups: [rbac.authorization.k8s.io]
    apiVersions: ["v1"]
    resources: [clusterroles, clusterrolebindings]
    
  # Watch CRDs
  - operations: [CREATE, UPDATE, DELETE]
    apiGroups: [apiextensions.k8s.io]
    apiVersions: ["v1"]
    resources: [customresourcedefinitions]
```

---

## Implementation Changes

### 1. Update CompiledRule in RuleStore

**File**: [`internal/rulestore/store.go`](../internal/rulestore/store.go)

**Current structure** (flattens all rules):
```go
type CompiledRule struct {
    Source           types.NamespacedName
    GitRepoConfigRef string
    ExcludeLabels    *metav1.LabelSelector
    Resources        []string  // Flattened from all rules
}
```

**New structure** (preserves filtering, handles both CRD types):
```go
type CompiledRule struct {
    Source                 types.NamespacedName
    GitRepoConfigRef       string
    GitRepoConfigNamespace string                     // For ClusterWatchRule
    IsClusterScoped        bool                       // NEW: Distinguishes ClusterWatchRule from WatchRule
    ObjectSelector         *metav1.LabelSelector      // NEW: Label-based filtering
    ResourceRules          []CompiledResourceRule     // NEW: Separate rules
}

type CompiledResourceRule struct {
    Operations  []v1alpha1.OperationType // NEW: Our custom type
    APIGroups   []string                 // NEW
    APIVersions []string                 // NEW
    Resources   []string
    // No Scope field - implicit from CompiledRule.IsClusterScoped
}
```

**Why**: 
- Current implementation loses filtering granularity
- Separate CompiledResourceRule preserves each rule's filters
- IsClusterScoped flag handles both WatchRule and ClusterWatchRule uniformly
- No Scope field needed since CRD type determines scope

### 2. Update AddOrUpdate Methods (Two Variants)

**File**: [`internal/rulestore/store.go`](../internal/rulestore/store.go)

```go
// AddOrUpdateWatchRule adds or updates a namespace-scoped WatchRule
func (s *RuleStore) AddOrUpdateWatchRule(rule configv1alpha1.WatchRule) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    key := types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}
    
    compiled := CompiledRule{
        Source:                 key,
        GitRepoConfigRef:       rule.Spec.GitRepoConfigRef,
        GitRepoConfigNamespace: rule.Namespace,  // Same namespace as WatchRule
        IsClusterScoped:        false,           // WatchRule is namespace-scoped
        ObjectSelector:         rule.Spec.ObjectSelector,
        ResourceRules:          make([]CompiledResourceRule, 0, len(rule.Spec.Rules)),
    }
    
    for _, r := range rule.Spec.Rules {
        compiled.ResourceRules = append(compiled.ResourceRules, CompiledResourceRule{
            Operations:  r.Operations,
            APIGroups:   r.APIGroups,
            APIVersions: r.APIVersions,
            Resources:   r.Resources,
        })
    }
    
    s.rules[key] = compiled
}

// AddOrUpdateClusterWatchRule adds or updates a cluster-scoped ClusterWatchRule
func (s *RuleStore) AddOrUpdateClusterWatchRule(rule configv1alpha1.ClusterWatchRule) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    // Use empty namespace for cluster-scoped rules
    key := types.NamespacedName{Name: rule.Name, Namespace: ""}
    
    compiled := CompiledRule{
        Source:                 key,
        GitRepoConfigRef:       rule.Spec.GitRepoConfigRef,
        GitRepoConfigNamespace: rule.Spec.GitRepoConfigNamespace,  // Explicit namespace
        IsClusterScoped:        true,                              // ClusterWatchRule is cluster-scoped
        ObjectSelector:         rule.Spec.ObjectSelector,
        ResourceRules:          make([]CompiledResourceRule, 0, len(rule.Spec.Rules)),
    }
    
    for _, r := range rule.Spec.Rules {
        compiled.ResourceRules = append(compiled.ResourceRules, CompiledResourceRule{
            Operations:  r.Operations,
            APIGroups:   r.APIGroups,
            APIVersions: r.APIVersions,
            Resources:   r.Resources,
        })
    }
    
    s.rules[key] = compiled
}
```

### 3. Enhance GetMatchingRules Logic

**File**: [`internal/rulestore/store.go`](../internal/rulestore/store.go)

**Current signature**:
```go
func (s *RuleStore) GetMatchingRules(obj client.Object, resourcePlural string) []CompiledRule
```

**New signature** (with filtering context, no scope parameter):
```go
func (s *RuleStore) GetMatchingRules(
    obj client.Object,
    resourcePlural string,
    operation v1alpha1.OperationType,  // NEW: Our custom type
    apiGroup string,                    // NEW
    apiVersion string,                  // NEW
    isClusterScoped bool,              // NEW: Is the resource cluster-scoped?
) []CompiledRule {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    var matchingRules []CompiledRule
    for _, rule := range s.rules {
        // First check: Does rule scope match resource scope?
        if rule.IsClusterScoped != isClusterScoped {
            continue  // WatchRule can't match cluster resources and vice versa
        }
        
        // For namespace-scoped rules, check namespace match
        if !rule.IsClusterScoped && obj.GetNamespace() != rule.Source.Namespace {
            continue  // WatchRule only watches its own namespace
        }
        
        if rule.matches(obj, resourcePlural, operation, apiGroup, apiVersion) {
            matchingRules = append(matchingRules, rule)
        }
    }
    return matchingRules
}
```

**New matching logic**:
```go
func (r *CompiledRule) matches(
    obj client.Object,
    resourcePlural string,
    operation v1alpha1.OperationType,
    apiGroup string,
    apiVersion string,
) bool {
    // Check object selector (label-based filtering)
    if !r.matchesObjectSelector(obj) {
        return false
    }
    
    // Check if any resource rule matches (logical OR)
    for _, rule := range r.ResourceRules {
        if rule.matches(resourcePlural, operation, apiGroup, apiVersion) {
            return true
        }
    }
    
    return false
}

func (r *CompiledRule) matchesObjectSelector(obj client.Object) bool {
    if r.ObjectSelector == nil {
        return true // No selector = match all
    }
    
    selector, err := metav1.LabelSelectorAsSelector(r.ObjectSelector)
    if err != nil {
        return false // Invalid selector = exclude for safety
    }
    
    return selector.Matches(labels.Set(obj.GetLabels()))
}

func (r *CompiledResourceRule) matches(
    resourcePlural string,
    operation v1alpha1.OperationType,
    apiGroup string,
    apiVersion string,
) bool {
    // Match operations (empty = match all)
    if !r.matchesOperations(operation) {
        return false
    }
    
    // Match API groups (empty = match all)
    if !r.matchesAPIGroups(apiGroup) {
        return false
    }
    
    // Match API versions (empty = match all)
    if !r.matchesAPIVersions(apiVersion) {
        return false
    }
    
    // Match resource plural (required)
    return r.resourceMatches(resourcePlural)
}

func (r *CompiledResourceRule) matchesOperations(operation v1alpha1.OperationType) bool {
    if len(r.Operations) == 0 {
        return true // Empty = match all
    }
    
    for _, op := range r.Operations {
        if op == v1alpha1.OperationAll || op == operation {
            return true
        }
    }
    return false
}

func (r *CompiledResourceRule) matchesAPIGroups(apiGroup string) bool {
    if len(r.APIGroups) == 0 {
        return true // Empty = match all
    }
    
    for _, group := range r.APIGroups {
        if group == "*" || group == apiGroup {
            return true
        }
    }
    return false
}

func (r *CompiledResourceRule) matchesAPIVersions(apiVersion string) bool {
    if len(r.APIVersions) == 0 {
        return true // Empty = match all
    }
    
    for _, version := range r.APIVersions {
        if version == "*" || version == apiVersion {
            return true
        }
    }
    return false
}

// resourceMatches needs UPDATE: Remove prefix/suffix wildcard support
// Only support: "*" (all), exact match, and subresource notation ("pods/*")
// isWildcardMatch function should be REMOVED (no longer needed)
```

**Changes to `resourceMatches()` and `singleResourceMatches()`**:

```go
// Update these functions to remove custom wildcard support
func (r *CompiledResourceRule) singleResourceMatches(ruleResource, resourcePlural string) bool {
    if ruleResource == "" {
        return false
    }
    
    // Match wildcard for all resources
    if ruleResource == "*" {
        return true
    }
    
    // Exact match (case-insensitive)
    if strings.EqualFold(ruleResource, resourcePlural) {
        return true
    }
    
    // Subresource wildcard: "pods/*" matches "pods/log", "pods/status", etc.
    if strings.HasSuffix(ruleResource, "/*") {
        prefix := ruleResource[:len(ruleResource)-2]  // Remove "/*"
        return strings.HasPrefix(strings.ToLower(resourcePlural), strings.ToLower(prefix)+"/")
    }
    
    return false
}

// REMOVE isWildcardMatch() function entirely - no longer needed
```

**Key changes**:
- Removed `matchesScope()` - scope is now implicit from IsClusterScoped flag
- Scope filtering happens in `GetMatchingRules()` before calling `matches()`
- Simplified matching logic since CRD type determines scope

### 4. Update EventHandler

**File**: [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)

**Current call**:
```go
matchingRules := h.RuleStore.GetMatchingRules(obj, resourcePlural)
```

**New call** (with filtering context):
```go
func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
    // ... existing decode logic ...
    
    // Extract filtering parameters from admission request
    resourcePlural := req.Resource.Resource
    operation := admissionv1.OperationType(req.Operation)
    apiGroup := req.Resource.Group
    apiVersion := req.Resource.Version
    
    // Determine scope from object
    scope := admissionv1.NamespacedScope
    if obj.GetNamespace() == "" {
        scope = admissionv1.ClusterScope
    }
    
    // Get matching rules with enhanced filtering
    matchingRules := h.RuleStore.GetMatchingRules(
        obj, 
        resourcePlural,
        operation,  // NEW
        apiGroup,   // NEW
        apiVersion, // NEW
        scope,      // NEW
    )
    
    // ... rest of logic unchanged ...
}
```

**Why**: Webhook now passes complete filtering context to rulestore for precise matching.

### 5. New ClusterWatchRuleReconciler

**File**: [`internal/controller/clusterwatchrule_controller.go`](../internal/controller/clusterwatchrule_controller.go) (NEW)

Similar to WatchRuleReconciler but for cluster-scoped rules:

**Key responsibilities**:
1. Fetch GitRepoConfig from specified namespace (not same namespace)
2. Validate GitRepoConfig is ready
3. Call `RuleStore.AddOrUpdateClusterWatchRule()` instead of `AddOrUpdateWatchRule()`
4. No namespace restriction on watched resources

**Key differences from WatchRuleReconciler**:
- GitRepoConfig lookup uses `spec.gitRepoConfigNamespace` instead of rule's namespace
- Cluster-scoped, so no namespace filtering on resources
- RBAC requires cluster-level permissions

### 6. Update WatchRuleReconciler

**File**: [`internal/controller/watchrule_controller.go`](../internal/controller/watchrule_controller.go)

Update method call from `AddOrUpdate()` to `AddOrUpdateWatchRule()`:

```go
// Change this line:
r.RuleStore.AddOrUpdate(watchRule)

// To:
r.RuleStore.AddOrUpdateWatchRule(watchRule)
```

### 7. No Changes Needed

These components work correctly with both CRD types:

- **Git worker**: Processes events from queue (unaware of scope)
- **Event queue**: Stores events (unaware of scope)
- **Sanitization**: Cleans resource data (unaware of scope)

---

## Testing Strategy

### Unit Tests for RuleStore

**File**: [`internal/rulestore/store_test.go`](../internal/rulestore/store_test.go)

**New test cases**:

```go
func TestGetMatchingRules_OperationFiltering(t *testing.T) {
    // Test: Rule with operations: [CREATE, UPDATE] matches CREATE and UPDATE but not DELETE
    // Test: Rule with operations: ["*"] matches all operations
    // Test: Rule with empty operations matches all operations (default behavior)
    // Test: Multiple operations in single rule
}

func TestGetMatchingRules_APIGroupFiltering(t *testing.T) {
    // Test: apiGroups: ["apps"] matches Deployment but not Pod (core group)
    // Test: apiGroups: [""] matches core API (Pod, ConfigMap)
    // Test: apiGroups: ["*"] matches all groups
    // Test: Empty apiGroups matches all groups (default)
    // Test: Exact match only - "example.com" matches "example.com" but not "myapp.example.com"
}

func TestGetMatchingRules_APIVersionFiltering(t *testing.T) {
    // Test: apiVersions: ["v1"] matches v1 but not v1beta1
    // Test: apiVersions: ["*"] matches all versions
    // Test: Empty apiVersions matches all versions (default)
    // Test: Multiple versions in single rule
}

func TestGetMatchingRules_WatchRuleVsClusterWatchRule(t *testing.T) {
    // Test: WatchRule (IsClusterScoped=false) matches namespaced Pod but not cluster Node
    // Test: ClusterWatchRule (IsClusterScoped=true) matches cluster Node but not namespaced Pod
    // Test: WatchRule only matches resources in its own namespace
    // Test: ClusterWatchRule matches cluster resources regardless of namespace
}

func TestGetMatchingRules_ObjectSelector(t *testing.T) {
    // Test: objectSelector with matchLabels filters resources
    // Test: Resource without required label doesn't match
    // Test: Resource with required label matches
    // Test: objectSelector with matchExpressions (In, NotIn, Exists, DoesNotExist)
    // Test: nil objectSelector includes all (default)
    // Test: Complex selector with multiple expressions (all must match)
}

func TestGetMatchingRules_ObjectSelectorExclusion(t *testing.T) {
    // Test: Using NotIn operator to exclude resources
    // Test: Using DoesNotExist to exclude labeled resources
    // Test: Combining positive (In) and negative (DoesNotExist) in same selector
}

func TestGetMatchingRules_CombinedFilters(t *testing.T) {
    // Test: operations + apiGroups + resources + objectSelector work together
    // Test: Multiple rules with different filters in single WatchRule
    // Test: Complex real-world scenarios combining all filter types
}

func TestGetMatchingRules_MultipleRulesLogicalOR(t *testing.T) {
    // Test: Resource matching ANY rule in WatchRule is included
    // Test: First rule matches, second doesn't
    // Test: First rule doesn't match, second does
    // Test: Both rules match
}

func TestCompiledResourceRule_DefaultBehavior(t *testing.T) {
    // Test: Empty/nil fields behave as "match all"
    // Test: Only resources specified, all other fields match everything
}
```

### Integration Tests

**File**: New file `internal/webhook/event_handler_integration_test.go`

```go
func TestEventHandler_OperationFiltering(t *testing.T) {
    // Setup: Create WatchRule with operations: [CREATE, UPDATE]
    // Test: CREATE event triggers
    // Test: UPDATE event triggers
    // Test: DELETE event does NOT trigger
}

func TestEventHandler_ScopeFiltering(t *testing.T) {
    // Setup: Create WatchRule with scope: Namespaced
    // Test: Pod (namespaced) triggers
    // Test: Node (cluster-scoped) does NOT trigger
}

func TestEventHandler_APIGroupFiltering(t *testing.T) {
    // Setup: Create WatchRule with apiGroups: ["apps"]
    // Test: Deployment (apps group) triggers
    // Test: Pod (core group) does NOT trigger
}

func TestEventHandler_ComplexFiltering(t *testing.T) {
    // Setup: WatchRule with operations, apiGroups, and objectSelector
    // Test: Resource matching all criteria triggers
    // Test: Resource missing one criterion does NOT trigger
}
```

### E2E Tests

**File**: [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go)

```go
func TestE2E_EnhancedWatchRule_OperationFiltering(t *testing.T) {
    // Scenario: WatchRule with operations: [CREATE, UPDATE]
    // Action: Create ConfigMap -> Verify Git commit
    // Action: Update ConfigMap -> Verify Git commit
    // Action: Delete ConfigMap -> Verify NO Git commit
}

func TestE2E_EnhancedWatchRule_APIGroupFiltering(t *testing.T) {
    // Scenario: WatchRule with apiGroups: ["apps"]
    // Action: Create Deployment -> Verify Git commit
    // Action: Create Pod -> Verify NO Git commit
}

func TestE2E_EnhancedWatchRule_ObjectSelector(t *testing.T) {
    // Scenario: WatchRule with objectSelector matching app=myapp
    // Action: Create Pod with label app=myapp -> Verify Git commit
    // Action: Create Pod without label -> Verify NO Git commit
    // Scenario: WatchRule with objectSelector excluding ignore=true
    // Action: Create Pod without ignore label -> Verify Git commit
    // Action: Create Pod with ignore=true label -> Verify NO Git commit
}

func TestE2E_EnhancedWatchRule_ComplexScenario(t *testing.T) {
    // Scenario: WatchRule with operations, apiGroups, resources, objectSelector
    // Action: Create resource matching all criteria -> Verify Git commit
    // Action: Create resource missing one criterion -> Verify NO Git commit
}

func TestE2E_ClusterWatchRule_ClusterResources(t *testing.T) {
    // Scenario: ClusterWatchRule watching Nodes
    // Action: Create/Update Node -> Verify Git commit (if writable in test env)
    // Scenario: ClusterWatchRule watching ClusterRoles
    // Action: Create ClusterRole -> Verify Git commit
    // Action: Create Role (namespaced) -> Verify NO Git commit
}

func TestE2E_EnhancedWatchRule_NamespacedScoping(t *testing.T) {
    // Scenario: WatchRule in namespace team-a
    // Action: Create resource in team-a -> Verify Git commit
    // Action: Create resource in team-b -> Verify NO Git commit (namespace isolation)
}
```

---

## Implementation Checklist

### ✅ Phase 1: API Changes (COMPLETED)

#### WatchRule API
- [x] Update [`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go)
  - [x] Add custom `OperationType` type and constants (CREATE, UPDATE, DELETE, *)
  - [x] Add `ObjectSelector` field to WatchRuleSpec (replaces ExcludeLabels)
  - [x] Add `Operations`, `APIGroups`, `APIVersions` to ResourceRule
  - [x] Add kubebuilder validation tags
  - [x] Add comprehensive godoc comments (namespace-scoping security model)
  - [x] Add kubebuilder printcolumn annotations
  - [x] **Removed old `ExcludeLabels` from WatchRuleSpec** (breaking change)

#### 🚧 ClusterWatchRule API (POSTPONED)
- [ ] Create [`api/v1alpha1/clusterwatchrule_types.go`](../api/v1alpha1/clusterwatchrule_types.go)
  - Postponed until WatchRule is validated in production

#### Code Generation
- [x] Run `make generate` to update deepcopy methods
- [x] Run `make manifests` to update CRDs
- [x] Verify generated CRDs in `config/crd/bases/`
  - [x] `configbutler.ai_watchrules.yaml`

### ✅ Phase 2: RuleStore Changes (COMPLETED)

- [x] Update [`internal/rulestore/store.go`](../internal/rulestore/store.go)
  - [x] Add `CompiledResourceRule` type
  - [x] Update `CompiledRule` structure
    - [x] Add `GitRepoConfigNamespace` field
    - [x] Add `IsClusterScoped bool` field (prepared for future ClusterWatchRule)
    - [x] Replace `ExcludeLabels` with `ObjectSelector`
    - [x] Replace `Resources []string` with `ResourceRules []CompiledResourceRule`
  - [x] Create `AddOrUpdateWatchRule()` method
  - [x] Kept `AddOrUpdate()` for compatibility
  - [x] Update `GetMatchingRules()` signature (add operation, apiGroup, apiVersion, isClusterScoped params)
  - [x] Implement `CompiledRule.matchesObjectSelector()`
  - [x] Implement `CompiledResourceRule.matches()`
  - [x] Implement `CompiledResourceRule.matchesOperations()`
  - [x] Implement `CompiledResourceRule.matchesAPIGroups()`
  - [x] Implement `CompiledResourceRule.matchesAPIVersions()`
  - [x] Update `singleResourceMatches()` - removed prefix/suffix wildcards, added subresource support
  - [x] **REMOVED `isWildcardMatch()` method**

### ✅ Phase 3: Controller Changes (COMPLETED)

#### WatchRuleReconciler
- [x] Update [`internal/controller/watchrule_controller.go`](../internal/controller/watchrule_controller.go)
  - [x] Change `r.RuleStore.AddOrUpdate(watchRule)` to `r.RuleStore.AddOrUpdateWatchRule(watchRule)`

#### 🚧 ClusterWatchRuleReconciler (POSTPONED)
- [ ] Create [`internal/controller/clusterwatchrule_controller.go`](../internal/controller/clusterwatchrule_controller.go)
  - Postponed until WatchRule is validated

### ✅ Phase 4: Webhook Handler Changes (COMPLETED)

- [x] Update [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)
  - [x] Extract operation, apiGroup, apiVersion from admission request
  - [x] Calculate isClusterScoped from object (namespace == "")
  - [x] Update `GetMatchingRules()` call with new parameters
  - [x] Add configv1alpha1 import

### ✅ Phase 5: Testing (COMPLETED)

- [x] Update existing tests in [`internal/rulestore/store_test.go`](../internal/rulestore/store_test.go)
  - [x] Fix broken tests (new GetMatchingRules signature)
  - [x] Add operation filtering tests
  - [x] Add API group filtering tests
  - [x] Add namespace isolation tests
  - [x] Add objectSelector tests (replaced ExcludeLabels tests)
  - [x] Add combined filter tests
  - [x] **Removed wildcard tests** for prefix/suffix patterns (pod*, *.example.com)
  - [x] **Added subresource tests** (pods/*, pods/log)
- [x] Update tests in [`internal/webhook/event_handler_test.go`](../internal/webhook/event_handler_test.go)
  - [x] Updated ExcludeLabels to ObjectSelector
  - [x] Fixed cluster-scoped resource test
  - [x] Updated wildcard test to use namespace-scoped resources
- [x] Update E2E tests in [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go)
  - [x] Updated templates to use new API
  - [x] Skipped CRD watching tests (cluster-scoped, needs ClusterWatchRule)

### ✅ Phase 6: Controller Tests (COMPLETED)

- [x] Controller tests pass with new API structure
  - All existing tests continue to work

### ✅ Phase 7: Documentation (COMPLETED)

- [x] Update [`README.md`](../README.md)
  - [x] Document WatchRule API with comprehensive examples
  - [x] Document security model and namespace isolation
  - [x] Explain objectSelector usage
  - [x] Document wildcard behavior
  - [x] Note ClusterWatchRule postponement
- [x] Update [`docs/review-refactor-proposal.md`](review-refactor-proposal.md)
  - [x] Reflect current architecture
- [x] Add godoc comments to all new types and methods
- [x] Update sample manifest in [`config/samples/`](../config/samples/)

### ✅ Phase 8: Validation (COMPLETED)

- [x] Run `make fmt` - ✅ Passed
- [x] Run `make vet` - ✅ Passed
- [x] Run `make lint` - ✅ Passed (0 issues)
- [x] Run `make test` - ✅ Passed (95.9% coverage for rulestore)
- [x] Update `make test-e2e` - ✅ Templates updated, CRD tests skipped

### ✅ Phase 9: Configuration Updates (COMPLETED)

- [x] Update sample WatchRules in [`config/samples/`](../config/samples/)
  - [x] Comprehensive example with all new features
  - [x] Operation filtering, API group filtering, objectSelector
- [x] Helm chart CRDs sync automatically via `make helm-sync-crds`

---

## Migration Notes

**No backward compatibility required** - there are no production users yet.

**Breaking changes**:
1. `Resources []string` field removed from WatchRuleSpec
2. `Resources []string` field moved to ResourceRule
3. New required fields may cause validation errors for old manifests

**Migration path** (if any test/dev WatchRules exist):

**Old format**:
```yaml
spec:
  gitRepoConfigRef: my-repo
  resources: [pods, services]  # REMOVED
```

**New format**:
```yaml
spec:
  gitRepoConfigRef: my-repo
  rules:                        # NEW - required
  - resources: [pods, services]
```

**Automated migration** (if needed for any test data):
```bash
# Simple sed script to update YAML
sed -i 's/resources:/rules:\n  - resources:/' config/samples/*.yaml
```

---

## Future Enhancements

### Namespace Selector for ClusterWatchRule

Future enhancement: Allow ClusterWatchRule to watch resources across specific namespaces:

```go
type ClusterWatchRuleSpec struct {
    // ... existing fields ...
    
    // NamespaceSelector optionally restricts watching to specific namespaces
    // If omitted, watches all namespaces (current behavior)
    // +optional
    NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}
```

**Use case**: Platform team wants to watch all ConfigMaps across namespaces with label `platform-managed: true`

**Timeline**: Post-MVP, based on user feedback

### Metrics and Observability

- Rule match rate metrics per WatchRule/ClusterWatchRule
- Filter performance metrics (how many resources checked vs matched)
- Top filtered resources by operation/group/version

---

## Questions for Review

1. **Default values**: Should empty Operations/APIGroups/APIVersions default to ["*"] explicitly in AddOrUpdate(), or handle implicitly in matching logic?
   - **Recommendation**: Handle implicitly (cleaner code, less duplication)

2. **RBAC for ClusterWatchRule**: Should we add additional RBAC restrictions beyond cluster-scoped create/update?
   - **Recommendation**: Yes, require explicit ClusterRole for ClusterWatchRule management

3. **Validation webhooks**: Should we add admission webhooks to validate WatchRule/ClusterWatchRule configuration?
   - **Recommendation**: Yes, validate that resources/apiGroups/apiVersions make sense together

---

## References

- Current WatchRule: [`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go)
- RuleStore implementation: [`internal/rulestore/store.go`](../internal/rulestore/store.go)
- RuleStore tests: [`internal/rulestore/store_test.go`](../internal/rulestore/store_test.go)
- Event handler: [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)
- Webhook configuration: [`charts/gitops-reverser/templates/validating-webhook.yaml`](../charts/gitops-reverser/templates/validating-webhook.yaml)
- Development rules: [`DEVELOPMENT_RULES.md`](../DEVELOPMENT_RULES.md)