// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// Access between objects (docs/configuration.md): consent lives on the object
// that lends something, in a list named "allowed" plus the referring kind. Every grant says where
// it admits from with an enum borrowed from Gateway API's allowedRoutes.namespaces.from, required
// here so that no grant admits anyone by omission. Nothing is automatic, the object's own namespace
// included: an omitted or empty list admits nobody, and grants combine by union. The object
// asking for access is the consumer, and a scope or branch pattern can name it with a variable.

// GrantFrom is where a grant admits referring objects from.
// +kubebuilder:validation:Enum=Same;Names;Selector;All
type GrantFrom string

const (
	// GrantFromSame admits the namespace of the object that holds the grant.
	GrantFromSame GrantFrom = "Same"
	// GrantFromNames admits exactly the namespaces the grant lists in names.
	GrantFromNames GrantFrom = "Names"
	// GrantFromSelector admits the namespaces whose labels match the grant's selector.
	GrantFromSelector GrantFrom = "Selector"
	// GrantFromAll admits every namespace.
	GrantFromAll GrantFrom = "All"
)

// ConsumerNamespaceVariable stands, in a grant's scope or branch pattern, for the consumer's
// namespace: the control-cluster namespace of the object asking for access.
const ConsumerNamespaceVariable = "{consumer.namespace}"

// ConsumerNameVariable stands, in a branch pattern that also holds ConsumerNamespaceVariable, for the
// consumer's name: the name of the object asking for access. The consumer picks its own name, so the
// name bounds nothing on its own; beside the namespace it is a naming convention.
const ConsumerNameVariable = "{consumer.name}"

// GrantNamespace is one control-cluster namespace a grant admits by name.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:XValidation:rule="self != '{consumer.namespace}' && self != '{consumer.name}'",message="names: a variable is refused, because names are matched against the consumer's namespace and '{consumer.namespace}' would admit every consumer; use from: All to admit every namespace, or from: Same for the namespace of the object holding the grant"
// +kubebuilder:validation:XValidation:rule="self != '*'",message="names: '*' is refused; use from: All to admit every namespace"
// +kubebuilder:validation:XValidation:rule="self == '{consumer.namespace}' || self == '{consumer.name}' || self == '*' || self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="names holds namespace names (DNS labels)"
type GrantNamespace string

// ScopeNamespace is one namespace in a source or applier scope: a namespace name,
// "{consumer.namespace}" for the namespace of the same name as the consumer's, or "*" for every
// namespace.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:XValidation:rule="self != '{consumer.name}'",message="'{consumer.name}' is refused in a namespace scope: the consumer picks its own name, so any namespace could be reached by naming an object after it; use '{consumer.namespace}', namespace names, or '*'"
// +kubebuilder:validation:XValidation:rule="self == '*' || self == '{consumer.namespace}' || self == '{consumer.name}' || self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="a namespace scope holds a namespace name, '{consumer.namespace}' alone, or '*' alone"
type ScopeNamespace string

// ClusterResourceIdentity names a cluster-scoped resource type: resource.group, a bare resource for
// the core group, or "*" for every one.
// +kubebuilder:validation:MaxLength=317
// +kubebuilder:validation:Pattern=`^(\*|[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)$`
type ClusterResourceIdentity string

// The pattern grammar is checked in CEL so a refusal names the mistake. The character set is an
// OpenAPI pattern: git check-ref-format refuses all of these in a branch name, so a pattern holding
// one could never match, and with no '?' or '[' nothing in a pattern ever needs escaping.

// BranchPattern is a pattern matched against a whole branch name: literal text, '*' for any
// sequence of characters ('/' included), and '{consumer.namespace}' and '{consumer.name}' as whole
// path segments. '{consumer.name}' needs '{consumer.namespace}' in the same pattern.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[^\s~^:?\[\\]*$`
// +kubebuilder:validation:XValidation:rule="!self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{][^{}]*[}]')",messageExpression="'branch pattern ' + self + ' references unknown variable ' + self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').find('[{][^{}]*[}]') + '; the variables are {consumer.namespace} and {consumer.name}'"
// +kubebuilder:validation:XValidation:rule="self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{][^{}]*[}]') || !self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{}]')",messageExpression="'branch pattern ' + self + ' has an unmatched brace; the variables are {consumer.namespace} and {consumer.name}'"
// +kubebuilder:validation:XValidation:rule="!self.matches('[^/][{]consumer[.]name(space)?[}]') && !self.matches('[{]consumer[.]name(space)?[}][^/]')",messageExpression="'branch pattern ' + self + ': {consumer.namespace} and {consumer.name} must each be a whole path segment, at the start or after a /, and at the end or before a /'"
// +kubebuilder:validation:XValidation:rule="!self.contains('{consumer.name}') || self.contains('{consumer.namespace}')",messageExpression="'branch pattern ' + self + ': {consumer.name} needs {consumer.namespace} in the same pattern; the consumer picks its own name, so on its own it would let any admitted namespace claim any branch the pattern leaves open'"
// +kubebuilder:validation:XValidation:rule="!self.contains('..') && !self.contains('@{') && !self.startsWith('/') && !self.endsWith('/') && !self.contains('//')",messageExpression="'branch pattern ' + self + ' can match no branch name: git refuses .., @{, a leading or trailing / and //'"
type BranchPattern string

// Each grant type holds ConsumerNamespaces as a named field, never inline-embedded: the CEL rules
// sit on the type, and an inline-embedded struct's type-level rules do not carry over to the struct
// embedding it.

// ConsumerNamespaces says which control-cluster namespaces a grant admits consumers from, for the act
// the list holding the grant names.
// +kubebuilder:validation:XValidation:rule="self.from != 'Names' || has(self.names)",message="from: Names needs names"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || self.from == 'Names'",message="names goes only with from: Names"
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="from: Selector needs a selector; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.from == 'Selector'",message="selector goes only with from: Selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || (has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector: {} is refused; to admit every namespace, use from: All"
type ConsumerNamespaces struct {
	// From is where this grant admits from: Same (the namespace of the object holding the grant, not
	// the consumer's), Names (the namespaces in names), Selector (the namespaces whose labels match
	// selector) or All (every namespace).
	// +required
	From GrantFrom `json:"from"`

	// Names are the control-cluster namespaces admitted with from: Names, by exact name.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Names []GrantNamespace `json:"names,omitempty"`

	// Selector matches labels on control-cluster Namespaces, with from: Selector. It must select
	// something: to admit every namespace, use from: All.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// GitProviderGitBranchGrant admits GitBranches in some control-cluster namespaces to claim the
// branches its patterns match through a GitProvider.
type GitProviderGitBranchGrant struct {
	// ConsumerNamespaces are the control-cluster namespaces whose GitBranches this grant admits.
	// from: Same is this GitProvider's namespace.
	// +required
	ConsumerNamespaces ConsumerNamespaces `json:"consumerNamespaces"`

	// BranchPatterns are the branches an admitted GitBranch may claim, as patterns matched against
	// the whole branch name. '*' matches any sequence of characters, '/' included, so "*" alone is
	// every branch, and a name with no '*', such as "main", matches only itself.
	// '{consumer.namespace}' and '{consumer.name}' are the claiming GitBranch's namespace and name,
	// each a whole path segment: "teams/{consumer.namespace}/*". '{consumer.name}' needs
	// '{consumer.namespace}' in the same pattern, "teams/{consumer.namespace}/{consumer.name}": it is
	// a naming convention, not a bound, since a GitBranch picks its own name. There is no escaping,
	// '?' or character class.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	BranchPatterns []BranchPattern `json:"branchPatterns"`
}

// GitBranchGitTargetGrant admits GitTargets in some control-cluster namespaces to write through a
// GitBranch.
type GitBranchGitTargetGrant struct {
	// ConsumerNamespaces are the control-cluster namespaces whose GitTargets this grant admits.
	// from: Same is this GitBranch's namespace.
	// +required
	ConsumerNamespaces ConsumerNamespaces `json:"consumerNamespaces"`
}

// KubeProviderGitTargetGrant admits GitTargets in some control-cluster namespaces to mirror through a
// KubeProvider, and bounds what they may mirror.
// +kubebuilder:validation:XValidation:rule="has(self.sourceNamespaces) || has(self.clusterResources)",message="a grant in allowedGitTargets sets sourceNamespaces, clusterResources, or both; one that grants neither grants nothing"
type KubeProviderGitTargetGrant struct {
	// ConsumerNamespaces are the control-cluster namespaces whose GitTargets this grant admits.
	// from: Same is this KubeProvider's namespace.
	// +required
	ConsumerNamespaces ConsumerNamespaces `json:"consumerNamespaces"`

	// SourceNamespaces are source-cluster namespaces that a WatchRule feeding an admitted GitTarget may
	// name in rules[].sourceNamespace. Entries are exact names, '{consumer.namespace}' for the
	// namespace named like the GitTarget's, or '*'. Omitted, this grant gives no namespace.
	//
	// The effective scope is the union of sourceNamespaces from all grants admitting the GitTarget,
	// after expanding variables. Every WatchRule item must be allowed by that union, an omitted
	// sourceNamespace included (it names the WatchRule's own namespace). This is a permission, not a
	// selection: listing a namespace watches nothing until a WatchRule names it.
	//
	// It is not a filter on "*". A rule item with sourceNamespace "*" needs '*' in at least one
	// admitting grant; otherwise it is refused, never narrowed to the namespaces listed. With '*'
	// granted, such an item is one cluster-wide watch, bounded only by this KubeProvider credential's
	// RBAC.
	//
	// The check runs when the WatchRule reconciles, and again when a grant changes, never at
	// admission. A WatchRule with any item outside the union is refused as a whole, in its status
	// (SourceNamespaceNotAllowed), and watches nothing. The scope bounds what is read, never where
	// objects are written in Git, and the credential's RBAC still applies inside it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	SourceNamespaces []ScopeNamespace `json:"sourceNamespaces,omitempty"`

	// ClusterResources are the cluster-scoped resource types a ClusterWatchRule may mirror through an
	// admitted GitTarget: resource.group (clusterroles.rbac.authorization.k8s.io), a bare resource for
	// the core group only (nodes), or '*' for every one, including CRDs installed later. An identity
	// covers every served version. Omitted, this grant gives none.
	//
	// The effective scope is the union of clusterResources from all grants admitting the GitTarget.
	// This is a permission, not a selection: listing a type mirrors nothing until a ClusterWatchRule
	// asks for it. A ClusterWatchRule item with '*' in apiGroups or resources needs '*' in at least
	// one admitting grant; otherwise it is refused, never narrowed to the types listed. The check runs
	// when the ClusterWatchRule reconciles, never at admission, and a rule asking for any type outside
	// the union is refused as a whole, in its status (ClusterResourceNotAllowed).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ClusterResources []ClusterResourceIdentity `json:"clusterResources,omitempty"`
}

// KubeProviderApplierClaimGrant admits applier claims in some control-cluster namespaces to hold an
// applier through a KubeProvider, and bounds where that applier may live.
type KubeProviderApplierClaimGrant struct {
	// ConsumerNamespaces are the control-cluster namespaces whose applier claims this grant admits.
	// from: Same is this KubeProvider's namespace.
	// +required
	ConsumerNamespaces ConsumerNamespaces `json:"consumerNamespaces"`

	// ApplierNamespaces are namespaces in this provider's cluster in which an admitted applier claim's
	// Kustomization or Application may live: exact names, '{consumer.namespace}' for the namespace
	// named like the applier claim's, or '*' for every namespace.
	//
	// The effective scope is the union of applierNamespaces from all grants admitting the applier
	// claim, after expanding variables. This is a permission, not a selection: it is checked against
	// one namespace, that of the applier claim's kustomizationRef or applicationRef. The check runs
	// when the applier claim reconciles, and again when a grant changes, never at admission. An
	// applier outside the union is refused, in the applier claim's status
	// (ApplierNamespaceNotAllowed), and an applier claim holding it hands it back.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ApplierNamespaces []ScopeNamespace `json:"applierNamespaces"`
}

// consumers is the admitting half of a GitProviderGitBranchGrant.
func (g *GitProviderGitBranchGrant) consumers() ConsumerNamespaces { return g.ConsumerNamespaces }

// consumers is the admitting half of a GitBranchGitTargetGrant.
func (g *GitBranchGitTargetGrant) consumers() ConsumerNamespaces { return g.ConsumerNamespaces }

// consumers is the admitting half of a KubeProviderGitTargetGrant.
func (g *KubeProviderGitTargetGrant) consumers() ConsumerNamespaces { return g.ConsumerNamespaces }

// consumers is the admitting half of a KubeProviderApplierClaimGrant.
func (g *KubeProviderApplierClaimGrant) consumers() ConsumerNamespaces { return g.ConsumerNamespaces }

// consumersOf is the admitting half of each grant in a list.
func consumersOf[G any, PG interface {
	*G
	consumers() ConsumerNamespaces
}](grants []G) []ConsumerNamespaces {
	consumers := make([]ConsumerNamespaces, len(grants))
	for i := range grants {
		consumers[i] = PG(&grants[i]).consumers()
	}
	return consumers
}

// Admits reports whether this grant, held by an object in holderNamespace, admits a referring
// object in namespace, by the labels that namespace carries in the control cluster. A selector that
// does not convert is an error, never a silent allow; an unknown from admits nothing.
func (g *ConsumerNamespaces) Admits(holderNamespace, namespace string, nsLabels map[string]string) (bool, error) {
	switch g.From {
	case GrantFromSame:
		return namespace == holderNamespace, nil
	case GrantFromNames:
		for _, name := range g.Names {
			if string(name) == namespace {
				return true, nil
			}
		}
		return false, nil
	case GrantFromSelector:
		if g.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(g.Selector)
		if err != nil {
			return false, fmt.Errorf("selector %s: %w", metav1.FormatLabelSelector(g.Selector), err)
		}
		if selector.Empty() {
			// selector: {} is refused at admission; a stored one admits nobody rather than everyone.
			return false, nil
		}
		return selector.Matches(labels.Set(nsLabels)), nil
	case GrantFromAll:
		return true, nil
	}
	return false, nil
}

// Describe names the namespaces this grant admits, for a refusal message.
func (g *ConsumerNamespaces) Describe(holderNamespace string) string {
	switch g.From {
	case GrantFromSame:
		return holderNamespace
	case GrantFromNames:
		names := make([]string, 0, len(g.Names))
		for _, name := range g.Names {
			names = append(names, string(name))
		}
		return strings.Join(names, ", ")
	case GrantFromSelector:
		if g.Selector == nil {
			return "no namespace"
		}
		return "namespaces labelled " + metav1.FormatLabelSelector(g.Selector)
	case GrantFromAll:
		return "every namespace"
	}
	return fmt.Sprintf("no namespace (from %q is unknown)", g.From)
}

// admitAll evaluates every grant's consumerNamespaces for namespace and returns which admit it. A grant whose
// selector does not convert admits nothing; its error is returned only when no grant admits, so
// one malformed grant cannot widen anything, and an admitted namespace is not refused over a grant
// that did not apply to it.
func admitAll(
	consumers []ConsumerNamespaces, holderNamespace, namespace string, nsLabels map[string]string,
) ([]int, error) {
	var admitting []int
	var errs []error
	for i := range consumers {
		admitted, err := consumers[i].Admits(holderNamespace, namespace, nsLabels)
		if err != nil {
			errs = append(errs, fmt.Errorf("grant %d: %w", i, err))
			continue
		}
		if admitted {
			admitting = append(admitting, i)
		}
	}
	if len(admitting) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return admitting, nil
}

// describeAdmitted names the namespaces a list of grants admits, for a refusal message: "admits
// team-a, team-b" or "admits no namespace".
func describeAdmitted(consumers []ConsumerNamespaces, holderNamespace string) string {
	if len(consumers) == 0 {
		return "admits no namespace"
	}
	parts := make([]string, 0, len(consumers))
	for i := range consumers {
		parts = append(parts, consumers[i].Describe(holderNamespace))
	}
	return "admits " + strings.Join(parts, ", ")
}

// expandConsumer replaces the consumer variables in s with consumer's namespace and name.
func expandConsumer(s string, consumer types.NamespacedName) string {
	return strings.NewReplacer(ConsumerNamespaceVariable, consumer.Namespace, ConsumerNameVariable, consumer.Name).
		Replace(s)
}

// expandScope expands the consumer variables in each scope entry, and returns the entries sorted and
// without duplicates.
func expandScope[S ~string](entries []S, consumer types.NamespacedName) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, expandConsumer(string(entry), consumer))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// MatchBranchPattern reports whether branch matches pattern for the GitBranch consumer: the pattern
// is matched against the whole branch name, '*' matches any sequence of characters, '/' included,
// and '{consumer.namespace}' and '{consumer.name}' are consumer's namespace and name. Nothing else is
// special.
func MatchBranchPattern(pattern BranchPattern, consumer types.NamespacedName, branch string) bool {
	return globMatch(expandConsumer(string(pattern), consumer), branch)
}

// globMatch matches name against a pattern in which '*' is the only special character, and matches
// any sequence, the empty one included.
func globMatch(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(name, part)
		if index < 0 {
			return false
		}
		name = name[index+len(part):]
	}
	return strings.HasSuffix(name, last) && len(name) >= len(last)
}

// ClusterResourceIdentityFor is the identity of a resource in group: resource.group, or the bare
// resource for the core group.
func ClusterResourceIdentityFor(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}

// GitBranchAdmission is what a GitProvider's allowedGitBranches give a GitBranch.
// +kubebuilder:object:generate=false
type GitBranchAdmission struct {
	// Admitted reports whether some grant admits the GitBranch's namespace.
	Admitted bool
	// BranchPatterns are the patterns the admitting grants give the GitBranch, consumer variables
	// expanded.
	BranchPatterns []string
	// Allowed reports whether the namespace is admitted and the branch matches one of BranchPatterns.
	Allowed bool
}

// AdmitsGitBranch reports what spec.allowedGitBranches gives the GitBranch consumer, by the labels
// its namespace carries in the control cluster, and whether it may claim branch.
func (p *GitProvider) AdmitsGitBranch(
	consumer types.NamespacedName, nsLabels map[string]string, branch string,
) (GitBranchAdmission, error) {
	grants := p.Spec.AllowedGitBranches
	admitting, err := admitAll(consumersOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return GitBranchAdmission{}, err
	}
	out := GitBranchAdmission{Admitted: true}
	var patterns []BranchPattern
	for _, i := range admitting {
		for _, pattern := range grants[i].BranchPatterns {
			patterns = append(patterns, pattern)
			if MatchBranchPattern(pattern, consumer, branch) {
				out.Allowed = true
			}
		}
	}
	out.BranchPatterns = expandScope(patterns, consumer)
	return out, nil
}

// DescribeAllowedGitBranches names the namespaces spec.allowedGitBranches admits.
func (p *GitProvider) DescribeAllowedGitBranches() string {
	return describeAdmitted(consumersOf(p.Spec.AllowedGitBranches), p.Namespace)
}

// AdmitsGitTarget reports whether spec.allowedGitTargets admits a GitTarget in namespace, by the
// labels that namespace carries in the control cluster.
func (b *GitBranch) AdmitsGitTarget(namespace string, nsLabels map[string]string) (bool, error) {
	admitting, err := admitAll(consumersOf(b.Spec.AllowedGitTargets), b.Namespace, namespace, nsLabels)
	return len(admitting) > 0, err
}

// DescribeAllowedGitTargets names the namespaces spec.allowedGitTargets admits.
func (b *GitBranch) DescribeAllowedGitTargets() string {
	return describeAdmitted(consumersOf(b.Spec.AllowedGitTargets), b.Namespace)
}

// KubeProviderGitTargetScope is what a KubeProvider's allowedGitTargets give a GitTarget: whether it may mirror at
// all, and what.
// +kubebuilder:object:generate=false
type KubeProviderGitTargetScope struct {
	// Admitted reports whether some grant admits the GitTarget's namespace.
	Admitted bool
	// SourceNamespaces is the union of the admitting grants' sourceNamespaces, consumer variables
	// expanded. "*" is every namespace.
	SourceNamespaces []string
	// ClusterResources is the union of the admitting grants' clusterResources. "*" is every
	// cluster-scoped resource.
	ClusterResources []string
}

// AllowsSourceNamespace reports whether a WatchRule may watch namespace; "*" asks for every
// namespace, which only a "*" in the scope allows.
func (s KubeProviderGitTargetScope) AllowsSourceNamespace(namespace string) bool {
	return scopeAllows(s.SourceNamespaces, namespace)
}

// AllowsAllClusterResources reports whether the scope holds "*" in clusterResources.
func (s KubeProviderGitTargetScope) AllowsAllClusterResources() bool {
	return scopeAllows(s.ClusterResources, "*")
}

// AllowsClusterResource reports whether a ClusterWatchRule may mirror resource in group.
func (s KubeProviderGitTargetScope) AllowsClusterResource(group, resource string) bool {
	return scopeAllows(s.ClusterResources, ClusterResourceIdentityFor(group, resource))
}

// scopeAllows reports whether scope holds value or "*".
func scopeAllows(scope []string, value string) bool {
	for _, entry := range scope {
		if entry == "*" || entry == value {
			return true
		}
	}
	return false
}

// AdmitsGitTarget reports what spec.allowedGitTargets gives the GitTarget consumer, by the labels its
// namespace carries in the control cluster.
func (p *KubeProvider) AdmitsGitTarget(
	consumer types.NamespacedName, nsLabels map[string]string,
) (KubeProviderGitTargetScope, error) {
	grants := p.Spec.AllowedGitTargets
	admitting, err := admitAll(consumersOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return KubeProviderGitTargetScope{}, err
	}
	var namespaces []ScopeNamespace
	var resources []ClusterResourceIdentity
	for _, i := range admitting {
		namespaces = append(namespaces, grants[i].SourceNamespaces...)
		resources = append(resources, grants[i].ClusterResources...)
	}
	return KubeProviderGitTargetScope{
		Admitted:         true,
		SourceNamespaces: expandScope(namespaces, consumer),
		ClusterResources: expandScope(resources, consumer),
	}, nil
}

// DescribeAllowedGitTargets names the namespaces spec.allowedGitTargets admits.
func (p *KubeProvider) DescribeAllowedGitTargets() string {
	return describeAdmitted(consumersOf(p.Spec.AllowedGitTargets), p.Namespace)
}

// KubeProviderApplierClaimScope is what a KubeProvider's allowedApplierClaims give an applier claim.
// +kubebuilder:object:generate=false
type KubeProviderApplierClaimScope struct {
	// Admitted reports whether some grant admits the applier claim's namespace.
	Admitted bool
	// ApplierNamespaces is the union of the admitting grants' applierNamespaces, consumer variables
	// expanded. "*" is every namespace.
	ApplierNamespaces []string
}

// AllowsApplierNamespace reports whether an applier in namespace may be held.
func (s KubeProviderApplierClaimScope) AllowsApplierNamespace(namespace string) bool {
	return scopeAllows(s.ApplierNamespaces, namespace)
}

// AdmitsApplierClaim reports what spec.allowedApplierClaims gives the applier claim consumer, by the
// labels its namespace carries in the control cluster.
func (p *KubeProvider) AdmitsApplierClaim(
	consumer types.NamespacedName, nsLabels map[string]string,
) (KubeProviderApplierClaimScope, error) {
	grants := p.Spec.AllowedApplierClaims
	admitting, err := admitAll(consumersOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return KubeProviderApplierClaimScope{}, err
	}
	var namespaces []ScopeNamespace
	for _, i := range admitting {
		namespaces = append(namespaces, grants[i].ApplierNamespaces...)
	}
	return KubeProviderApplierClaimScope{Admitted: true, ApplierNamespaces: expandScope(namespaces, consumer)}, nil
}

// DescribeAllowedApplierClaims names the namespaces spec.allowedApplierClaims admits.
func (p *KubeProvider) DescribeAllowedApplierClaims() string {
	return describeAdmitted(consumersOf(p.Spec.AllowedApplierClaims), p.Namespace)
}
