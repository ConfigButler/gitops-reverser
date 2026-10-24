// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Reasons for the WatchRule SourceNamespaceAuthorized condition. They are the rule-side names, so a
// reader never has to know which gate input produced the verdict — the Message carries that.
//
// Every input is on the WatchRule, the GitTarget, the KubeProvider and the GitTarget's Namespace, all
// in the control plane, so the gate is a comparison and its answer is always allowed or denied.
const (
	// ReasonSourceNamespaceAllowed is the True reason when every item watches a namespace the
	// KubeProvider's grants give the GitTarget's namespace.
	ReasonSourceNamespaceAllowed = "SourceNamespaceAllowed"

	// ReasonSourceNamespaceNotAllowed is the TERMINAL False reason for a refusal: the referenced
	// KubeProvider does not exist, does not admit this GitTarget's namespace, or its grants do not
	// give it the namespace an item asks for. It is a decision, never an inability to decide.
	ReasonSourceNamespaceNotAllowed = "SourceNamespaceNotAllowed"
)

// SourceNamespaceDecision is one rule ITEM's source-namespace verdict, plus the concrete namespace
// set it resolved to.
type SourceNamespaceDecision struct {
	// Index is the item's position in spec.rules.
	Index int
	// Requested is what the item asked for, verbatim: "" (omitted), a name, or "*".
	Requested string
	// Namespaces is the RESOLVED source-namespace set this item watches. It is a single-element
	// slice in every case: one name, or the EMPTY STRING for a wildcard, which the planner reads as
	// the cluster-wide collection. It is meaningful only when the item is admitted.
	Namespaces []string
	// Allowed reports whether the item may contribute selections.
	Allowed bool
	// Reason is the SourceNamespaceAuthorized condition reason this item would produce.
	Reason string
	// Message explains the verdict to an operator.
	Message string
}

// ResolvedSourceScope is a WHOLE WatchRule's source-namespace verdict: one decision per spec.rules
// item, index-aligned, plus the aggregate the SourceNamespaceAuthorized condition publishes.
//
// It is a pure function of (rule spec, the grants that admit the GitTarget) and is recomputed on
// every compile. Nothing per-item is persisted across a spec change, which is what lets rule items
// have no stable API identity: no state outlives the spec that produced it.
type ResolvedSourceScope struct {
	// Items is index-aligned with spec.rules.
	Items []SourceNamespaceDecision
	// Allowed is the aggregate over Items: every item admitted.
	Allowed bool
	// Reason is the aggregate SourceNamespaceAuthorized reason.
	Reason string
	// Message explains the aggregate, naming the deciding item when one item decided it.
	Message string
}

// Admitted reports whether the whole rule may compile.
func (s ResolvedSourceScope) Admitted() bool { return s.Allowed }

// NamespacesFor returns the resolved namespace set for one item index.
func (s ResolvedSourceScope) NamespacesFor(index int) []string {
	if index < 0 || index >= len(s.Items) {
		return nil
	}
	return s.Items[index].Namespaces
}

// Fingerprint renders the resolved scope as a stable string, per item, for the watched-type
// re-projection gate.
//
// It is computed from the RESOLVED set rather than the requested one, because the two differ for a
// wildcard — "*" resolves to the empty string — and for a denied item, which resolves to nothing:
// the fingerprint's job is to describe what is actually watched.
func (s ResolvedSourceScope) Fingerprint() string {
	parts := make([]string, 0, len(s.Items))
	for _, item := range s.Items {
		parts = append(parts, fmt.Sprintf("%d=%s", item.Index, strings.Join(item.Namespaces, ",")))
	}
	return strings.Join(parts, ";")
}

// ResolveWatchRuleSourceScope is the WatchRule source-namespace gate: may each of this rule's items
// watch the source namespace it asks for?
//
// It is CROSS-OBJECT authorization — WatchRule → GitTarget → KubeProvider — so it is not
// expressible in CEL and is deliberately a reconciler check rather than a webhook
// (docs/spec/where-validation-lives.md). Like GitTargetAdmitted it runs on every reconcile, so a
// policy TIGHTENED after a rule was accepted revokes it.
//
// An item may watch a namespace only while the KubeProvider's spec.allowedGitTargets admit the
// GitTarget's namespace and give it that namespace in sourceNamespaces: a name, '{namespace}' for the
// GitTarget's own namespace (which is the WatchRule's), or "*" for every namespace, which is also
// what an item asking for "*" needs. The rule's own namespace is in scope only when a grant lists it.
// The source credential's RBAC stays the hard maximum: a request this gate permits still fails 403
// at the apiserver if the credential cannot read that namespace.
//
// A non-NotFound read error is returned as err so the caller requeues instead of tearing down a
// running stream on a transient apiserver failure.
func ResolveWatchRuleSourceScope(
	ctx context.Context,
	reader client.Reader,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
) (ResolvedSourceScope, error) {
	decision, scope, err := GitTargetSourceScope(ctx, reader, target)
	if err != nil {
		return ResolvedSourceScope{}, err
	}
	gate := &itemGate{rule: rule, target: target, decision: decision, scope: scope}
	items := make([]SourceNamespaceDecision, 0, len(rule.Spec.Rules))
	for i := range rule.Spec.Rules {
		items = append(items, gate.decide(i, &rule.Spec.Rules[i]))
	}
	return aggregateSourceScope(items), nil
}

// itemGate carries the per-rule inputs so each item's decision reads as one step. The KubeProvider
// is read once per rule: every item asks the same provider about the same GitTarget.
type itemGate struct {
	rule     *configv1alpha3.WatchRule
	target   *configv1alpha3.GitTarget
	decision Decision
	scope    configv1alpha3.SourceScope
}

// decide resolves ONE rule item.
func (g *itemGate) decide(index int, item *configv1alpha3.ResourceRule) SourceNamespaceDecision {
	base := SourceNamespaceDecision{Index: index, Requested: item.SourceNamespace}
	provider := g.target.KubeProviderKey()
	if !g.decision.Allowed {
		base.Reason = ReasonSourceNamespaceNotAllowed
		base.Message = fmt.Sprintf("%s: GitTarget %s/%s may not mirror through KubeProvider %s at all: %s",
			g.describeItem(index, item), g.target.Namespace, g.target.Name, provider, g.decision.Message)
		return base
	}

	requested := item.EffectiveSourceNamespace(g.rule.Namespace)
	if !g.scope.AllowsSourceNamespace(requested) {
		base.Reason = ReasonSourceNamespaceNotAllowed
		base.Message = fmt.Sprintf(
			"KubeProvider %s lets namespace %q watch %s (spec.allowedGitTargets[].sourceNamespaces); %s is not "+
				"allowed", provider, g.target.Namespace, describeScope(g.scope.SourceNamespaces),
			g.describeItem(index, item))
		return base
	}

	base.Allowed = true
	base.Reason = ReasonSourceNamespaceAllowed
	if item.IsSourceNamespaceWildcard() {
		// The empty namespace IS the cluster-wide collection: openTargetList and openTargetWatch branch on
		// it, and for a namespaced GVR it is the all-namespaces collection. It is a peer of any
		// named-namespace collection on the same type, never a replacement for one.
		base.Namespaces = []string{""}
		base.Message = fmt.Sprintf(
			"%s: KubeProvider %s grants sourceNamespaces \"*\", so this item watches every namespace its source "+
				"credential can read, as one cluster-wide stream", g.describeItem(index, item), provider)
		return base
	}
	base.Namespaces = []string{requested}
	base.Message = fmt.Sprintf(
		"%s: KubeProvider %s lets this item watch source namespace %q; whether it can is decided by that "+
			"credential's RBAC", g.describeItem(index, item), provider, requested)
	return base
}

// describeScope renders a source-namespace scope for a message.
func describeScope(namespaces []string) string {
	if len(namespaces) == 0 {
		return "no namespace"
	}
	return quoteAll(namespaces)
}

// describeItem names an item by index AND by what it selects. The index alone goes stale the moment
// somebody reorders the list while reading the message, so both are always present.
func (g *itemGate) describeItem(index int, item *configv1alpha3.ResourceRule) string {
	return fmt.Sprintf("spec.rules[%d] (resources %s, sourceNamespace %s)",
		index, strings.Join(item.Resources, ","), item.DescribeSourceNamespace(g.rule.Namespace))
}

// aggregateSourceScope folds the per-item decisions into the one SourceNamespaceAuthorized verdict
// the object publishes: any item denied is False / SourceNamespaceNotAllowed / Stalled=True, naming
// the first denied item; otherwise True / SourceNamespaceAllowed.
//
// There is no "cannot say yet" row. Every input is a control-plane object this reconcile already
// read, so an item that is not denied is decided.
func aggregateSourceScope(items []SourceNamespaceDecision) ResolvedSourceScope {
	out := ResolvedSourceScope{Items: items}
	if len(items) == 0 {
		out.Allowed = true
		out.Reason = ReasonSourceNamespaceAllowed
		out.Message = "no rule items to authorize"
		return out
	}

	for _, item := range items {
		if !item.Allowed {
			out.Reason = item.Reason
			out.Message = item.Message
			return out
		}
	}

	out.Allowed = true
	out.Reason = ReasonSourceNamespaceAllowed
	out.Message = summariseAdmitted(items)
	return out
}

func summariseAdmitted(items []SourceNamespaceDecision) string {
	all := make([]string, 0, len(items))
	for _, item := range items {
		for _, ns := range item.Namespaces {
			if ns == "" {
				all = append(all, "every namespace (cluster-wide)")
				continue
			}
			all = append(all, ns)
		}
	}
	slices.Sort(all)
	all = slices.Compact(all)
	return fmt.Sprintf("all %d rule item(s) are authorized; watching source namespace(s) %s",
		len(items), strings.Join(all, ", "))
}
