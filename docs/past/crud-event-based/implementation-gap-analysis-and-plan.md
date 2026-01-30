# GitOps Reverser: Implementation Gap Analysis and Architectural Plan

**Date**: 2025-10-10  
**Status**: Architecture Planning Phase  
**Version**: v1alpha1

## Executive Summary

This document provides a comprehensive gap analysis between the current implementation and the architectural design documented in [`review-refactor-proposal.md`](review-refactor-proposal.md), along with a detailed implementation plan for remaining features.

### Key Findings

**✅ COMPLETED FEATURES:**
- WatchRule CRD with namespace-scoped resources
- GitRepoConfig CRD with Git repository management
- Admission webhook for capturing resource changes
- Sanitization engine removing server-managed fields
- Git worker with conflict resolution
- Event queue for buffering changes
- RuleStore for efficient rule matching
- E2E tests for ConfigMaps and Custom Resources (IceCreamOrder)
- Metrics and observability
- Leader election for HA deployments

**🚧 MISSING FEATURES:**
1. **ClusterWatchRule CRD** - For cluster-scoped AND namespaced resources with per-rule scope control
2. **GitRepoConfig accessPolicy field** - For cross-namespace access control
3. **CEL validation** - For enforcing accessPolicy constraints
4. **E2E tests** - Two tests are currently disabled (lines 691-693 and 1108-1110 in e2e_test.go)
5. **README best practices** - Single GitRepoConfig per repository guidance

**🎯 KEY DESIGN DECISION:**
ClusterWatchRule uses **per-rule `scope` field** (Cluster or Namespaced) with optional per-rule `namespaceSelector`. This allows a single ClusterWatchRule to audit BOTH cluster-scoped resources AND namespaced resources (optionally filtered by namespace labels) in one manifest.

---

## Part 1: Current Implementation Analysis

### 1.1 Implemented Components

#### ✅ WatchRule (Namespace-Scoped)
**Location**: [`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go)

**Status**: Fully implemented and tested

**Features**:
- Namespace isolation (can only watch resources in own namespace)
- Operation filtering (CREATE, UPDATE, DELETE)
- API group/version filtering
- Label-based object selection
- Multiple rules with logical OR
- Wildcard support for resources (`*`, `pods/*`, `pods/log`)

**Test Coverage**: E2E tests passing for:
- ConfigMap CREATE/UPDATE/DELETE operations
- Custom Resource (IceCreamOrder) CREATE/UPDATE/DELETE
- Status field filtering verification

#### ✅ GitRepoConfig
**Location**: [`api/v1alpha1/gitrepoconfig_types.go`](../api/v1alpha1/gitrepoconfig_types.go)

**Status**: Fully implemented

**Features**:
- SSH and HTTPS authentication
- Branch validation
- Secret reference (namespaced)
- Push strategy (interval, maxCommits)
- Status conditions

**Missing**: `accessPolicy` field for cross-namespace rule access

#### ✅ Rule Store
**Location**: [`internal/rulestore/store.go`](../internal/rulestore/store.go)

**Status**: Fully implemented with cluster-scoped support prepared

**Features**:
- Thread-safe concurrent access
- Namespace-aware filtering (line 134: enforces namespace isolation)
- Cluster-scoped resource detection (line 129-131)
- Label selector matching
- Operation/group/version filtering

**Ready for Enhancement**: Already has `IsClusterScoped` field (line 29) prepared for ClusterWatchRule

#### ✅ Event Handler
**Location**: [`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go)

**Status**: Fully implemented with cluster-scoped support prepared

**Features**:
- Admission webhook handling for all resource types
- Resource scope detection (line 94: `isClusterScoped`)
- DELETE operation support with OldObject
- Sanitization integration
- Multi-rule matching

**Ready for Enhancement**: Already passes `isClusterScoped` parameter to GetMatchingRules (line 103)

### 1.2 Architecture Alignment

The current implementation **closely follows** the design in [`review-refactor-proposal.md`](review-refactor-proposal.md):

| Component | Design Doc | Implementation | Status |
|-----------|------------|----------------|--------|
| Resource Identification | ✓ | ✓ | ✅ Complete |
| Manifest Sanitization | ✓ | ✓ | ✅ Complete |
| WatchRule (namespaced) | ✓ | ✓ | ✅ Complete |
| ClusterWatchRule | ✓ Planned | ❌ Missing | 🚧 To Do |
| Git Operations | ✓ | ✓ | ✅ Complete |
| Event Processing | ✓ | ✓ | ✅ Complete |
| Webhook | ✓ | ✓ | ✅ Complete |
| Rule Store | ✓ | ✓ | ✅ Complete |
| Controllers | ✓ | ✓ | ✅ Complete |

---

## Part 2: ClusterWatchRule Architecture (Per-Rule Scope Design)

### 2.1 Design Requirements

**Purpose**: Watch both cluster-scoped AND namespaced resources with fine-grained per-rule control

**Key Innovation**: Each rule has its own `scope` field and optional `namespaceSelector`

**ClusterWatchRule Can Audit**:
1. **Cluster-scoped resources**: Nodes, ClusterRoles, PersistentVolumes, CRDs, Namespaces
2. **Namespaced resources across ALL namespaces**: All Deployments cluster-wide
3. **Namespaced resources in specific namespaces**: Secrets only in `compliance: pci` namespaces

### 2.2 Why Per-Rule Scope is Superior

Following Kubernetes `ValidatingWebhookConfiguration` pattern, each rule specifies:
- `scope: Cluster` - for cluster-scoped resources (namespaceSelector ignored)
- `scope: Namespaced` - for namespaced resources (with optional namespaceSelector)

**Advantages**:
- ✅ Single ClusterWatchRule can audit BOTH cluster and namespaced resources
- ✅ Different namespace sets for different resource types in ONE rule
- ✅ Comprehensive audit policies in single manifests
- ✅ Follows established Kubernetes patterns

**Example**:
```yaml
rules:
- scope: Cluster
  resources: [clusterroles]       # All cluster ClusterRoles
  
- scope: Namespaced
  resources: [deployments]        # Deployments in ALL namespaces
  
- scope: Namespaced
  resources: [secrets]
  namespaceSelector:              # Secrets ONLY in PCI namespaces
    matchLabels:
      compliance: pci
```

### 2.3 API Design

See complete specification in [`clusterwatchrule-design.md`](clusterwatchrule-design.md)

**Key Types**:
```go
type ClusterResourceRule struct {
    Operations        []OperationType       `json:"operations,omitempty"`
    APIGroups         []string              `json:"apiGroups,omitempty"`
    APIVersions       []string              `json:"apiVersions,omitempty"`
    Resources         []string              `json:"resources"`
    
    // NEW: Scope field (REQUIRED)
    // +required
    // +kubebuilder:validation:Enum=Cluster;Namespaced
    Scope ResourceScope `json:"scope"`
    
    // NEW: Per-rule namespace selector (OPTIONAL)
    // Only evaluated when Scope is "Namespaced"
    // If omitted = all namespaces
    // +optional
    NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

type ResourceScope string
const (
    ResourceScopeCluster    ResourceScope = "Cluster"
    ResourceScopeNamespaced ResourceScope = "Namespaced"
)
```

### 2.4 Security Model

**RBAC Requirements**: Cluster-admin level permissions required

**Access Control**:
- Referenced GitRepoConfig must have `accessPolicy.allowClusterRules: true`
- GitRepoConfig stays namespaced (for secret reference)
- ClusterWatchRule controller validates access before activating
- Namespace selectors controlled by RBAC (can't bypass via labeling)

### 2.5 Implementation Components

**New Files Required**:
1. `api/v1alpha1/clusterwatchrule_types.go` - CRD with ClusterResourceRule type
2. `internal/controller/clusterwatchrule_controller.go` - Controller with selector validation
3. `internal/controller/clusterwatchrule_controller_test.go` - Tests
4. `config/crd/bases/configbutler.ai_clusterwatchrules.yaml` - Generated CRD
5. `config/rbac/clusterwatchrule_*.yaml` - RBAC manifests

**Modified Files**:
- `internal/rulestore/store.go` - Add `AddOrUpdateClusterWatchRule()` with per-rule matching
- `internal/webhook/event_handler.go` - Handle per-rule namespace matching
- `cmd/main.go` - Register ClusterWatchRule controller

**Critical**: RuleStore must match namespaceSelector for EACH rule independently

---

## Part 3: GitRepoConfig AccessPolicy

### 3.1 Design Requirements

**Purpose**: Control which WatchRules can access a GitRepoConfig across namespaces

**Use Cases**:
1. **Single namespace** (default): Only WatchRules in same namespace
2. **All namespaces**: Any WatchRule in any namespace can use this GitRepoConfig
3. **Selector-based**: Only WatchRules in namespaces matching selector
4. **Cluster rules**: Allow/deny ClusterWatchRule references

### 3.2 Proposed API

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
    name: git-credentials
  
  # NEW: Access control
  accessPolicy:
    namespacedRules:
      mode: SameNamespace  # Options: SameNamespace, AllNamespaces, FromSelector
      namespaceSelector:   # Only when mode=FromSelector
        matchLabels:
          team: platform
    allowClusterRules: false  # Default: false for security
```

### 3.3 CEL Validation

**Rule**: `namespaceSelector` can only be set when `mode` is `FromSelector`

```go
// +kubebuilder:validation:XValidation:rule="!has(self.accessPolicy) || !has(self.accessPolicy.namespacedRules) || !has(self.accessPolicy.namespacedRules.namespaceSelector) || self.accessPolicy.namespacedRules.mode == 'FromSelector'",message="namespaceSelector can only be set when mode is 'FromSelector'"
```

### 3.4 Implementation Components

**New Files Required**:
1. `internal/webhook/v1alpha1/gitrepoconfig_webhook.go` - Validation webhook
2. `internal/webhook/v1alpha1/gitrepoconfig_webhook_test.go` - Tests

**Modified Files**:
- `api/v1alpha1/gitrepoconfig_types.go` - Add AccessPolicy types
- `internal/controller/watchrule_controller.go` - Add access validation
- `internal/controller/clusterwatchrule_controller.go` - Check allowClusterRules

---

## Part 4: E2E Test Enablement

### 4.1 Disabled Tests

**Test 1**: `should create Git commit when IceCreamOrder CRD is installed via ClusterWatchRule`
- **Location**: line 691-693
- **Reason**: Requires ClusterWatchRule implementation
- **Action**: Remove `PIt` wrapper and `Skip()` call

**Test 2**: `should delete Git file when IceCreamOrder CRD is deleted via ClusterWatchRule`
- **Location**: line 1108-1110
- **Reason**: Requires ClusterWatchRule implementation
- **Action**: Remove `PIt` wrapper and `Skip()` call

### 4.2 Prerequisites for Re-enabling

1. ClusterWatchRule CRD implemented
2. ClusterWatchRuleReconciler working
3. RuleStore supports cluster-scoped rules
4. GitRepoConfig allows cluster rules via accessPolicy

### 4.3 Additional Tests Needed

**ClusterWatchRule Tests (Per-Rule Scope)**:
- Node watching test (scope: Cluster)
- ClusterRole watching test (scope: Cluster)
- Cross-namespace namespaced resource test (scope: Namespaced, no selector)
- Namespace selector filtering test (scope: Namespaced with selector)
- Mixed scope rules test (both Cluster and Namespaced in one ClusterWatchRule)

**AccessPolicy Tests (WatchRule)**:
- Cross-namespace WatchRule test (AllNamespaces mode)
- Selector-based access test (FromSelector mode)

---

## Part 5: README Updates

### 5.1 Best Practices Section (New)

**Location**: After "Security Model" section

**Content**:
- ⚠️ **Critical**: Recommend single GitRepoConfig per repository
- Explain why multiple GitRepoConfigs cause issues (duplication, conflicts)
- Show RECOMMENDED pattern (one GitRepoConfig with accessPolicy)
- Show NOT RECOMMENDED pattern (multiple GitRepoConfigs to same repo)

### 5.2 AccessPolicy Documentation (New)

**Location**: After "Configure Repository" section

**Content**:
- Default behavior (SameNamespace)
- AllNamespaces mode example
- FromSelector mode example
- allowClusterRules explanation

### 5.3 ClusterWatchRule Examples (New)

**Location**: After existing WatchRule examples

**Content**:
- **Example 1**: Cluster-scoped resources (scope: Cluster for Nodes, ClusterRoles, CRDs)
- **Example 2**: Namespaced resources in all namespaces (scope: Namespaced without selector)
- **Example 3**: Namespaced resources with namespace filtering (scope: Namespaced with selector)
- **Example 4**: Mixed comprehensive policy (both cluster and namespaced scope in one rule)
- Security warning about cluster-admin permissions
- Requirement for `accessPolicy.allowClusterRules: true` in GitRepoConfig
- **Clarification**: Explain the two different uses of namespaceSelector:
  - ClusterWatchRule's per-rule `namespaceSelector`: Filters which namespaces to watch resources in
  - GitRepoConfig's `accessPolicy.namespaceSelector`: Controls which WatchRules can access the GitRepoConfig

---

## Part 6: Implementation Checklist

See detailed checklist in [`implementation-checklist.md`](implementation-checklist.md)

### Phase 1: ClusterWatchRule (HIGH Priority)

- [ ] Create CRD types
- [ ] Create controller
- [ ] Update RuleStore
- [ ] Create manifests
- [ ] Generate code
- [ ] Write tests (>90% coverage)
- [ ] Validate: `make lint && make test && make test-e2e`

### Phase 2: GitRepoConfig AccessPolicy (HIGH Priority)

- [ ] Update API types
- [ ] Create validation webhook
- [ ] Update controllers
- [ ] Add CEL validation
- [ ] Write tests (>90% coverage)
- [ ] Validate: `make lint && make test`

### Phase 3: E2E Tests (MEDIUM Priority)

- [ ] Re-enable CRD tests
- [ ] Add ClusterWatchRule tests
- [ ] Add accessPolicy tests
- [ ] Validate: `make test-e2e` (requires Docker)

### Phase 4: Documentation (MEDIUM Priority)

- [ ] Update README.md
- [ ] Create ACCESS_POLICY_GUIDE.md
- [ ] Create CLUSTER_RESOURCES.md
- [ ] Update review-refactor-proposal.md

### Phase 5: Final Validation (MANDATORY)

- [ ] `make fmt` - Format code
- [ ] `make generate` - Update generated code
- [ ] `make manifests` - Update CRDs
- [ ] `make vet` - Static analysis
- [ ] **`make lint`** - MUST PASS
- [ ] **`make test`** - MUST PASS (>90% coverage)
- [ ] **`make test-e2e`** - MUST PASS

---

## Part 7: Known Limitations

### Current Limitations

1. **No duplicate GitRepoConfig detection**: System allows multiple GitRepoConfigs pointing to same repository (inefficient but functional)

2. **No migration path**: Since there's no public release yet, breaking changes are acceptable without migration support

3. **Basic conflict resolution**: "Last Writer Wins" strategy may need enhancement for complex scenarios

### Future Enhancements (Post-Implementation)

- Validation webhook to warn about duplicate repositories
- Advanced conflict resolution strategies
- Git submodules support
- Multiple Git remotes support
- Git garbage collection for long-running workers
- Prometheus alerts for common misconfigurations

---

## Part 8: Success Criteria

Implementation is complete when:

✅ **All validation commands pass**:
- `make lint` passes without errors
- `make test` passes with >90% coverage
- `make test-e2e` passes all tests (including re-enabled CRD tests)

✅ **All features implemented**:
- ClusterWatchRule CRD created and working
- GitRepoConfig accessPolicy implemented with CEL validation
- RuleStore supports cluster-scoped rules
- Controllers handle cross-namespace access
- Webhooks validate configurations

✅ **Documentation complete**:
- README.md updated with best practices
- All examples tested and working
- API documentation generated
- Godoc comments present for all exports

✅ **E2E tests passing**:
- Both disabled tests re-enabled and passing
- New ClusterWatchRule tests added and passing
- AccessPolicy tests added and passing

---

## Summary

This implementation plan provides a **complete roadmap** for finishing the GitOps Reverser project:

1. **ClusterWatchRule with Per-Rule Scope** - Enables watching cluster-scoped AND namespaced resources with fine-grained per-rule control (follows Kubernetes ValidatingWebhookConfiguration pattern)
2. **AccessPolicy** - Enables secure cross-namespace GitRepoConfig sharing
3. **E2E Tests** - Re-enables disabled tests and adds comprehensive coverage
4. **Documentation** - Updates README with best practices and examples

**Architectural Highlight**: ClusterWatchRule's per-rule `scope` and `namespaceSelector` fields provide maximum flexibility, allowing administrators to create comprehensive, all-in-one audit policies in single manifests.

All work is tracked in detailed checklists, with clear validation criteria and success metrics. The plan follows the project's development rules and maintains >90% test coverage throughout.

**Estimated Effort**: 
- ClusterWatchRule: 2-3 days
- AccessPolicy: 2-3 days  
- E2E Tests: 1-2 days
- Documentation: 1 day
- **Total: 6-9 days** of focused development

**Next Steps**: Begin with Phase 1 (ClusterWatchRule) as it's the foundation for the disabled e2e tests.

---

## Related Documents

- [`clusterwatchrule-design.md`](clusterwatchrule-design.md) - Detailed ClusterWatchRule API design
- [`accesspolicy-design.md`](accesspolicy-design.md) - Detailed AccessPolicy implementation
- [`implementation-checklist.md`](implementation-checklist.md) - Step-by-step implementation tasks
- [`review-refactor-proposal.md`](review-refactor-proposal.md) - Original architecture design