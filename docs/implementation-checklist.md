# GitOps Reverser: Implementation Checklist

**Status**: Ready for Implementation
**Estimated Effort**: 6-9 days
**Target Version**: v1alpha1

> 📋 **This is your single starting point** for implementing the remaining GitOps Reverser features.
> All design decisions, architecture details, and step-by-step tasks are documented here and in the linked design documents.

---

## 📚 Required Reading (Start Here!)

Before beginning implementation, **you must read these documents** in order:

1. **[Implementation Gap Analysis & Plan](implementation-gap-analysis-and-plan.md)** ⭐ **START HERE**
   - Overview of what's done vs. what needs to be done
   - High-level architecture and design decisions
   - Success criteria and validation requirements
   - Estimated timelines and phases

2. **[ClusterWatchRule Design](clusterwatchrule-design.md)**
   - Complete API specification for cluster-scoped resource watching
   - Controller implementation details
   - Security model and RBAC requirements
   - Examples and usage patterns

3. **[AccessPolicy Design](accesspolicy-design.md)**
   - GitRepoConfig cross-namespace access control
   - Three access modes explained (SameNamespace/AllNamespaces/FromSelector)
   - CEL validation rules and webhook implementation
   - Security implications and best practices

4. **[Original Architecture](review-refactor-proposal.md)** (Reference)
   - Original design document this work is based on
   - Core concepts and requirements
   - Context for architectural decisions

---

## 🎯 What You're Building

### Missing Features (From Gap Analysis)

1. **ClusterWatchRule CRD** - Watch cluster-scoped resources (Nodes, ClusterRoles, CRDs)
2. **GitRepoConfig AccessPolicy** - Secure cross-namespace access control
3. **E2E Test Enablement** - Re-enable 2 disabled tests (requires ClusterWatchRule)
4. **README Best Practices** - Document single GitRepoConfig per repo guidance

### Why This Work Is Needed

- **ClusterWatchRule**: Current WatchRule can only watch namespaced resources. Many critical Kubernetes resources are cluster-scoped (Nodes, CRDs, ClusterRoles).
- **AccessPolicy**: Organizations need to share a single GitRepoConfig across namespaces without creating duplicates.
- **Disabled Tests**: Two E2E tests for CRD watching are currently disabled because they require ClusterWatchRule.
- **Best Practices**: Users need guidance to avoid inefficient configurations.

### Good News! 🎉

The existing codebase is **already prepared** for these features:
- RuleStore has `IsClusterScoped` field ready
- Event handler already detects cluster-scoped resources
- Git operations and sanitization work for all resource types
- Infrastructure is solid and well-tested (>90% coverage)

---

## ⚠️ Critical Requirements

### MANDATORY Validation (Must Pass Before Completion)

```bash
make lint      # MUST PASS - no linting errors
make test      # MUST PASS - >90% test coverage
make test-e2e  # MUST PASS - all tests including re-enabled ones
```

### No Migration Needed

Since there's **no public release yet**:
- No backward compatibility concerns
- No migration scripts required
- Breaking changes are acceptable
- Focus on getting it right, not maintaining old behavior

### Docker Requirement

E2E tests require Docker to run Kind cluster:
```bash
docker info  # Verify Docker is running before `make test-e2e`
```

---

## 📋 Pre-Implementation Setup

- [ ] **Development environment ready**
  - [ ] Go 1.21+ installed (`go version`)
  - [ ] Docker installed and running (`docker info`)
  - [ ] kubectl configured for test cluster (`kubectl version`)
  - [ ] kubebuilder tools available (`kubebuilder version`)
  - [ ] Git configured with SSH keys (`git config --list`)

- [ ] **All design documents read** (see "Required Reading" above)
  - [ ] Read [`implementation-gap-analysis-and-plan.md`](implementation-gap-analysis-and-plan.md) ⭐
  - [ ] Read [`clusterwatchrule-design.md`](clusterwatchrule-design.md)
  - [ ] Read [`accesspolicy-design.md`](accesspolicy-design.md)
  - [ ] Reviewed [`review-refactor-proposal.md`](review-refactor-proposal.md) (original design)

- [ ] **Understand the codebase**
  - [ ] Reviewed current WatchRule implementation ([`api/v1alpha1/watchrule_types.go`](../api/v1alpha1/watchrule_types.go))
  - [ ] Reviewed RuleStore implementation ([`internal/rulestore/store.go`](../internal/rulestore/store.go))
  - [ ] Reviewed event handler ([`internal/webhook/event_handler.go`](../internal/webhook/event_handler.go))
  - [ ] Ran existing tests (`make test`) to understand patterns

- [ ] **Project rules understood**
  - [ ] Read [`.kilocode/rules/implementation-rules.md`](../.kilocode/rules/implementation-rules.md)
  - [ ] Understand >90% coverage requirement
  - [ ] Understand mandatory validation sequence

---

## Phase 1: ClusterWatchRule Implementation (2-3 days)

### Step 1.1: Create CRD API Types

- [ ] Create `api/v1alpha1/clusterwatchrule_types.go`
  ```bash
  touch api/v1alpha1/clusterwatchrule_types.go
  ```

- [ ] Add type definitions (see [`clusterwatchrule-design.md`](clusterwatchrule-design.md) for complete spec):
  - [ ] `ClusterWatchRule` struct with kubebuilder markers
  - [ ] `ClusterWatchRuleSpec` struct
  - [ ] `ClusterWatchRuleStatus` struct
  - [ ] `ClusterWatchRuleList` struct
  - [ ] `NamespacedName` type for GitRepoConfig reference
  - [ ] **`ClusterResourceRule` type** (with `scope` and `namespaceSelector` fields)
  - [ ] **`ResourceScope` enum type** (Cluster/Namespaced constants)
  - [ ] Add godoc comments for all exported types

- [ ] Add kubebuilder markers:
  - [ ] `+kubebuilder:resource:scope=Cluster`
  - [ ] `+kubebuilder:printcolumn` for GitRepoConfig name
  - [ ] `+kubebuilder:printcolumn` for namespace
  - [ ] `+kubebuilder:printcolumn` for Ready status
  - [ ] `+kubebuilder:printcolumn` for Age

- [ ] Add to init() function:
  ```go
  func init() {
      SchemeBuilder.Register(&ClusterWatchRule{}, &ClusterWatchRuleList{})
  }
  ```

- [ ] Generate deep copy methods:
  ```bash
  make generate
  ```

- [ ] Verify compilation:
  ```bash
  go build ./api/v1alpha1
  ```

### Step 1.2: Generate CRD Manifests

- [ ] Generate CRD YAML:
  ```bash
  make manifests
  ```

- [ ] Verify CRD created:
  ```bash
  ls -la config/crd/bases/configbutler.ai_clusterwatchrules.yaml
  ```

- [ ] Review generated CRD:
  ```bash
  cat config/crd/bases/configbutler.ai_clusterwatchrules.yaml
  ```

- [ ] Install CRD in test cluster:
  ```bash
  make install
  ```

- [ ] Verify CRD installed:
  ```bash
  kubectl get crd clusterwatchrules.configbutler.ai
  ```

### Step 1.3: Create Controller

- [ ] Create `internal/controller/clusterwatchrule_controller.go`

- [ ] Implement controller structure:
  - [ ] `ClusterWatchRuleReconciler` struct
  - [ ] Add `Client`, `Scheme`, `RuleStore` fields
  - [ ] Add godoc comment for struct

- [ ] Implement `Reconcile()` method:
  - [ ] Fetch ClusterWatchRule
  - [ ] Handle deletion (return early if not found)
  - [ ] Fetch referenced GitRepoConfig
  - [ ] Validate GitRepoConfig exists
  - [ ] Check `accessPolicy.allowClusterRules`
  - [ ] **Validate namespaceSelector for each Namespaced scope rule**
  - [ ] Update RuleStore with compiled rule (with per-rule scope)
  - [ ] Set status conditions (Ready, GitRepoConfigNotFound, AccessDenied, InvalidSelector)
  - [ ] Return appropriate result

- [ ] Add kubebuilder RBAC markers:
  ```go
  // +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules,verbs=get;list;watch;create;update;patch;delete
  // +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/status,verbs=get;update;patch
  // +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/finalizers,verbs=update
  // +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch
  // +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
  ```
  **Note**: Namespace read access required for per-rule namespaceSelector matching

- [ ] Implement `SetupWithManager()`:
  ```go
  func (r *ClusterWatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
      return ctrl.NewControllerManagedBy(mgr).
          For(&configv1alpha1.ClusterWatchRule{}).
          Complete(r)
  }
  ```

- [ ] Add to `cmd/main.go`:
  ```go
  if err = (&controller.ClusterWatchRuleReconciler{
      Client:    mgr.GetClient(),
      Scheme:    mgr.GetScheme(),
      RuleStore: ruleStore,
  }).SetupWithManager(mgr); err != nil {
      setupLog.Error(err, "unable to create controller", "controller", "ClusterWatchRule")
      os.Exit(1)
  }
  ```

- [ ] Verify compilation:
  ```bash
  go build ./internal/controller
  ```

### Step 1.4: Create Controller Tests

- [ ] Create `internal/controller/clusterwatchrule_controller_test.go`

- [ ] Write test cases:
  - [ ] Test successful reconciliation with valid GitRepoConfig
  - [ ] Test GitRepoConfig not found
  - [ ] Test GitRepoConfig without allowClusterRules
  - [ ] Test GitRepoConfig with allowClusterRules = true
  - [ ] **Test Cluster scope rule validation**
  - [ ] **Test Namespaced scope with all namespaces (no selector)**
  - [ ] **Test Namespaced scope with namespaceSelector**
  - [ ] **Test invalid namespaceSelector for Namespaced rules**
  - [ ] Test RuleStore updates correctly with per-rule scope
  - [ ] Test status condition updates

- [ ] Run tests:
  ```bash
  go test ./internal/controller -v -run TestClusterWatchRule
  ```

- [ ] Check coverage:
  ```bash
  go test ./internal/controller -coverprofile=coverage.out
  go tool cover -html=coverage.out
  ```

- [ ] Ensure >90% coverage for new code

### Step 1.5: Update RuleStore

- [ ] Open `internal/rulestore/store.go`

- [ ] Add `AddOrUpdateClusterWatchRule()` method:
  - [ ] Create CompiledClusterRule structure (separate from WatchRule)
  - [ ] Use empty namespace as key marker for cluster rules
  - [ ] Store GitRepoConfigNamespace from rule spec
  - [ ] **Compile ClusterResourceRules with per-rule scope and namespaceSelector**
  - [ ] Add to cluster rules map

- [ ] **Add `GetMatchingClusterRules()` method** (NEW - separate matching logic):
  - [ ] For each rule, check rule.Scope matches resource scope
  - [ ] For Cluster scope: ignore namespaceSelector, match cluster resources
  - [ ] For Namespaced scope without selector: match all namespaces
  - [ ] For Namespaced scope with selector: evaluate selector against namespace labels
  - [ ] Return all matching ClusterWatchRules

- [ ] Add tests in `internal/rulestore/store_test.go`:
  - [ ] Test AddOrUpdateClusterWatchRule
  - [ ] **Test Cluster scope matching (cluster resources only)**
  - [ ] **Test Namespaced scope without selector (all namespaces)**
  - [ ] **Test Namespaced scope with matching selector**
  - [ ] **Test Namespaced scope with non-matching selector**
  - [ ] **Test mixed Cluster and Namespaced rules in single ClusterWatchRule**

- [ ] Run tests:
  ```bash
  go test ./internal/rulestore -v
  ```

### Step 1.6: Create RBAC Manifests

- [ ] Create `config/rbac/clusterwatchrule_role.yaml`:
  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRole
  metadata:
    name: gitops-reverser-clusterwatchrule-manager
  rules:
  - apiGroups: [configbutler.ai]
    resources: [clusterwatchrules]
    verbs: [create, delete, get, list, patch, update, watch]
  - apiGroups: [configbutler.ai]
    resources: [clusterwatchrules/status]
    verbs: [get, patch, update]
  - apiGroups: [configbutler.ai]
    resources: [clusterwatchrules/finalizers]
    verbs: [update]
  ```

- [ ] Create editor and viewer roles:
  - [ ] `config/rbac/clusterwatchrule_editor_role.yaml`
  - [ ] `config/rbac/clusterwatchrule_viewer_role.yaml`

- [ ] Update `config/rbac/kustomization.yaml` to include new files

- [ ] Generate RBAC:
  ```bash
  make manifests
  ```

### Step 1.7: Create Sample Manifests

- [ ] Create `config/samples/configbutler.ai_v1alpha1_clusterwatchrule.yaml` (with per-rule scope):
  ```yaml
  apiVersion: configbutler.ai/v1alpha1
  kind: ClusterWatchRule
  metadata:
    name: clusterwatchrule-sample
  spec:
    gitRepoConfigRef:
      name: gitrepoconfig-sample
      namespace: gitops-reverser-system
    rules:
    # Cluster-scoped resources
    - scope: Cluster
      operations: [CREATE, UPDATE, DELETE]
      apiGroups: [""]
      resources: [nodes]
    
    # Namespaced resources in all namespaces
    - scope: Namespaced
      apiGroups: [apps]
      resources: [deployments]
    
    # Namespaced resources with namespace selector
    - scope: Namespaced
      apiGroups: [""]
      resources: [secrets]
      namespaceSelector:
        matchLabels:
          compliance: pci
  ```

- [ ] Update `config/samples/kustomization.yaml`

### Step 1.8: Phase 1 Validation

- [ ] Run all validation commands:
  ```bash
  make fmt
  make generate
  make manifests
  make vet
  make lint
  make test
  ```

- [ ] All commands must pass without errors

- [ ] Check test coverage:
  ```bash
  go test ./... -coverprofile=coverage.out
  go tool cover -func=coverage.out | grep total
  ```

- [ ] Ensure >90% coverage maintained

---

## Phase 2: GitRepoConfig AccessPolicy (2-3 days)

### Step 2.1: Update API Types

- [ ] Open `api/v1alpha1/gitrepoconfig_types.go`

- [ ] Add new types at end of file (before init):
  - [ ] `AccessPolicy` struct
  - [ ] `NamespacedRulesPolicy` struct
  - [ ] `AccessPolicyMode` type (string enum)
  - [ ] Constants for mode values
  - [ ] Add godoc comments

- [ ] Add `AccessPolicy` field to `GitRepoConfigSpec`:
  ```go
  // AccessPolicy controls which WatchRules can reference this GitRepoConfig.
  // +optional
  AccessPolicy *AccessPolicy `json:"accessPolicy,omitempty"`
  ```

- [ ] Add CEL validation rule to `GitRepoConfigSpec`:
  ```go
  // +kubebuilder:validation:XValidation:rule="!has(self.accessPolicy) || !has(self.accessPolicy.namespacedRules) || !has(self.accessPolicy.namespacedRules.namespaceSelector) || self.accessPolicy.namespacedRules.mode == 'FromSelector'",message="namespaceSelector can only be set when mode is 'FromSelector'"
  ```

- [ ] Add default value markers:
  ```go
  // +kubebuilder:default=SameNamespace
  Mode AccessPolicyMode `json:"mode,omitempty"`
  
  // +kubebuilder:default=false
  AllowClusterRules bool `json:"allowClusterRules,omitempty"`
  ```

- [ ] Generate code:
  ```bash
  make generate
  ```

- [ ] Generate manifests:
  ```bash
  make manifests
  ```

- [ ] Verify CRD updated:
  ```bash
  cat config/crd/bases/configbutler.ai_gitrepoconfigs.yaml | grep -A 20 accessPolicy
  ```

### Step 2.2: Create Validation Webhook

- [ ] Create directory:
  ```bash
  mkdir -p internal/webhook/v1alpha1
  ```

- [ ] Create `internal/webhook/v1alpha1/gitrepoconfig_webhook.go`

- [ ] Implement webhook interface:
  - [ ] `SetupWebhookWithManager()` function
  - [ ] `ValidateCreate()` method
  - [ ] `ValidateUpdate()` method  
  - [ ] `ValidateDelete()` method (no-op)
  - [ ] `validateAccessPolicy()` helper method

- [ ] Add validation logic in `validateAccessPolicy()`:
  - [ ] Check selector requires FromSelector mode
  - [ ] Check FromSelector mode requires selector
  - [ ] Validate label selector is well-formed

- [ ] Add kubebuilder webhook marker:
  ```go
  // +kubebuilder:webhook:path=/validate-configbutler-ai-v1alpha1-gitrepoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=configbutler.ai,resources=gitrepoconfigs,verbs=create;update,versions=v1alpha1,name=vgitrepoconfig.kb.io,admissionReviewVersions=v1
  ```

- [ ] Register webhook in `cmd/main.go`:
  ```go
  if err = (&configv1alpha1.GitRepoConfig{}).SetupWebhookWithManager(mgr); err != nil {
      setupLog.Error(err, "unable to create webhook", "webhook", "GitRepoConfig")
      os.Exit(1)
  }
  ```

- [ ] Generate webhook configuration:
  ```bash
  make manifests
  ```

### Step 2.3: Create Webhook Tests

- [ ] Create `internal/webhook/v1alpha1/gitrepoconfig_webhook_test.go`

- [ ] Write test cases:
  - [ ] Test valid: no access policy
  - [ ] Test valid: AllNamespaces mode
  - [ ] Test valid: FromSelector with selector
  - [ ] Test invalid: selector without FromSelector
  - [ ] Test invalid: FromSelector without selector
  - [ ] Test invalid: malformed label selector

- [ ] Create test suite setup:
  - [ ] `internal/webhook/v1alpha1/webhook_suite_test.go`

- [ ] Run tests:
  ```bash
  go test ./internal/webhook/v1alpha1 -v
  ```

- [ ] Ensure >90% coverage

### Step 2.4: Update WatchRule Controller

- [ ] Open `internal/controller/watchrule_controller.go`

- [ ] Add `validateGitRepoConfigAccess()` method:
  - [ ] Handle nil accessPolicy (default SameNamespace)
  - [ ] Handle nil namespacedRules (default SameNamespace)
  - [ ] Implement SameNamespace check
  - [ ] Implement AllNamespaces (always allow)
  - [ ] Implement FromSelector with namespace lookup
  - [ ] Return clear error messages

- [ ] Call validation in `Reconcile()`:
  ```go
  if err := r.validateGitRepoConfigAccess(ctx, watchRule); err != nil {
      // Set AccessDenied condition
      return ctrl.Result{}, err
  }
  ```

- [ ] Add namespace read RBAC:
  ```go
  // +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
  ```

- [ ] Update tests in `watchrule_controller_test.go`:
  - [ ] Test same namespace access (allowed)
  - [ ] Test cross-namespace denied (no policy)
  - [ ] Test AllNamespaces mode (allowed)
  - [ ] Test FromSelector matching (allowed)
  - [ ] Test FromSelector not matching (denied)

- [ ] Run tests:
  ```bash
  go test ./internal/controller -v -run TestWatchRule
  ```

### Step 2.5: Update ClusterWatchRule Controller

- [ ] Open `internal/controller/clusterwatchrule_controller.go`

- [ ] Add allowClusterRules check in `Reconcile()`:
  ```go
  if gitRepoConfig.Spec.AccessPolicy != nil {
      if !gitRepoConfig.Spec.AccessPolicy.AllowClusterRules {
          // Set AccessDenied condition
          return ctrl.Result{}, fmt.Errorf("GitRepoConfig does not allow cluster rules")
      }
  } else {
      // Default: deny cluster rules
      return ctrl.Result{}, fmt.Errorf("GitRepoConfig must explicitly allow cluster rules")
  }
  ```

- [ ] Update tests:
  - [ ] Test with allowClusterRules = true (allowed)
  - [ ] Test with allowClusterRules = false (denied)
  - [ ] Test with no accessPolicy (denied)

### Step 2.6: Create Sample Manifests

- [ ] Create `config/samples/configbutler.ai_v1alpha1_gitrepoconfig_with_accesspolicy.yaml`:
  ```yaml
  apiVersion: configbutler.ai/v1alpha1
  kind: GitRepoConfig
  metadata:
    name: gitrepoconfig-with-policy
    namespace: gitops-reverser-system
  spec:
    repoUrl: "git@github.com:example/audit.git"
    branch: "main"
    secretRef:
      name: git-credentials
    accessPolicy:
      namespacedRules:
        mode: AllNamespaces
      allowClusterRules: true
  ```

### Step 2.7: Phase 2 Validation

- [ ] Run validation commands:
  ```bash
  make fmt
  make generate  
  make manifests
  make vet
  make lint
  make test
  ```

- [ ] Test CEL validation manually:
  ```bash
  # Apply invalid config (should fail)
  kubectl apply -f test-invalid-config.yaml
  # Should see: "namespaceSelector can only be set when mode is 'FromSelector'"
  ```

- [ ] All commands must pass

---

## Phase 3: E2E Test Enablement (1-2 days)

### Step 3.1: Re-enable Disabled Tests

- [ ] Open `test/e2e/e2e_test.go`

- [ ] Find line 691-693 (CRD installation test):
  - [ ] Change `PIt(` to `It(`
  - [ ] Remove `Skip("CRD watching requires ClusterWatchRule (not yet implemented)")`

- [ ] Find line 1108-1110 (CRD deletion test):
  - [ ] Change `PIt(` to `It(`
  - [ ] Remove `Skip("CRD watching requires ClusterWatchRule (not yet implemented)")`

- [ ] Save file

### Step 3.2: Create Test Templates

- [ ] Create `test/e2e/templates/clusterwatchrule.tmpl` (with scope field):
  ```yaml
  apiVersion: configbutler.ai/v1alpha1
  kind: ClusterWatchRule
  metadata:
    name: {{ .Name }}
  spec:
    gitRepoConfigRef:
      name: {{ .GitRepoConfigRef }}
      namespace: {{ .Namespace }}
    rules:
    {{ range .Rules }}
    - scope: {{ .Scope }}
      operations: {{ .Operations }}
      apiGroups: {{ .APIGroups }}
      resources: {{ .Resources }}
      {{- if .NamespaceSelector }}
      namespaceSelector:
        {{ .NamespaceSelector | toYaml | indent 4 }}
      {{- end }}
    {{ end }}
  ```

- [ ] Create `test/e2e/templates/clusterwatchrule-crd.tmpl`

- [ ] Create `test/e2e/templates/gitrepoconfig-with-cluster-access.tmpl`

### Step 3.3: Add Test Helpers

- [ ] Add to `test/e2e/helpers.go`:
  - [ ] `createClusterWatchRule()` function
  - [ ] `createGitRepoConfigWithAccessPolicy()` function
  - [ ] `verifyClusterWatchRuleStatus()` function
  - [ ] `cleanupClusterWatchRule()` function

### Step 3.4: Update Existing Tests

- [ ] Update CRD installation test (line 691):
  - [ ] Create GitRepoConfig with `allowClusterRules: true`
  - [ ] Create ClusterWatchRule with **`scope: Cluster`** for CRDs
  - [ ] Install IceCreamOrder CRD
  - [ ] Verify CRD YAML in Git (NO namespace in path - cluster resource)
  - [ ] Check file path: `apiextensions.k8s.io/v1/customresourcedefinitions/icecreamorders.shop.example.com.yaml`

- [ ] Update CRD deletion test (line 1108):
  - [ ] Similar to installation test
  - [ ] Delete CRD
  - [ ] Verify file removed from Git
  - [ ] Check git log for DELETE

### Step 3.5: Add New E2E Tests

- [ ] Add Node watching test (Cluster scope):
  ```go
  It("should watch Node resources via ClusterWatchRule with Cluster scope", func() {
      // Create ClusterWatchRule with scope: Cluster for nodes
      // Label a node (if possible in test env)
      // Verify node YAML in Git (no namespace in path)
  })
  ```

- [ ] Add cross-namespace watching test (Namespaced scope, no selector):
  ```go
  It("should watch namespaced resources in ALL namespaces via ClusterWatchRule", func() {
      // Create ClusterWatchRule with scope: Namespaced, no namespaceSelector
      // Create ConfigMaps in multiple namespaces
      // Verify ALL ConfigMaps appear in Git (from all namespaces)
  })
  ```

- [ ] Add namespace selector test (Namespaced scope with selector):
  ```go
  It("should filter namespaced resources by per-rule namespaceSelector", func() {
      // Create namespaces with different labels (prod, dev)
      // Create ClusterWatchRule with scope: Namespaced + selector for prod
      // Create resources in both namespaces
      // Verify ONLY prod namespace resources in Git
  })
  ```

- [ ] Add WatchRule cross-namespace access test (AccessPolicy):
  ```go
  It("should allow cross-namespace WatchRule with AllNamespaces policy", func() {
      // Create GitRepoConfig with accessPolicy.AllNamespaces in namespace-a
      // Create WatchRule in namespace-b referencing it
      // Verify WatchRule is Ready (cross-namespace access allowed)
      // Create resource, verify Git commit
  })
  ```

- [ ] Add selector-based WatchRule access test (AccessPolicy):
  ```go
  It("should allow FromSelector access for matching namespaces", func() {
      // Create namespace with specific labels
      // Create GitRepoConfig with accessPolicy.FromSelector
      // Create WatchRule in matching namespace (should work)
      // Create WatchRule in non-matching namespace (should fail with AccessDenied)
  })
  ```

### Step 3.6: Phase 3 Validation

- [ ] Check Docker is running:
  ```bash
  docker info
  ```

- [ ] Run e2e tests:
  ```bash
  make test-e2e
  ```

- [ ] All tests must pass, including re-enabled CRD tests

- [ ] Review test output for any warnings or issues

---

## Phase 4: Documentation Updates (1 day)

### Step 4.1: Update README.md

- [ ] Open `README.md`

- [ ] Add "Best Practices" section after "Security Model" (line 225):
  - [ ] Add warning about single GitRepoConfig per repo
  - [ ] Show RECOMMENDED pattern with accessPolicy
  - [ ] Show NOT RECOMMENDED pattern with duplicates
  - [ ] Explain why duplicates cause issues

- [ ] Add "Configure Access Policy" section after "Configure Repository" (line 65):
  - [ ] Example: Allow All Namespaces
  - [ ] Example: Selector-Based Access
  - [ ] Example: Allow Cluster Resources

- [ ] Add ClusterWatchRule examples after "Example 4" (line 134):
  - [ ] **Example 1**: Watch cluster-scoped resources (scope: Cluster)
  - [ ] **Example 2**: Watch namespaced resources across all namespaces (scope: Namespaced, no selector)
  - [ ] **Example 3**: Watch namespaced resources in filtered namespaces (scope: Namespaced with selector)
  - [ ] **Example 4**: Mixed scope (both Cluster and Namespaced in one ClusterWatchRule)
  - [ ] Security warning about cluster-admin permissions
  - [ ] Note about `accessPolicy.allowClusterRules: true` requirement in GitRepoConfig
  - [ ] **Clarify two different namespacSelector uses**: ClusterWatchRule's (resource filtering) vs GitRepoConfig's (access control)

- [ ] Update "Cluster-Scoped Resources" section (line 220):
  - [ ] Remove "not yet implemented" warning
  - [ ] Add link to ClusterWatchRule examples

### Step 4.2: Create New Documentation

- [ ] Create `docs/ACCESS_POLICY_GUIDE.md`:
  - [ ] Comprehensive guide to accessPolicy
  - [ ] All three modes explained
  - [ ] Security considerations
  - [ ] Real-world examples
  - [ ] Troubleshooting guide

- [ ] Create `docs/CLUSTER_RESOURCES.md`:
  - [ ] Guide to cluster-scoped resources
  - [ ] ClusterWatchRule usage patterns
  - [ ] RBAC requirements
  - [ ] Security best practices

### Step 4.3: Update Design Documents

- [ ] Update `docs/review-refactor-proposal.md`:
  - [ ] Mark ClusterWatchRule as ✅ Implemented
  - [ ] Mark AccessPolicy as ✅ Implemented
  - [ ] Update testing section

### Step 4.4: Update Samples

- [ ] Verify all sample YAMLs in `config/samples/` are up to date

- [ ] Test each sample manually:
  ```bash
  kubectl apply -f config/samples/
  ```

- [ ] Verify all samples work correctly

---

## Phase 5: Final Validation (MANDATORY)

### Step 5.1: Pre-Completion Validation

**CRITICAL**: All these commands MUST pass before completion

- [ ] Format code:
  ```bash
  make fmt
  ```

- [ ] Update generated code:
  ```bash
  make generate
  ```

- [ ] Update CRDs:
  ```bash
  make manifests
  ```

- [ ] Run go vet:
  ```bash
  make vet
  ```

- [ ] **Run linter (MANDATORY)**:
  ```bash
  make lint
  ```
  - If fails, try auto-fix first:
    ```bash
    make lint-fix
    ```

- [ ] **Run unit tests (MANDATORY)**:
  ```bash
  make test
  ```
  - Must pass with >90% coverage

- [ ] Check Docker is running:
  ```bash
  docker info
  ```
  - If not running, start Docker daemon

- [ ] **Run e2e tests (MANDATORY)**:
  ```bash
  make test-e2e
  ```
  - All tests must pass
  - Including re-enabled CRD tests

### Step 5.2: Coverage Verification

- [ ] Generate coverage report:
  ```bash
  go test ./... -coverprofile=coverage.out
  go tool cover -html=coverage.out -o coverage.html
  ```

- [ ] Check overall coverage:
  ```bash
  go tool cover -func=coverage.out | grep total
  ```

- [ ] Verify >90% coverage for new code:
  - [ ] `api/v1alpha1/clusterwatchrule_types.go`
  - [ ] `internal/controller/clusterwatchrule_controller.go`
  - [ ] `internal/webhook/v1alpha1/gitrepoconfig_webhook.go`
  - [ ] `internal/rulestore/store.go` (new methods)

### Step 5.3: Documentation Review

- [ ] All godoc comments present for:
  - [ ] ClusterWatchRule types
  - [ ] AccessPolicy types
  - [ ] New controller methods
  - [ ] New webhook methods

- [ ] README.md updated and accurate:
  - [ ] Best practices section added
  - [ ] AccessPolicy examples added
  - [ ] ClusterWatchRule examples added
  - [ ] No broken links

- [ ] All design docs complete:
  - [ ] `implementation-gap-analysis-and-plan.md`
  - [ ] `clusterwatchrule-design.md`
  - [ ] `accesspolicy-design.md`

- [ ] Example YAMLs tested:
  - [ ] All samples in `config/samples/` work
  - [ ] All examples in README work

### Step 5.4: Integration Testing

- [ ] Deploy to test cluster:
  ```bash
  make deploy IMG=test-registry/gitops-reverser:test
  ```

- [ ] Create test resources:
  - [ ] GitRepoConfig with accessPolicy
  - [ ] ClusterWatchRule
  - [ ] WatchRule in different namespace
  - [ ] Test cluster resource (e.g., create namespace)
  - [ ] Test namespaced resource (e.g., configmap)

- [ ] Verify Git commits:
  - [ ] Check cluster resource file in Git
  - [ ] Check namespaced resource file in Git
  - [ ] Verify paths are correct
  - [ ] Verify content is sanitized

- [ ] Test access control:
  - [ ] Verify cross-namespace access works with AllNamespaces
  - [ ] Verify access denied without policy
  - [ ] Verify ClusterWatchRule rejected without allowClusterRules

### Step 5.5: Security Review

- [ ] RBAC permissions appropriate:
  - [ ] ClusterWatchRule requires cluster-admin
  - [ ] WatchRule limited to namespace
  - [ ] No excessive permissions granted

- [ ] Access control working:
  - [ ] Default deny (SameNamespace) enforced
  - [ ] AllowClusterRules defaults to false
  - [ ] CEL validation prevents invalid configs

- [ ] Audit trail complete:
  - [ ] All changes logged with user info
  - [ ] Timestamps present in commits
  - [ ] Operations recorded correctly

### Step 5.6: Performance Check

- [ ] Monitor resource usage:
  ```bash
  kubectl top pods -n gitops-reverser-system
  ```

- [ ] Check for memory leaks:
  - [ ] Create many resources
  - [ ] Monitor memory over time
  - [ ] Should remain stable

- [ ] Check queue performance:
  - [ ] Verify events processed promptly
  - [ ] No growing backlog in metrics

---

## Completion Criteria

Implementation is complete when **ALL** of the following are true:

✅ **Code Quality**
- [ ] All validation commands pass without errors
- [ ] Test coverage >90% for all new code
- [ ] No linter warnings or errors
- [ ] Godoc comments for all exports

✅ **Functionality**
- [ ] ClusterWatchRule CRD created and working
- [ ] GitRepoConfig accessPolicy implemented
- [ ] CEL validation working
- [ ] Controllers handle all scenarios
- [ ] Webhooks validate correctly

✅ **Testing**
- [ ] All unit tests passing
- [ ] All e2e tests passing (including re-enabled CRD tests)
- [ ] New tests added for new features
- [ ] Integration testing successful

✅ **Documentation**
- [ ] README.md updated completely
- [ ] Best practices documented
- [ ] All examples tested and working
- [ ] Design docs complete

✅ **Security**
- [ ] Default deny behavior enforced
- [ ] RBAC configured correctly
- [ ] Access control validated
- [ ] Audit trail working

---

## Rollback Plan

If issues are discovered:

1. **Minor Issues**: Fix and re-validate
2. **Major Issues**: 
   - Revert problematic commits
   - Address issues in feature branch
   - Re-test completely before merge

## Post-Implementation Tasks

After completion:

- [ ] Update CHANGELOG.md with new features
- [ ] Tag release (if applicable)
- [ ] Update project README with current status
- [ ] Archive this checklist for reference
- [ ] Celebrate successful implementation! 🎉

---

**Remember**: Quality over speed. It's better to take extra time to ensure >90% coverage and passing tests than to rush through and leave technical debt.