# GitRepoConfig AccessPolicy: Detailed Design Document

**Status**: Design Phase  
**API Version**: v1alpha1  
**Feature**: Cross-Namespace Access Control

## Overview

The AccessPolicy field enables GitRepoConfig to control which WatchRules and ClusterWatchRules can reference it, enabling secure resource sharing across namespaces while maintaining security boundaries.

## Problem Statement

**Current Behavior**: GitRepoConfig can only be referenced by WatchRules in the same namespace.

**Limitations**:
1. Teams need separate GitRepoConfigs for the same repository (inefficient)
2. No way to share a central audit repository across namespaces
3. Forces duplication of Git credentials and configurations

**Solution**: Add `accessPolicy` field to GitRepoConfig for flexible, secure access control.

## Use Cases

### Use Case 1: Central Audit Repository

**Scenario**: Organization wants single audit repository for all Kubernetes changes

**Solution**:
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: company-audit
  namespace: audit-system
spec:
  repoUrl: "git@github.com:company/k8s-audit.git"
  branch: "main"
  secretRef:
    name: git-credentials
  accessPolicy:
    namespacedRules:
      mode: AllNamespaces  # Any namespace can use this
    allowClusterRules: true  # Allow cluster resources too
```

### Use Case 2: Team-Specific Repositories

**Scenario**: Platform team wants shared repository only for their namespaces

**Solution**:
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: platform-audit
  namespace: platform-system
spec:
  repoUrl: "git@github.com:company/platform-audit.git"
  branch: "main"
  secretRef:
    name: git-credentials
  accessPolicy:
    namespacedRules:
      mode: FromSelector
      namespaceSelector:
        matchLabels:
          team: platform
          environment: production
```

### Use Case 3: Namespace-Isolated (Default)

**Scenario**: Development team wants private audit repository

**Solution**:
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: dev-audit
  namespace: dev-team
spec:
  repoUrl: "git@github.com:company/dev-audit.git"
  branch: "main"
  secretRef:
    name: git-credentials
  # No accessPolicy = defaults to SameNamespace mode
```

## API Specification

### Complete Type Definitions

```go
// GitRepoConfigSpec defines the desired state of GitRepoConfig.
type GitRepoConfigSpec struct {
    // RepoURL is the URL of the Git repository
    // +required
    RepoURL string `json:"repoUrl"`
    
    // Branch is the Git branch to commit to
    // +required
    Branch string `json:"branch"`
    
    // SecretRef specifies the Secret containing Git credentials
    // +optional
    SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
    
    // Push defines the strategy for pushing commits
    // +optional
    Push *PushStrategy `json:"push,omitempty"`
    
    // AccessPolicy controls which WatchRules can reference this GitRepoConfig.
    // If not specified, defaults to SameNamespace mode (most restrictive).
    // +optional
    AccessPolicy *AccessPolicy `json:"accessPolicy,omitempty"`
}

// AccessPolicy defines access control rules for GitRepoConfig.
type AccessPolicy struct {
    // NamespacedRules controls access from namespace-scoped WatchRules.
    // If not specified, defaults to SameNamespace mode.
    // +optional
    NamespacedRules *NamespacedRulesPolicy `json:"namespacedRules,omitempty"`
    
    // AllowClusterRules controls whether cluster-scoped ClusterWatchRules
    // can reference this GitRepoConfig.
    // Defaults to false for security (explicit opt-in required).
    // +optional
    // +kubebuilder:default=false
    AllowClusterRules bool `json:"allowClusterRules,omitempty"`
}

// NamespacedRulesPolicy defines which namespaces can access this GitRepoConfig.
type NamespacedRulesPolicy struct {
    // Mode determines the access control mode.
    // - SameNamespace (default): Only WatchRules in the same namespace
    // - AllNamespaces: WatchRules from any namespace can access
    // - FromSelector: Only namespaces matching the selector
    // +optional
    // +kubebuilder:default=SameNamespace
    // +kubebuilder:validation:Enum=SameNamespace;AllNamespaces;FromSelector
    Mode AccessPolicyMode `json:"mode,omitempty"`
    
    // NamespaceSelector selects which namespaces can access this GitRepoConfig.
    // ONLY evaluated when Mode is "FromSelector".
    // MUST be nil when Mode is NOT "FromSelector".
    // +optional
    NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// AccessPolicyMode defines the access control mode.
// +kubebuilder:validation:Enum=SameNamespace;AllNamespaces;FromSelector
type AccessPolicyMode string

const (
    // AccessPolicyModeSameNamespace allows only same namespace access (default, most secure)
    AccessPolicyModeSameNamespace AccessPolicyMode = "SameNamespace"
    
    // AccessPolicyModeAllNamespaces allows access from any namespace
    AccessPolicyModeAllNamespaces AccessPolicyMode = "AllNamespaces"
    
    // AccessPolicyModeFromSelector allows access from matching namespaces only
    AccessPolicyModeFromSelector AccessPolicyMode = "FromSelector"
)
```

## CEL Validation Rules

### Rule 1: NamespaceSelector Requires FromSelector Mode

**Goal**: Prevent invalid configuration where selector is set but mode doesn't use it

**Implementation**:
```go
// +kubebuilder:validation:XValidation:rule="!has(self.accessPolicy) || !has(self.accessPolicy.namespacedRules) || !has(self.accessPolicy.namespacedRules.namespaceSelector) || self.accessPolicy.namespacedRules.mode == 'FromSelector'",message="namespaceSelector can only be set when mode is 'FromSelector'"
type GitRepoConfigSpec struct {
    // ... fields
}
```

**CEL Expression Breakdown**:
```
!has(self.accessPolicy)  
    OR !has(self.accessPolicy.namespacedRules)  
    OR !has(self.accessPolicy.namespacedRules.namespaceSelector)  
    OR self.accessPolicy.namespacedRules.mode == 'FromSelector'
```

**Logic**: Validation passes if ANY of these is true:
1. No accessPolicy defined (valid: use defaults)
2. No namespacedRules defined (valid: use defaults)
3. No namespaceSelector defined (valid: not using selector)
4. Mode is FromSelector (valid: selector is used)

**Result**: Validation **fails** only when selector is set but mode is NOT FromSelector

### Rule 2: FromSelector Mode Requires NamespaceSelector

**Implementation** (in webhook validation):
```go
if namespacedRules.Mode == AccessPolicyModeFromSelector {
    if namespacedRules.NamespaceSelector == nil {
        return fmt.Errorf("namespaceSelector is required when mode is 'FromSelector'")
    }
}
```

## Validation Webhook

### File: `internal/webhook/v1alpha1/gitrepoconfig_webhook.go`

```go
package v1alpha1

import (
    "fmt"
    
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    logf "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/webhook"
    
    configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var gitrepoconfiglog = logf.Log.WithName("gitrepoconfig-webhook")

// SetupWebhookWithManager registers the webhook with the manager
func (r *configv1alpha1.GitRepoConfig) SetupWebhookWithManager(mgr ctrl.Manager) error {
    return ctrl.NewWebhookManagedBy(mgr).
        For(r).
        Complete()
}

// +kubebuilder:webhook:path=/validate-configbutler-ai-v1alpha1-gitrepoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=configbutler.ai,resources=gitrepoconfigs,verbs=create;update,versions=v1alpha1,name=vgitrepoconfig.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &configv1alpha1.GitRepoConfig{}

// ValidateCreate implements webhook.Validator
func (r *configv1alpha1.GitRepoConfig) ValidateCreate() error {
    gitrepoconfiglog.Info("validate create", "name", r.Name)
    return r.validateAccessPolicy()
}

// ValidateUpdate implements webhook.Validator
func (r *configv1alpha1.GitRepoConfig) ValidateUpdate(old runtime.Object) error {
    gitrepoconfiglog.Info("validate update", "name", r.Name)
    return r.validateAccessPolicy()
}

// ValidateDelete implements webhook.Validator
func (r *configv1alpha1.GitRepoConfig) ValidateDelete() error {
    gitrepoconfiglog.Info("validate delete", "name", r.Name)
    // No validation needed for delete
    return nil
}

// validateAccessPolicy validates the accessPolicy field
func (r *configv1alpha1.GitRepoConfig) validateAccessPolicy() error {
    if r.Spec.AccessPolicy == nil {
        return nil // No access policy = use defaults (valid)
    }
    
    namespacedRules := r.Spec.AccessPolicy.NamespacedRules
    if namespacedRules == nil {
        return nil // No namespaced rules = use defaults (valid)
    }
    
    // Validate: namespaceSelector requires mode=FromSelector
    if namespacedRules.NamespaceSelector != nil {
        if namespacedRules.Mode != configv1alpha1.AccessPolicyModeFromSelector {
            return fmt.Errorf(
                "namespaceSelector can only be set when mode is 'FromSelector', got mode '%s'",
                namespacedRules.Mode,
            )
        }
    }
    
    // Validate: mode=FromSelector requires namespaceSelector
    if namespacedRules.Mode == configv1alpha1.AccessPolicyModeFromSelector {
        if namespacedRules.NamespaceSelector == nil {
            return fmt.Errorf(
                "namespaceSelector is required when mode is 'FromSelector'",
            )
        }
        
        // Validate label selector is well-formed
        _, err := metav1.LabelSelectorAsSelector(namespacedRules.NamespaceSelector)
        if err != nil {
            return fmt.Errorf("invalid namespaceSelector: %w", err)
        }
    }
    
    return nil
}
```

## Controller Implementation

### WatchRule Controller Updates

**File**: `internal/controller/watchrule_controller.go`

```go
// validateGitRepoConfigAccess checks if WatchRule can access GitRepoConfig
func (r *WatchRuleReconciler) validateGitRepoConfigAccess(
    ctx context.Context,
    watchRule *configv1alpha1.WatchRule,
) error {
    // Get GitRepoConfig (in same namespace as WatchRule)
    gitRepoConfig := &configv1alpha1.GitRepoConfig{}
    err := r.Get(ctx, types.NamespacedName{
        Name:      watchRule.Spec.GitRepoConfigRef,
        Namespace: watchRule.Namespace,
    }, gitRepoConfig)
    
    if err != nil {
        return fmt.Errorf("GitRepoConfig not found: %w", err)
    }
    
    // Check access policy
    if gitRepoConfig.Spec.AccessPolicy == nil {
        // Default: SameNamespace
        if gitRepoConfig.Namespace != watchRule.Namespace {
            return fmt.Errorf("GitRepoConfig does not allow cross-namespace access (no accessPolicy)")
        }
        return nil
    }
    
    policy := gitRepoConfig.Spec.AccessPolicy.NamespacedRules
    if policy == nil {
        // Default: SameNamespace
        if gitRepoConfig.Namespace != watchRule.Namespace {
            return fmt.Errorf("GitRepoConfig does not allow cross-namespace access (no namespacedRules)")
        }
        return nil
    }
    
    switch policy.Mode {
    case configv1alpha1.AccessPolicyModeSameNamespace, "": // Empty string = default
        if gitRepoConfig.Namespace != watchRule.Namespace {
            return fmt.Errorf("GitRepoConfig only allows same-namespace access")
        }
        
    case configv1alpha1.AccessPolicyModeAllNamespaces:
        // Always allowed
        return nil
        
    case configv1alpha1.AccessPolicyModeFromSelector:
        // Get namespace object to check labels
        ns := &corev1.Namespace{}
        err := r.Get(ctx, types.NamespacedName{Name: watchRule.Namespace}, ns)
        if err != nil {
            return fmt.Errorf("failed to get namespace %s: %w", watchRule.Namespace, err)
        }
        
        // Check if namespace matches selector
        selector, err := metav1.LabelSelectorAsSelector(policy.NamespaceSelector)
        if err != nil {
            return fmt.Errorf("invalid namespace selector: %w", err)
        }
        
        if !selector.Matches(labels.Set(ns.Labels)) {
            return fmt.Errorf(
                "namespace '%s' does not match GitRepoConfig selector",
                watchRule.Namespace,
            )
        }
    }
    
    return nil
}
```

### ClusterWatchRule Controller Implementation

**File**: `internal/controller/clusterwatchrule_controller.go`

```go
// Check if GitRepoConfig allows cluster rules
if gitRepoConfig.Spec.AccessPolicy != nil {
    if !gitRepoConfig.Spec.AccessPolicy.AllowClusterRules {
        return fmt.Errorf("GitRepoConfig does not allow cluster rules")
    }
} else {
    // Default: do not allow cluster rules (explicit opt-in required)
    return fmt.Errorf("GitRepoConfig must explicitly allow cluster rules via accessPolicy.allowClusterRules")
}
```

## Default Behavior

**When accessPolicy is NOT specified**:
```yaml
# Equivalent to:
accessPolicy:
  namespacedRules:
    mode: SameNamespace
  allowClusterRules: false
```

**Security Principle**: Default is most restrictive (same namespace only, no cluster rules)

## Examples

### Example 1: Minimal Configuration (Default)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: team-repo
  namespace: team-a
spec:
  repoUrl: "git@github.com:company/team-a-audit.git"
  branch: "main"
  secretRef:
    name: git-creds
# No accessPolicy = SameNamespace mode, no cluster rules
```

**Behavior**:
- ✅ WatchRules in `team-a` namespace can use this
- ❌ WatchRules in other namespaces cannot use this
- ❌ ClusterWatchRules cannot use this

### Example 2: Allow All Namespaces

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: central-audit
  namespace: audit-system
spec:
  repoUrl: "git@github.com:company/k8s-audit.git"
  branch: "main"
  secretRef:
    name: git-creds
  accessPolicy:
    namespacedRules:
      mode: AllNamespaces
```

**Behavior**:
- ✅ WatchRules in ANY namespace can use this
- ❌ ClusterWatchRules cannot use this (allowClusterRules defaults to false)

### Example 3: Selector-Based Access

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: platform-audit
  namespace: platform-system
spec:
  repoUrl: "git@github.com:company/platform-audit.git"
  branch: "main"
  secretRef:
    name: git-creds
  accessPolicy:
    namespacedRules:
      mode: FromSelector
      namespaceSelector:
        matchLabels:
          team: platform
        matchExpressions:
        - key: environment
          operator: In
          values: [production, staging]
```

**Behavior**:
- ✅ WatchRules in namespaces with `team=platform` AND `environment in [production, staging]`
- ❌ WatchRules in other namespaces
- ❌ ClusterWatchRules cannot use this

### Example 4: Full Access (Cluster + All Namespaces)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: global-audit
  namespace: audit-system
spec:
  repoUrl: "git@github.com:company/global-audit.git"
  branch: "main"
  secretRef:
    name: git-creds
  accessPolicy:
    namespacedRules:
      mode: AllNamespaces
    allowClusterRules: true  # Explicit opt-in
```

**Behavior**:
- ✅ WatchRules in ANY namespace can use this
- ✅ ClusterWatchRules can use this
- ⚠️ Most permissive configuration (use with caution)

### Example 5: Invalid Configuration (Caught by CEL)

```yaml
# ❌ This will be REJECTED by CEL validation
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: invalid-config
  namespace: audit-system
spec:
  repoUrl: "git@github.com:company/audit.git"
  branch: "main"
  secretRef:
    name: git-creds
  accessPolicy:
    namespacedRules:
      mode: AllNamespaces  # Mode is NOT FromSelector
      namespaceSelector:    # But selector is defined!
        matchLabels:
          team: platform
```

**Error**: `namespaceSelector can only be set when mode is 'FromSelector'`

## Status Conditions

WatchRule/ClusterWatchRule conditions related to accessPolicy:

- **AccessDenied**: GitRepoConfig doesn't allow access from this rule
- **GitRepoConfigNotFound**: Referenced GitRepoConfig doesn't exist
- **InvalidAccessPolicy**: GitRepoConfig has invalid accessPolicy configuration

## Testing Strategy

### Unit Tests

```go
func TestGitRepoConfigWebhook_ValidateAccessPolicy(t *testing.T) {
    tests := []struct{
        name        string
        config      *configv1alpha1.GitRepoConfig
        expectError bool
        errorMsg    string
    }{
        {
            name: "valid - no access policy",
            config: minimalGitRepoConfig(),
            expectError: false,
        },
        {
            name: "valid - AllNamespaces mode",
            config: gitRepoConfigWithAllNamespaces(),
            expectError: false,
        },
        {
            name: "valid - FromSelector with selector",
            config: gitRepoConfigWithSelector(),
            expectError: false,
        },
        {
            name: "invalid - selector without FromSelector mode",
            config: gitRepoConfigWithSelectorButWrongMode(),
            expectError: true,
            errorMsg: "namespaceSelector can only be set when mode is 'FromSelector'",
        },
        {
            name: "invalid - FromSelector without selector",
            config: gitRepoConfigWithFromSelectorButNoSelector(),
            expectError: true,
            errorMsg: "namespaceSelector is required when mode is 'FromSelector'",
        },
    }
    // ... test implementation
}
```

### E2E Tests

1. **Cross-Namespace Access Test**: Create GitRepoConfig with AllNamespaces, verify WatchRule in different namespace works
2. **Selector Test**: Create namespaces with different labels, verify only matching ones can access
3. **Cluster Rules Test**: Verify ClusterWatchRule access control
4. **Default Behavior Test**: Verify same-namespace-only default

## Migration Considerations

Since there's no public release:
- No migration required
- No backward compatibility concerns
- New field is optional with safe defaults

## Security Implications

### Risks

1. **AllNamespaces mode**: Any namespace can use the GitRepoConfig (including malicious actors)
2. **allowClusterRules**: Grants access to cluster-wide resources
3. **FromSelector abuse**: Attackers might label their namespaces to match selectors

### Mitigations

1. **Default deny**: Defaults to most restrictive (SameNamespace, no cluster rules)
2. **Explicit opt-in**: Cluster rules require `allowClusterRules: true`
3. **Audit trail**: All access attempts logged
4. **RBAC integration**: Namespace creation/labeling controlled by RBAC
5. **Webhook validation**: Prevents invalid configurations

## Best Practices

1. **Least Privilege**: Use SameNamespace unless cross-namespace needed
2. **Specific Selectors**: Prefer FromSelector over AllNamespaces
3. **Label Governance**: Control namespace labeling via RBAC
4. **Separate Configs**: Use different GitRepoConfigs for different security zones
5. **Monitor Access**: Watch for unexpected namespace access patterns

## Summary

AccessPolicy provides flexible, secure cross-namespace access control for GitRepoConfig while maintaining strong security defaults. CEL validation prevents configuration errors, and webhook validation ensures runtime safety.

The three-tier model (SameNamespace/AllNamespaces/FromSelector) plus cluster rule control provides the right balance of flexibility and security for enterprise Kubernetes environments.