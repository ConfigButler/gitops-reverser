# ClusterWatchRule: Detailed Design Document

**Status**: Design Phase  
**API Version**: v1alpha1  
**Scope**: Cluster

## Overview

ClusterWatchRule is a cluster-scoped Custom Resource Definition (CRD) that provides administrators with a powerful tool to audit Kubernetes resources across the entire cluster. It acts as a set of instructions for the operator, defining which resource changes should be captured and committed to a specified Git repository.

Because it's a **cluster-scoped resource**, only users with cluster-level permissions (like cluster administrators) can create, modify, or delete it.

## When to Use ClusterWatchRule

Use a ClusterWatchRule when you need to:

1. **Audit cluster-scoped resources** like Nodes, ClusterRoles, PersistentVolumes, or CustomResourceDefinitions
2. **Audit namespaced resources across all namespaces simultaneously** (e.g., all Deployments cluster-wide)
3. **Audit namespaced resources in a specific subset of namespaces** that match a label selector (e.g., all namespaces with `env: production`)

## Design Rationale

### Per-Rule namespaceSelector (Most Flexible)

Placing the `namespaceSelector` **within each rule** (rather than at the top level) allows a single ClusterWatchRule to contain highly specific and mixed instructions. This pattern is used by native Kubernetes APIs like `ValidatingWebhookConfiguration`.

**Advantages:**
- A single ClusterWatchRule can audit different resources in different namespace sets
- No need for multiple ClusterWatchRule resources
- Administrators can construct comprehensive, all-in-one audit policies
- Follows established Kubernetes patterns

**Example Use Case:**
```yaml
# Single ClusterWatchRule with mixed scope rules
rules:
- scope: Cluster
  resources: [nodes]              # All Nodes
  
- scope: Namespaced
  resources: [deployments]        # Deployments in ALL namespaces
  
- scope: Namespaced
  resources: [secrets]
  namespaceSelector:              # Secrets only in PCI namespaces
    matchLabels:
      compliance: pci
```

## Complete API Specification

### ClusterWatchRule Type

```go
// ClusterWatchRule watches resources across the entire cluster.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="GitRepoConfig",type=string,JSONPath=`.spec.gitRepoConfigRef.name`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.gitRepoConfigRef.namespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterWatchRule struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    
    // Spec defines the desired state
    // +required
    Spec ClusterWatchRuleSpec `json:"spec"`
    
    // Status defines the observed state
    // +optional
    Status ClusterWatchRuleStatus `json:"status,omitempty"`
}

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
    // GitRepoConfigRef references the GitRepoConfig to use.
    // Since ClusterWatchRule is cluster-scoped and GitRepoConfig is namespace-scoped,
    // both name and namespace must be specified.
    // +required
    GitRepoConfigRef NamespacedName `json:"gitRepoConfigRef"`
    
    // Rules define which resources to watch.
    // Multiple rules create a logical OR - a resource matching ANY rule is watched.
    // Each rule can specify cluster-scoped or namespaced resources.
    // +required
    // +kubebuilder:validation:MinItems=1
    Rules []ClusterResourceRule `json:"rules"`
}

// NamespacedName represents a reference to a namespaced resource.
type NamespacedName struct {
    // Name of the GitRepoConfig
    // +required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
    
    // Namespace containing the GitRepoConfig
    // +required
    // +kubebuilder:validation:MinLength=1
    Namespace string `json:"namespace"`
}

// ClusterResourceRule defines which resources to watch with scope control.
type ClusterResourceRule struct {
    // Operations to watch. If empty, watches all operations (CREATE, UPDATE, DELETE).
    // +optional
    Operations []OperationType `json:"operations,omitempty"`
    
    // APIGroups to match. Empty string ("") matches the core API group.
    // If empty, matches all API groups.
    // +optional
    APIGroups []string `json:"apiGroups,omitempty"`
    
    // APIVersions to match. If empty, matches all versions.
    // +optional
    APIVersions []string `json:"apiVersions,omitempty"`
    
    // Resources to match (plural names like "pods", "configmaps").
    // +required
    // +kubebuilder:validation:MinItems=1
    Resources []string `json:"resources"`
    
    // Scope defines whether this rule watches Cluster-scoped or Namespaced resources.
    // - "Cluster": For cluster-scoped resources (Nodes, ClusterRoles, etc.).
    //              The namespaceSelector is ignored.
    // - "Namespaced": For namespaced resources (Pods, Deployments, etc.).
    //                 Optionally filtered by namespaceSelector.
    // +required
    // +kubebuilder:validation:Enum=Cluster;Namespaced
    Scope ResourceScope `json:"scope"`
    
    // NamespaceSelector filters which namespaces to watch.
    // Only evaluated when Scope is "Namespaced".
    // If omitted for Namespaced scope, watches resources in ALL namespaces.
    // If specified, only watches resources in namespaces matching the selector.
    // Ignored when Scope is "Cluster".
    // +optional
    NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// ResourceScope defines the scope of resources.
// +kubebuilder:validation:Enum=Cluster;Namespaced
type ResourceScope string

const (
    // ResourceScopeCluster indicates cluster-scoped resources
    ResourceScopeCluster ResourceScope = "Cluster"
    
    // ResourceScopeNamespaced indicates namespaced resources
    ResourceScopeNamespaced ResourceScope = "Namespaced"
)

// ClusterWatchRuleStatus defines the observed state.
type ClusterWatchRuleStatus struct {
    // Conditions represent the latest observations
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterWatchRuleList contains a list of ClusterWatchRule.
// +kubebuilder:object:root=true
type ClusterWatchRuleList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []ClusterWatchRule `json:"items"`
}
```

## Example Manifests

### Example 1: Comprehensive Audit Policy (All Three Patterns)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: global-audit-policy
spec:
  gitRepoConfigRef:
    name: main-audit-repo
    namespace: git-audit-system
  
  rules:
    # Rule 1: Audit cluster-scoped resources (namespaceSelector ignored)
    - scope: Cluster
      apiGroups: [rbac.authorization.k8s.io]
      resources: [clusterroles, clusterrolebindings]
      operations: [CREATE, UPDATE, DELETE]
    
    # Rule 2: Audit Deployments in ALL namespaces
    - scope: Namespaced
      apiGroups: [apps]
      resources: [deployments]
      # No namespaceSelector = all namespaces
    
    # Rule 3: Audit Secrets ONLY in PCI-compliant namespaces
    - scope: Namespaced
      apiGroups: [""]
      resources: [secrets]
      namespaceSelector:
        matchLabels:
          compliance: pci
    
    # Rule 4: Audit ConfigMaps in team-blue namespaces
    - scope: Namespaced
      apiGroups: [""]
      resources: [configmaps]
      namespaceSelector:
        matchLabels:
          team: team-blue
```

### Example 2: Infrastructure Resources Only

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: infrastructure-audit
spec:
  gitRepoConfigRef:
    name: infra-audit-repo
    namespace: platform-system
  
  rules:
    # Watch all cluster-scoped infrastructure
    - scope: Cluster
      apiGroups: [""]
      resources: [nodes, persistentvolumes]
    
    - scope: Cluster
      apiGroups: [storage.k8s.io]
      resources: [storageclasses, volumeattachments]
    
    - scope: Cluster
      apiGroups: [networking.k8s.io]
      resources: [ingressclasses]
```

### Example 3: CRD Lifecycle Tracking

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: crd-lifecycle-audit
spec:
  gitRepoConfigRef:
    name: crd-audit-repo
    namespace: audit-system
  
  rules:
    - scope: Cluster
      apiGroups: [apiextensions.k8s.io]
      apiVersions: [v1]
      resources: [customresourcedefinitions]
      operations: [CREATE, UPDATE, DELETE]
```

### Example 4: Production Namespaces Only

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: production-audit
spec:
  gitRepoConfigRef:
    name: prod-audit-repo
    namespace: audit-system
  
  rules:
    # All resource types in production namespaces
    - scope: Namespaced
      resources: ["*"]  # All resources
      namespaceSelector:
        matchLabels:
          environment: production
```

### Example 5: Multi-Team with Different Policies

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: multi-team-audit
spec:
  gitRepoConfigRef:
    name: team-audit-repo
    namespace: audit-system
  
  rules:
    # Platform team: watch system namespaces
    - scope: Namespaced
      resources: [deployments, services]
      namespaceSelector:
        matchLabels:
          managed-by: platform-team
    
    # Security team: watch secrets in security namespaces
    - scope: Namespaced
      apiGroups: [""]
      resources: [secrets, serviceaccounts]
      namespaceSelector:
        matchLabels:
          security-zone: high
    
    # All teams: watch RBAC cluster resources
    - scope: Cluster
      apiGroups: [rbac.authorization.k8s.io]
      resources: [clusterroles, clusterrolebindings]
```

## Controller Implementation

### File: `internal/controller/clusterwatchrule_controller.go`

```go
package controller

import (
    "context"
    "fmt"
    
    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/labels"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/log"
    
    configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
    "github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// ClusterWatchRuleReconciler reconciles a ClusterWatchRule object
type ClusterWatchRuleReconciler struct {
    client.Client
    Scheme    *runtime.Scheme
    RuleStore *rulestore.RuleStore
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *ClusterWatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)
    
    // Fetch ClusterWatchRule
    clusterRule := &configv1alpha1.ClusterWatchRule{}
    if err := r.Get(ctx, req.NamespacedName, clusterRule); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }
    
    // Validate GitRepoConfig exists
    gitRepoConfig := &configv1alpha1.GitRepoConfig{}
    gitRepoConfigKey := types.NamespacedName{
        Name:      clusterRule.Spec.GitRepoConfigRef.Name,
        Namespace: clusterRule.Spec.GitRepoConfigRef.Namespace,
    }
    
    if err := r.Get(ctx, gitRepoConfigKey, gitRepoConfig); err != nil {
        log.Error(err, "GitRepoConfig not found")
        // Set NotReady condition
        return ctrl.Result{}, err
    }
    
    // Check if GitRepoConfig allows cluster rules
    if gitRepoConfig.Spec.AccessPolicy != nil {
        if !gitRepoConfig.Spec.AccessPolicy.AllowClusterRules {
            log.Info("GitRepoConfig does not allow cluster rules")
            // Set AccessDenied condition
            return ctrl.Result{}, fmt.Errorf("GitRepoConfig does not allow cluster rules")
        }
    } else {
        // Default: do not allow cluster rules
        log.Info("GitRepoConfig has no accessPolicy (defaults to no cluster rules)")
        return ctrl.Result{}, fmt.Errorf("GitRepoConfig must explicitly allow cluster rules")
    }
    
    // Validate namespace selectors for Namespaced rules
    for i, rule := range clusterRule.Spec.Rules {
        if rule.Scope == configv1alpha1.ResourceScopeNamespaced && rule.NamespaceSelector != nil {
            // Validate selector is well-formed
            _, err := metav1.LabelSelectorAsSelector(rule.NamespaceSelector)
            if err != nil {
                log.Error(err, "Invalid namespaceSelector", "ruleIndex", i)
                return ctrl.Result{}, fmt.Errorf("invalid namespaceSelector in rule %d: %w", i, err)
            }
        }
    }
    
    // Update RuleStore
    r.RuleStore.AddOrUpdateClusterWatchRule(*clusterRule)
    log.Info("Updated RuleStore with ClusterWatchRule")
    
    // Set Ready condition
    // (Status update implementation)
    
    return ctrl.Result{}, nil
}

func (r *ClusterWatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&configv1alpha1.ClusterWatchRule{}).
        Complete(r)
}
```

## RuleStore Updates

### File: `internal/rulestore/store.go`

```go
// CompiledClusterRule represents a compiled ClusterWatchRule for efficient matching
type CompiledClusterRule struct {
    Source                 types.NamespacedName
    GitRepoConfigRef       string
    GitRepoConfigNamespace string
    Rules                  []CompiledClusterResourceRule
}

// CompiledClusterResourceRule represents a single compiled rule with scope
type CompiledClusterResourceRule struct {
    Operations        []configv1alpha1.OperationType
    APIGroups         []string
    APIVersions       []string
    Resources         []string
    Scope             configv1alpha1.ResourceScope
    NamespaceSelector *metav1.LabelSelector
}

// AddOrUpdateClusterWatchRule adds or updates a cluster-scoped ClusterWatchRule
func (s *RuleStore) AddOrUpdateClusterWatchRule(rule configv1alpha1.ClusterWatchRule) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    key := types.NamespacedName{
        Name:      rule.Name,
        Namespace: "", // Empty for cluster-scoped
    }
    
    compiled := CompiledClusterRule{
        Source:                 key,
        GitRepoConfigRef:       rule.Spec.GitRepoConfigRef.Name,
        GitRepoConfigNamespace: rule.Spec.GitRepoConfigRef.Namespace,
        Rules:                  make([]CompiledClusterResourceRule, 0, len(rule.Spec.Rules)),
    }
    
    for _, r := range rule.Spec.Rules {
        compiled.Rules = append(compiled.Rules, CompiledClusterResourceRule{
            Operations:        r.Operations,
            APIGroups:         r.APIGroups,
            APIVersions:       r.APIVersions,
            Resources:         r.Resources,
            Scope:             r.Scope,
            NamespaceSelector: r.NamespaceSelector,
        })
    }
    
    s.clusterRules[key] = compiled
}

// GetMatchingClusterRules returns ClusterWatchRules matching the resource
func (s *RuleStore) GetMatchingClusterRules(
    obj client.Object,
    resourcePlural string,
    operation configv1alpha1.OperationType,
    apiGroup string,
    apiVersion string,
    isClusterScoped bool,
) []CompiledClusterRule {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    var matchingRules []CompiledClusterRule
    for _, rule := range s.clusterRules {
        for _, r := range rule.Rules {
            // Check scope matches
            if isClusterScoped && r.Scope != configv1alpha1.ResourceScopeCluster {
                continue
            }
            if !isClusterScoped && r.Scope != configv1alpha1.ResourceScopeNamespaced {
                continue
            }
            
            // For namespaced resources, check namespace selector
            if !isClusterScoped && r.NamespaceSelector != nil {
                selector, err := metav1.LabelSelectorAsSelector(r.NamespaceSelector)
                if err != nil {
                    continue // Invalid selector, skip
                }
                
                // Get namespace labels (need namespace object)
                // This requires the namespace to be passed or looked up
                // Implementation detail: pass namespace labels to this function
                if !selector.Matches(labels.Set(/* namespace labels */)) {
                    continue
                }
            }
            
            // Check other filters (operation, apiGroup, apiVersion, resource)
            if r.matches(resourcePlural, operation, apiGroup, apiVersion) {
                matchingRules = append(matchingRules, rule)
                break // Don't add same rule multiple times
            }
        }
    }
    return matchingRules
}
```

## Git Repository Structure

### Cluster-Scoped Resources (No Namespace)

```
<repo-root>/
├── <apiGroup>/<version>/<resources>/<name>.yaml
│
├── v1/nodes/worker-node-1.yaml
├── v1/persistentvolumes/pv-storage-1.yaml
├── rbac.authorization.k8s.io/v1/clusterroles/admin.yaml
└── apiextensions.k8s.io/v1/customresourcedefinitions/mycrds.example.com.yaml
```

### Namespaced Resources (With Namespace)

```
<repo-root>/
├── <apiGroup>/<version>/<resources>/<namespace>/<name>.yaml
│
├── v1/configmaps/production/app-config.yaml
├── v1/secrets/team-blue/db-credentials.yaml
└── apps/v1/deployments/production/web-app.yaml
```

## Security Considerations

### 1. Cluster-Admin Required

ClusterWatchRule requires cluster-admin permissions because:
- Can watch all cluster-scoped resources
- Can watch namespaced resources across all namespaces
- Has cluster-wide visibility

### 2. GitRepoConfig Access Control

- GitRepoConfig must explicitly allow cluster rules: `accessPolicy.allowClusterRules: true`
- Default: deny (security first)
- GitRepoConfig stays namespaced (for secret management)

### 3. Namespace Selector Security

- Namespace labels controlled by RBAC
- Attackers can't arbitrarily label namespaces to bypass selectors
- Monitor namespace label changes

### 4. Audit Trail

All changes logged with:
- User attribution (who made the change)
- Timestamp
- Operation type
- Resource details

## Field Reference

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `spec.gitRepoConfigRef` | Yes | Object | Reference to GitRepoConfig |
| `spec.gitRepoConfigRef.name` | Yes | String | GitRepoConfig name |
| `spec.gitRepoConfigRef.namespace` | Yes | String | GitRepoConfig namespace |
| `spec.rules` | Yes | Array | List of resource rules |
| `spec.rules[].operations` | No | Array[String] | Operations to watch (CREATE, UPDATE, DELETE, *) |
| `spec.rules[].apiGroups` | No | Array[String] | API groups ("" for core) |
| `spec.rules[].apiVersions` | No | Array[String] | API versions |
| `spec.rules[].resources` | Yes | Array[String] | Resource types (plural) |
| `spec.rules[].scope` | Yes | Enum | "Cluster" or "Namespaced" |
| `spec.rules[].namespaceSelector` | No | LabelSelector | Namespace filter (only for Namespaced scope) |

## Testing Strategy

### Unit Tests

```go
func TestClusterWatchRuleReconciler(t *testing.T) {
    tests := []struct{
        name string
        rule configv1alpha1.ClusterWatchRule
        expectError bool
    }{
        {
            name: "valid cluster rule",
            rule: clusterRuleForNodes(),
            expectError: false,
        },
        {
            name: "valid namespaced rule all namespaces",
            rule: clusterRuleForAllDeployments(),
            expectError: false,
        },
        {
            name: "valid namespaced rule with selector",
            rule: clusterRuleForProductionSecrets(),
            expectError: false,
        },
    }
    // ... implementation
}
```

### E2E Tests

1. **CRD Watching**: Install/update/delete CRDs, verify Git commits
2. **Node Watching**: Label nodes, verify commits
3. **Cross-Namespace**: Create resources in multiple namespaces, verify filtering
4. **Selector Matching**: Test namespace label selectors

## Best Practices

1. **Specific Rules**: Avoid `resources: ["*"]` with `scope: Namespaced` and no selector
2. **Label Governance**: Control namespace labeling via RBAC
3. **Separate Repos**: Use different GitRepoConfigs for different security zones
4. **Monitor Access**: Watch for unexpected ClusterWatchRule creations
5. **Test Selectors**: Verify namespace selectors match expected namespaces

## Summary

ClusterWatchRule with per-rule `scope` and `namespaceSelector` provides:

✅ Maximum flexibility - single rule can define complex audit policies  
✅ Follows Kubernetes patterns - similar to ValidatingWebhookConfiguration  
✅ Clear intent - explicit scope declaration  
✅ Granular control - different namespace sets per rule  
✅ Security - explicit opt-in for cluster access via GitRepoConfig  

This design allows administrators to create comprehensive, maintainable audit policies in single manifests.