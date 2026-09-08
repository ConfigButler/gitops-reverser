// SPDX-License-Identifier: Apache-2.0

/*
Package rulestore manages the in-memory cache of compiled WatchRule configurations.
It provides efficient lookup and matching of Kubernetes resources against active watch rules.
*/
package rulestore

import (
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// CompiledRule represents a fully processed WatchRule, ready for quick lookups.
type CompiledRule struct {
	// Source is the NamespacedName of the WatchRule CR.
	Source types.NamespacedName

	// GitTarget reference (for event routing)
	GitTargetRef       string
	GitTargetNamespace string

	// Resolved values (from GitTarget)
	GitProviderRef       string
	GitProviderNamespace string
	Branch               string
	Path                 string

	// IsClusterScoped indicates if this rule watches cluster-scoped resources.
	// Always false for WatchRule (namespace-scoped).
	IsClusterScoped bool
	// ResourceRules contains the compiled resource matching rules.
	ResourceRules []CompiledResourceRule
}

// compiledSelector is the TYPE filter both rule kinds compile to: the operations, groups, versions
// and resource plurals an item selects, with nothing about where the object lives.
//
// It is one type because the answer has to be one answer. The namespaced and cluster-scoped rules
// carried byte-identical copies of all four checks, so a fix to how a wildcard or a subresource
// pattern is read landed on one kind and silently not the other — and the two kinds are routed by
// different call paths, so nothing failed loudly when they disagreed. Scope is the only axis the
// two rules genuinely differ on, and it stays outside: see CompiledResourceRule.SourceNamespaces.
type compiledSelector struct {
	// Operations specifies which operations trigger this rule.
	Operations []configv1alpha3.OperationType
	// APIGroups specifies which API groups this rule matches.
	APIGroups []string
	// APIVersions specifies which API versions this rule matches.
	APIVersions []string
	// Resources specifies which resource types this rule matches.
	Resources []string
}

// CompiledResourceRule represents a single resource matching rule with all its filters.
type CompiledResourceRule struct {
	compiledSelector

	// SourceNamespaces is this item's RESOLVED source-namespace set IN THE SOURCE CLUSTER —
	// spec.rules[i].sourceNamespace fully expanded to concrete names at compile time. Neither a
	// wildcard nor a policy reference ever survives into the data plane.
	//
	// It is per item, and separate from Source.Namespace, because the two are genuinely different
	// namespaces in (potentially) different clusters: Source names the WatchRule OBJECT in the
	// control plane, while these name the namespaces whose objects are mirrored. They coincide
	// only for a legacy item. Every watch-planning consumer — the watched-type selection, the
	// stream roll-up, and the fingerprint that decides whether that table is re-projected — must
	// read THIS field; reading Source.Namespace instead yields a stale watch, not an error.
	//
	// An EMPTY set is a legitimate resolved answer for a wildcard whose policy currently admits
	// nothing: the item watches nothing. It is never a stand-in for "could not resolve" — an
	// unevaluatable policy stops the rule from compiling at all, because an empty set that reached
	// here would be the input to a resync sweep.
	SourceNamespaces []string
}

// CompiledClusterRule represents a fully processed ClusterWatchRule, ready for quick lookups.
type CompiledClusterRule struct {
	// Source is the NamespacedName of the ClusterWatchRule CR (namespace will be empty).
	Source types.NamespacedName

	// GitTarget reference (for event routing)
	GitTargetRef       string
	GitTargetNamespace string

	// Resolved values (from GitTarget)
	GitProviderRef       string
	GitProviderNamespace string
	Branch               string
	Path                 string

	// Rules contains the compiled cluster resource rules with per-rule scope.
	Rules []CompiledClusterResourceRule
}

// CompiledClusterResourceRule represents a single cluster resource rule.
//
// It carries no scope: a ClusterWatchRule is cluster-scope-only, so resolution always matches
// cluster-scoped records and a stored scope other than Cluster is refused at compile time. Keeping
// a scope field here would let a pruned or absent value widen a stream, which is the failure the
// narrowing exists to prevent.
type CompiledClusterResourceRule struct {
	compiledSelector
}

// RuleStore holds the in-memory representation of all active watch rules.
// It is safe for concurrent use.
type RuleStore struct {
	mu           sync.RWMutex
	rules        map[types.NamespacedName]CompiledRule
	clusterRules map[types.NamespacedName]CompiledClusterRule
	ready        bool
}

// NewStore creates a new, empty RuleStore.
func NewStore() *RuleStore {
	return &RuleStore{
		rules:        make(map[types.NamespacedName]CompiledRule),
		clusterRules: make(map[types.NamespacedName]CompiledClusterRule),
	}
}

// AddOrUpdateWatchRule adds or updates a WatchRule with a resolved target from GitTarget.
// The chain is: WatchRule -> GitTarget -> GitProvider
//
// The whole compiled rule — including every item's resolved source-namespace set — is replaced
// ATOMICALLY. Nothing per-item survives a spec change, which is what lets rule items have no stable
// API identity: no state outlives the spec that produced it, so a reorder cannot make one item
// inherit another's grant.
//
// Parameters:
//   - rule: the WatchRule to add or update
//   - sourceNamespaces: the resolved source-namespace set PER rule item, index-aligned with
//     rule.Spec.Rules. A shorter slice leaves the remaining items with no namespaces, which is a
//     compile bug rather than a scope: callers must resolve every item (see
//     authz.ResolveWatchRuleSourceScope).
//   - gitTargetName: the name of the GitTarget
//   - gitTargetNamespace: the namespace containing the GitTarget
//   - gitProviderName: the name of the resolved GitProvider (from GitTarget.Spec.Provider)
//   - gitProviderNamespace: the namespace containing the resolved GitProvider
//   - branch: the Git branch to write to (from GitTarget.Spec.Branch)
//   - path: POSIX-like relative path prefix for writes (from GitTarget.Spec.Path, sanitized upstream)
func (s *RuleStore) AddOrUpdateWatchRule(
	rule configv1alpha3.WatchRule,
	sourceNamespaces [][]string,
	gitTargetName string,
	gitTargetNamespace string,
	gitProviderName string,
	gitProviderNamespace string,
	branch string,
	path string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{
		Name:      rule.Name,
		Namespace: rule.Namespace,
	}

	compiled := CompiledRule{
		Source:               key,
		GitTargetRef:         gitTargetName,
		GitTargetNamespace:   gitTargetNamespace,
		GitProviderRef:       gitProviderName,
		GitProviderNamespace: gitProviderNamespace,
		Branch:               branch,
		Path:                 path,
		IsClusterScoped:      false,
		ResourceRules:        make([]CompiledResourceRule, 0, len(rule.Spec.Rules)),
	}

	for i, r := range rule.Spec.Rules {
		var namespaces []string
		if i < len(sourceNamespaces) {
			namespaces = append([]string(nil), sourceNamespaces[i]...)
		}
		compiled.ResourceRules = append(compiled.ResourceRules, CompiledResourceRule{
			compiledSelector: compiledSelector{
				Operations:  r.Operations,
				APIGroups:   r.APIGroups,
				APIVersions: r.APIVersions,
				Resources:   r.Resources,
			},
			SourceNamespaces: namespaces,
		})
	}

	s.rules[key] = compiled
}

// GetWatchRule returns a compiled WatchRule by key, and whether it is compiled at all.
//
// The status roll-up reads it because a WatchRule's watched namespaces can no longer be derived
// from its spec: a "*" item's set exists only after resolution, so a roll-up computed from the spec
// would look for streams under keys that were never opened and report a healthy wildcard rule as
// permanently not-ready.
func (s *RuleStore) GetWatchRule(key types.NamespacedName) (CompiledRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rule, ok := s.rules[key]
	if !ok {
		return CompiledRule{}, false
	}
	return deepCopyCompiledRule(rule), true
}

// AddOrUpdateClusterWatchRule adds or updates a ClusterWatchRule with a resolved target from GitTarget.
// The chain is: ClusterWatchRule -> GitTarget -> GitProvider
// Parameters:
//   - rule: the ClusterWatchRule to add or update
//   - gitTargetName: the name of the GitTarget
//   - gitTargetNamespace: the namespace containing the GitTarget
//   - gitProviderName: the name of the resolved GitProvider (from GitTarget.Spec.Provider)
//   - gitProviderNamespace: the namespace containing the resolved GitProvider
//   - branch: the Git branch to write to (from GitTarget.Spec.Branch)
//   - path: POSIX-like relative path prefix for writes (from GitTarget.Spec.Path, sanitized upstream)
func (s *RuleStore) AddOrUpdateClusterWatchRule(
	rule configv1alpha3.ClusterWatchRule,
	gitTargetName string,
	gitTargetNamespace string,
	gitProviderName string,
	gitProviderNamespace string,
	branch string,
	path string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{
		Name:      rule.Name,
		Namespace: "", // cluster-scoped
	}

	compiled := CompiledClusterRule{
		Source:               key,
		GitTargetRef:         gitTargetName,
		GitTargetNamespace:   gitTargetNamespace,
		GitProviderRef:       gitProviderName,
		GitProviderNamespace: gitProviderNamespace,
		Branch:               branch,
		Path:                 path,
		Rules:                make([]CompiledClusterResourceRule, 0, len(rule.Spec.Rules)),
	}

	for _, r := range rule.Spec.Rules {
		compiled.Rules = append(compiled.Rules, CompiledClusterResourceRule{
			compiledSelector: compiledSelector{
				Operations:  r.Operations,
				APIGroups:   r.APIGroups,
				APIVersions: r.APIVersions,
				Resources:   r.Resources,
			},
		})
	}

	s.clusterRules[key] = compiled
}

// Delete removes a rule from the store.
func (s *RuleStore) Delete(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, key)
}

// DeleteClusterWatchRule removes a ClusterWatchRule from the store.
func (s *RuleStore) DeleteClusterWatchRule(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clusterRules, key)
}

// MarkReady records that initial WatchRule and ClusterWatchRule bootstrap has completed.
func (s *RuleStore) MarkReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = true
}

// IsReady reports whether the store has completed initial rule bootstrap.
func (s *RuleStore) IsReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// GetMatchingRules returns all namespaced WatchRules that match the given resource.
// For namespaced resources, callers should provide an object carrying the event namespace
// so namespaced WatchRules only match objects from their own namespace.
// Parameters:
//   - obj: The Kubernetes object to match; its namespace is used for WatchRule filtering
//   - resourcePlural: The plural form of the resource (e.g., "pods", "deployments")
//   - operation: The operation type (CREATE, UPDATE, DELETE)
//   - apiGroup: The API group of the resource (empty string for core API)
//   - apiVersion: The API version of the resource
//   - isClusterScoped: Whether the resource is cluster-scoped
func (s *RuleStore) GetMatchingRules(
	obj client.Object,
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
) []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	eventNamespace := ""
	if obj != nil {
		eventNamespace = obj.GetNamespace()
	}

	var matchingRules []CompiledRule
	for _, rule := range s.rules {
		// First check: Does rule scope match resource scope?
		if rule.IsClusterScoped != isClusterScoped {
			continue // WatchRule can't match cluster resources
		}

		if rule.matches(eventNamespace, resourcePlural, operation, apiGroup, apiVersion) {
			matchingRules = append(matchingRules, rule)
		}
	}
	return matchingRules
}

// GetMatchingClusterRules returns ClusterWatchRules matching the resource.
// This handles both cluster-scoped and namespaced resources with per-rule scope matching.
// Parameters:
//   - resourcePlural: The plural form of the resource (e.g., "nodes", "pods")
//   - operation: The operation type (CREATE, UPDATE, DELETE)
//   - apiGroup: The API group of the resource (empty string for core API)
//   - apiVersion: The API version of the resource
//   - isClusterScoped: Whether the resource is cluster-scoped
//   - namespaceLabels: Labels of the namespace (ignored in simplified MVP)
func (s *RuleStore) GetMatchingClusterRules(
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
	_ map[string]string,
) []CompiledClusterRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchingRules []CompiledClusterRule
	for _, clusterRule := range s.clusterRules {
		if s.clusterRuleMatches(
			clusterRule,
			resourcePlural,
			operation,
			apiGroup,
			apiVersion,
			isClusterScoped,
		) {
			matchingRules = append(matchingRules, clusterRule)
		}
	}
	return matchingRules
}

// clusterRuleMatches checks if a cluster rule matches the given criteria.
//
// A ClusterWatchRule is cluster-scope-only, so a NAMESPACED object never matches one — whatever a
// stored (or pruned) scope value might say. Keying on the object's discovered scope rather than on
// the rule's stored one is the third enforcement point: even if admission and compile were both
// bypassed, resolution cannot widen a stream.
func (s *RuleStore) clusterRuleMatches(
	clusterRule CompiledClusterRule,
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
) bool {
	if !isClusterScoped {
		return false
	}
	for _, rule := range clusterRule.Rules {
		if rule.matchesType(resourcePlural, operation, apiGroup, apiVersion) {
			return true
		}
	}
	return false
}

// matches checks if a single rule matches the given filters.
//
// The namespace is matched PER ITEM, not once per rule: each item resolved its own source-namespace
// set, so a rule can legitimately follow configmaps in its own namespace and secrets in another.
// Matching the rule object's namespace instead would drop every event an override asked for.
func (r *CompiledRule) matches(
	eventNamespace string,
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	// Check if any resource rule matches (logical OR)
	for _, rule := range r.ResourceRules {
		if rule.matches(eventNamespace, resourcePlural, operation, apiGroup, apiVersion) {
			return true
		}
	}

	return false
}

// matches checks if a resource rule matches the given filters.
func (r *CompiledResourceRule) matches(
	eventNamespace string,
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	// Match the resolved source-namespace set (a cluster-scoped or namespace-less event is left to
	// the caller's scope check).
	if !r.matchesSourceNamespace(eventNamespace) {
		return false
	}

	return r.matchesType(resourcePlural, operation, apiGroup, apiVersion)
}

// matchesSourceNamespace checks the event's namespace against this item's RESOLVED set. An event
// with no namespace is left to the caller's cluster-scope check rather than being filtered here.
//
// An EMPTY ENTRY in the set is the cluster-wide selection a `sourceNamespace: "*"` item compiles
// to, and it matches every namespace. The identity is the same one CellKey uses (its Namespace doc
// records why), so the router and the stream planner agree about what the item selects. Without
// this the item would open a cluster-wide stream and then route none of its events, since no real
// object carries the empty namespace.
func (r *CompiledResourceRule) matchesSourceNamespace(eventNamespace string) bool {
	if eventNamespace == "" {
		return true
	}
	for _, ns := range r.SourceNamespaces {
		if ns == "" || ns == eventNamespace {
			return true
		}
	}
	return false
}

// matchesType answers the whole type filter: operations, group, version, resource plural. An empty
// list means "match all" on every axis except the resource plural, which is required.
func (s *compiledSelector) matchesType(
	resourcePlural string,
	operation configv1alpha3.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	if !s.matchesOperations(operation) {
		return false
	}
	if !matchesAny(s.APIGroups, apiGroup) {
		return false
	}
	if !matchesAny(s.APIVersions, apiVersion) {
		return false
	}
	return s.resourceMatches(resourcePlural)
}

// matchesOperations checks if the operation matches any in the rule.
func (s *compiledSelector) matchesOperations(operation configv1alpha3.OperationType) bool {
	if len(s.Operations) == 0 {
		return true // Empty = match all
	}

	for _, op := range s.Operations {
		if op == configv1alpha3.OperationAll || op == operation {
			return true
		}
	}
	return false
}

// matchesAny is the group and version test, which are the same test: an empty list matches all, and
// "*" is the only wildcard either axis accepts.
func matchesAny(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true // Empty = match all
	}

	for _, pattern := range patterns {
		if pattern == "*" || pattern == value {
			return true
		}
	}
	return false
}

// resourceMatches checks if the resource plural matches any of the rule patterns.
func (s *compiledSelector) resourceMatches(resourcePlural string) bool {
	for _, ruleResource := range s.Resources {
		if singleResourceMatches(ruleResource, resourcePlural) {
			return true
		}
	}
	return false
}

// singleResourceMatches checks if a single rule pattern matches the given resource plural.
// Supports:
//   - "*" - matches all resources
//   - "pods" - exact match (case-insensitive)
//   - "pods/*" - matches all pod subresources (pods/log, pods/status, etc.)
//   - "pods/log" - matches specific subresource
//
// Does NOT support:
//   - Prefix wildcards: "pod*" (removed per enhancement plan)
//   - Suffix wildcards: "*.example.com" (removed per enhancement plan)
func singleResourceMatches(ruleResource, resourcePlural string) bool {
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
		prefix := ruleResource[:len(ruleResource)-2] // Remove "/*"
		return strings.HasPrefix(strings.ToLower(resourcePlural), strings.ToLower(prefix)+"/")
	}

	return false
}

// SnapshotWatchRules returns a deep-copied slice of compiled WatchRule entries.
// Safe for concurrent use; the returned slice can be freely modified by callers.
func (s *RuleStore) SnapshotWatchRules() []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]CompiledRule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, deepCopyCompiledRule(r))
	}
	return out
}

// SnapshotClusterWatchRules returns a deep-copied slice of compiled ClusterWatchRule entries.
// Safe for concurrent use; the returned slice can be freely modified by callers.
func (s *RuleStore) SnapshotClusterWatchRules() []CompiledClusterRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]CompiledClusterRule, 0, len(s.clusterRules))
	for _, r := range s.clusterRules {
		out = append(out, deepCopyCompiledClusterRule(r))
	}
	return out
}

// deepCopyCompiledRule creates a defensive copy of a CompiledRule including its slices.
func deepCopyCompiledRule(in CompiledRule) CompiledRule {
	cp := in
	if len(in.ResourceRules) > 0 {
		cp.ResourceRules = make([]CompiledResourceRule, len(in.ResourceRules))
		copy(cp.ResourceRules, in.ResourceRules)
		for i := range cp.ResourceRules {
			cp.ResourceRules[i].SourceNamespaces =
				append([]string(nil), in.ResourceRules[i].SourceNamespaces...)
		}
	}
	return cp
}

// deepCopyCompiledClusterRule creates a defensive copy of a CompiledClusterRule including its slices.
func deepCopyCompiledClusterRule(in CompiledClusterRule) CompiledClusterRule {
	cp := in
	if len(in.Rules) > 0 {
		cp.Rules = make([]CompiledClusterResourceRule, len(in.Rules))
		copy(cp.Rules, in.Rules)
	}
	return cp
}
