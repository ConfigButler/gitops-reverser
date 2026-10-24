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

// ConsumerNameVariable stands, in a grant's scope or branch pattern, for the consumer's name: the
// name of the object asking for access.
const ConsumerNameVariable = "{consumer.name}"

// GrantNamespace is one control-cluster namespace a grant admits by name.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:XValidation:rule="self != '{consumer.namespace}' && self != '{consumer.name}'",message="names: a variable is refused, because names are matched against the consumer's namespace and '{consumer.namespace}' would admit every consumer; use from: All to admit every namespace, or from: Same for the namespace of the object holding the grant"
// +kubebuilder:validation:XValidation:rule="self != '*'",message="names: '*' is refused; use from: All to admit every namespace"
// +kubebuilder:validation:XValidation:rule="self == '{consumer.namespace}' || self == '{consumer.name}' || self == '*' || self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="names holds namespace names (DNS labels)"
type GrantNamespace string

// ScopeNamespace is one namespace in a source or applier scope: a namespace name,
// "{consumer.namespace}" or "{consumer.name}" for the namespace of the same name as the consumer's
// namespace or name, or "*" for every namespace.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:XValidation:rule="self == '*' || self == '{consumer.namespace}' || self == '{consumer.name}' || self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="a namespace scope holds a namespace name, '{consumer.namespace}' or '{consumer.name}' alone, or '*' alone"
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
// path segments.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[^\s~^:?\[\\]*$`
// +kubebuilder:validation:XValidation:rule="!self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{][^{}]*[}]')",messageExpression="'branch pattern ' + self + ' references unknown variable ' + self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').find('[{][^{}]*[}]') + '; the variables are {consumer.namespace} and {consumer.name}'"
// +kubebuilder:validation:XValidation:rule="self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{][^{}]*[}]') || !self.replace('{consumer.namespace}', 'x').replace('{consumer.name}', 'x').matches('[{}]')",messageExpression="'branch pattern ' + self + ' has an unmatched brace; the variables are {consumer.namespace} and {consumer.name}'"
// +kubebuilder:validation:XValidation:rule="!self.matches('[^/][{]consumer[.]name(space)?[}]') && !self.matches('[{]consumer[.]name(space)?[}][^/]')",messageExpression="'branch pattern ' + self + ': {consumer.namespace} and {consumer.name} must each be a whole path segment, at the start or after a /, and at the end or before a /'"
// +kubebuilder:validation:XValidation:rule="!self.contains('..') && !self.contains('@{') && !self.startsWith('/') && !self.endsWith('/') && !self.contains('//')",messageExpression="'branch pattern ' + self + ' can match no branch name: git refuses .., @{, a leading or trailing / and //'"
type BranchPattern string

// The source half (from, names, selector) is repeated in each grant type rather than embedded: the
// CEL rules sit on the type, and an inline-embedded struct's type-level rules do not carry over to
// the struct embedding it.

// NamespaceGrant admits the objects in some control-cluster namespaces, for the act the list holding
// it names.
// +kubebuilder:validation:XValidation:rule="self.from != 'Names' || has(self.names)",message="from: Names needs names"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || self.from == 'Names'",message="names goes only with from: Names"
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="from: Selector needs a selector; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.from == 'Selector'",message="selector goes only with from: Selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || (has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector: {} is refused; to admit every namespace, use from: All"
type NamespaceGrant struct {
	// From is where this grant admits from: Same (the namespace of the object holding the grant),
	// Names (the namespaces in names), Selector (the namespaces whose labels match selector) or All
	// (every namespace).
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

// GitBranchGrant admits GitBranches in some control-cluster namespaces to claim the branches its
// patterns match through a GitProvider.
// +kubebuilder:validation:XValidation:rule="self.from != 'Names' || has(self.names)",message="from: Names needs names"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || self.from == 'Names'",message="names goes only with from: Names"
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="from: Selector needs a selector; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.from == 'Selector'",message="selector goes only with from: Selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || (has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector: {} is refused; to admit every namespace, use from: All"
type GitBranchGrant struct {
	// From is where this grant admits from: Same (this GitProvider's namespace), Names (the
	// namespaces in names), Selector (the namespaces whose labels match selector) or All (every
	// namespace).
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

	// Branches are the branches an admitted GitBranch may claim, as patterns matched against the
	// whole branch name. '*' matches any sequence of characters, '/' included, so "*" alone is every
	// branch. '{consumer.namespace}' and '{consumer.name}' are the claiming GitBranch's namespace and
	// name, each a whole path segment: "teams/{consumer.namespace}/*". There is no escaping, '?' or
	// character class.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Branches []BranchPattern `json:"branches"`
}

// SourceGrant admits GitTargets in some control-cluster namespaces to mirror through a KubeProvider,
// and bounds what they may mirror.
// +kubebuilder:validation:XValidation:rule="self.from != 'Names' || has(self.names)",message="from: Names needs names"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || self.from == 'Names'",message="names goes only with from: Names"
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="from: Selector needs a selector; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.from == 'Selector'",message="selector goes only with from: Selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || (has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector: {} is refused; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="has(self.sourceNamespaces) || has(self.clusterResources)",message="a grant in allowedGitTargets sets sourceNamespaces, clusterResources, or both; one that grants neither grants nothing"
type SourceGrant struct {
	// From is where this grant admits from: Same (this KubeProvider's namespace), Names (the
	// namespaces in names), Selector (the namespaces whose labels match selector) or All (every
	// namespace).
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

	// SourceNamespaces are the source-cluster namespaces the WatchRules feeding an admitted GitTarget
	// may watch: exact names, '{consumer.namespace}' or '{consumer.name}' for the namespace named like
	// the GitTarget's namespace or name, or '*' for every namespace, which also allows
	// rules[].sourceNamespace: "*". The list is the whole scope; the GitTarget's own namespace is in it
	// only when listed. Omitted, no namespace.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	SourceNamespaces []ScopeNamespace `json:"sourceNamespaces,omitempty"`

	// ClusterResources are the cluster-scoped resource types a ClusterWatchRule may mirror through an
	// admitted GitTarget: resource.group (clusterroles.rbac.authorization.k8s.io), a bare resource for
	// the core group only (nodes), or '*' for every one, including CRDs installed later. An identity
	// covers every served version. A ClusterWatchRule item with '*' in apiGroups or resources needs
	// '*' here. Omitted, none.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ClusterResources []ClusterResourceIdentity `json:"clusterResources,omitempty"`
}

// ApplierGrant admits applier claims in some control-cluster namespaces to hold an applier through a
// KubeProvider, and bounds where that applier may live.
// +kubebuilder:validation:XValidation:rule="self.from != 'Names' || has(self.names)",message="from: Names needs names"
// +kubebuilder:validation:XValidation:rule="!has(self.names) || self.from == 'Names'",message="names goes only with from: Names"
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="from: Selector needs a selector; to admit every namespace, use from: All"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.from == 'Selector'",message="selector goes only with from: Selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || (has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector: {} is refused; to admit every namespace, use from: All"
type ApplierGrant struct {
	// From is where this grant admits from: Same (this KubeProvider's namespace), Names (the
	// namespaces in names), Selector (the namespaces whose labels match selector) or All (every
	// namespace).
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

	// ApplierNamespaces are the source-cluster namespaces in which an admitted applier claim's
	// Kustomization or Application may live: exact names, '{consumer.namespace}' or '{consumer.name}'
	// for the namespace named like the applier claim's namespace or name, or '*' for every namespace.
	// The list is the whole scope.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ApplierNamespaces []ScopeNamespace `json:"applierNamespaces"`
}

// source is the admitting half of a GitBranchGrant.
func (g *GitBranchGrant) source() NamespaceGrant {
	return NamespaceGrant{From: g.From, Names: g.Names, Selector: g.Selector}
}

// source is the admitting half of a SourceGrant.
func (g *SourceGrant) source() NamespaceGrant {
	return NamespaceGrant{From: g.From, Names: g.Names, Selector: g.Selector}
}

// source is the admitting half of an ApplierGrant.
func (g *ApplierGrant) source() NamespaceGrant {
	return NamespaceGrant{From: g.From, Names: g.Names, Selector: g.Selector}
}

// sourcesOf is the admitting half of each grant in a list.
func sourcesOf[G any, PG interface {
	*G
	source() NamespaceGrant
}](grants []G) []NamespaceGrant {
	sources := make([]NamespaceGrant, len(grants))
	for i := range grants {
		sources[i] = PG(&grants[i]).source()
	}
	return sources
}

// Admits reports whether this grant, held by an object in holderNamespace, admits a referring
// object in namespace, by the labels that namespace carries in the control cluster. A selector that
// does not convert is an error, never a silent allow; an unknown from admits nothing.
func (g *NamespaceGrant) Admits(holderNamespace, namespace string, nsLabels map[string]string) (bool, error) {
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
func (g *NamespaceGrant) Describe(holderNamespace string) string {
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

// admitAll evaluates every grant's source for namespace and returns which admit it. A grant whose
// selector does not convert admits nothing; its error is returned only when no grant admits, so
// one malformed grant cannot widen anything, and an admitted namespace is not refused over a grant
// that did not apply to it.
func admitAll(
	sources []NamespaceGrant, holderNamespace, namespace string, nsLabels map[string]string,
) ([]int, error) {
	var admitting []int
	var errs []error
	for i := range sources {
		admitted, err := sources[i].Admits(holderNamespace, namespace, nsLabels)
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
func describeAdmitted(sources []NamespaceGrant, holderNamespace string) string {
	if len(sources) == 0 {
		return "admits no namespace"
	}
	parts := make([]string, 0, len(sources))
	for i := range sources {
		parts = append(parts, sources[i].Describe(holderNamespace))
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
	// Branches are the patterns the admitting grants give the GitBranch, consumer variables expanded.
	Branches []string
	// Allowed reports whether the namespace is admitted and the branch matches one of Branches.
	Allowed bool
}

// AdmitsGitBranch reports what spec.allowedGitBranches gives the GitBranch consumer, by the labels
// its namespace carries in the control cluster, and whether it may claim branch.
func (p *GitProvider) AdmitsGitBranch(
	consumer types.NamespacedName, nsLabels map[string]string, branch string,
) (GitBranchAdmission, error) {
	grants := p.Spec.AllowedGitBranches
	admitting, err := admitAll(sourcesOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return GitBranchAdmission{}, err
	}
	out := GitBranchAdmission{Admitted: true}
	var patterns []BranchPattern
	for _, i := range admitting {
		for _, pattern := range grants[i].Branches {
			patterns = append(patterns, pattern)
			if MatchBranchPattern(pattern, consumer, branch) {
				out.Allowed = true
			}
		}
	}
	out.Branches = expandScope(patterns, consumer)
	return out, nil
}

// DescribeAllowedGitBranches names the namespaces spec.allowedGitBranches admits.
func (p *GitProvider) DescribeAllowedGitBranches() string {
	return describeAdmitted(sourcesOf(p.Spec.AllowedGitBranches), p.Namespace)
}

// AdmitsGitTarget reports whether spec.allowedGitTargets admits a GitTarget in namespace, by the
// labels that namespace carries in the control cluster.
func (b *GitBranch) AdmitsGitTarget(namespace string, nsLabels map[string]string) (bool, error) {
	admitting, err := admitAll(b.Spec.AllowedGitTargets, b.Namespace, namespace, nsLabels)
	return len(admitting) > 0, err
}

// DescribeAllowedGitTargets names the namespaces spec.allowedGitTargets admits.
func (b *GitBranch) DescribeAllowedGitTargets() string {
	return describeAdmitted(b.Spec.AllowedGitTargets, b.Namespace)
}

// SourceScope is what a KubeProvider's allowedGitTargets give a GitTarget: whether it may mirror at
// all, and what.
// +kubebuilder:object:generate=false
type SourceScope struct {
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
func (s SourceScope) AllowsSourceNamespace(namespace string) bool {
	return scopeAllows(s.SourceNamespaces, namespace)
}

// AllowsAllClusterResources reports whether the scope holds "*" in clusterResources.
func (s SourceScope) AllowsAllClusterResources() bool {
	return scopeAllows(s.ClusterResources, "*")
}

// AllowsClusterResource reports whether a ClusterWatchRule may mirror resource in group.
func (s SourceScope) AllowsClusterResource(group, resource string) bool {
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
) (SourceScope, error) {
	grants := p.Spec.AllowedGitTargets
	admitting, err := admitAll(sourcesOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return SourceScope{}, err
	}
	var namespaces []ScopeNamespace
	var resources []ClusterResourceIdentity
	for _, i := range admitting {
		namespaces = append(namespaces, grants[i].SourceNamespaces...)
		resources = append(resources, grants[i].ClusterResources...)
	}
	return SourceScope{
		Admitted:         true,
		SourceNamespaces: expandScope(namespaces, consumer),
		ClusterResources: expandScope(resources, consumer),
	}, nil
}

// DescribeAllowedGitTargets names the namespaces spec.allowedGitTargets admits.
func (p *KubeProvider) DescribeAllowedGitTargets() string {
	return describeAdmitted(sourcesOf(p.Spec.AllowedGitTargets), p.Namespace)
}

// ApplierScope is what a KubeProvider's allowedApplierClaims give an applier claim.
// +kubebuilder:object:generate=false
type ApplierScope struct {
	// Admitted reports whether some grant admits the applier claim's namespace.
	Admitted bool
	// ApplierNamespaces is the union of the admitting grants' applierNamespaces, consumer variables
	// expanded. "*" is every namespace.
	ApplierNamespaces []string
}

// AllowsApplierNamespace reports whether an applier in namespace may be held.
func (s ApplierScope) AllowsApplierNamespace(namespace string) bool {
	return scopeAllows(s.ApplierNamespaces, namespace)
}

// AdmitsApplierClaim reports what spec.allowedApplierClaims gives the applier claim consumer, by the
// labels its namespace carries in the control cluster.
func (p *KubeProvider) AdmitsApplierClaim(
	consumer types.NamespacedName, nsLabels map[string]string,
) (ApplierScope, error) {
	grants := p.Spec.AllowedApplierClaims
	admitting, err := admitAll(sourcesOf(grants), p.Namespace, consumer.Namespace, nsLabels)
	if err != nil || len(admitting) == 0 {
		return ApplierScope{}, err
	}
	var namespaces []ScopeNamespace
	for _, i := range admitting {
		namespaces = append(namespaces, grants[i].ApplierNamespaces...)
	}
	return ApplierScope{Admitted: true, ApplierNamespaces: expandScope(namespaces, consumer)}, nil
}

// DescribeAllowedApplierClaims names the namespaces spec.allowedApplierClaims admits.
func (p *KubeProvider) DescribeAllowedApplierClaims() string {
	return describeAdmitted(sourcesOf(p.Spec.AllowedApplierClaims), p.Namespace)
}
