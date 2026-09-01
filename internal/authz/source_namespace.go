// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Reasons for the WatchRule SourceNamespaceAuthorized condition. They are the rule-side names, so a
// reader never has to know which gate input produced the verdict — the Message carries that.
//
// There used to be five, and three of them existed only because the deleted
// GitTarget.spec.allowedSourceNamespaces had a SELECTOR half evaluated against Namespace labels in
// ANOTHER cluster: that read could be Forbidden, or still syncing, or permanently unevaluatable, so
// the verdict had to be three-valued and two of the reasons described an inability to decide rather
// than a decision. Nothing here reads another cluster any more. Every input is on the WatchRule, the
// GitTarget and the ClusterProvider, all in the control plane, so the gate is a comparison and its
// answer is always allowed or denied.
const (
	// ReasonLegacySourceNamespace is the True reason when EVERY item watches the rule's OWN
	// namespace. No authorization was needed.
	ReasonLegacySourceNamespace = "LegacySourceNamespace"

	// ReasonSourceNamespaceAllowed is the True reason when at least one item names a namespace
	// other than the rule's own — an authorized override or the cluster-wide wildcard — and every
	// item passed the gate.
	ReasonSourceNamespaceAllowed = "SourceNamespaceAllowed"

	// ReasonSourceNamespaceNotAllowed is the TERMINAL False reason for a refusal: the referenced
	// ClusterProvider does not exist, does not admit this GitTarget's namespace, or does not set
	// spec.allowAnySourceNamespace. It is a decision, never an inability to decide.
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
	// the cluster-wide cell. It is meaningful only when the item is admitted.
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
// It is a pure function of (rule spec, provider flags) and is recomputed on every compile. Nothing
// per-item is persisted across a spec change, which is what lets rule items have no stable API
// identity: no state outlives the spec that produced it.
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
// It is now derivable from the rule spec alone, since the resolution reads no cluster state. It is
// still computed from the RESOLVED set rather than the requested one, because the two differ for a
// wildcard — "*" resolves to the empty string — and the fingerprint's job is to describe what is
// actually watched.
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
// It is CROSS-OBJECT authorization — WatchRule → GitTarget → ClusterProvider — so it is not
// expressible in CEL and is deliberately a reconciler check rather than a webhook
// (docs/spec/where-validation-lives.md). Like GitTargetAdmitted it runs on every reconcile, so a
// policy TIGHTENED after a rule was accepted revokes it.
//
// The gate is two rules:
//
//  1. An item watching the rule's OWN namespace is allowed, with no provider involvement at all.
//     This is the legacy case and it must stay free: gating it would break every existing WatchRule
//     on upgrade.
//  2. Any OTHER namespace — a name, or "*" for every namespace the credential can read —
//     additionally requires the GitTarget's namespace to be admitted by its ClusterProvider, and
//     that provider to set spec.allowAnySourceNamespace.
//
// There is no third rule. GitTarget.spec.allowedSourceNamespaces used to supply one, and what it
// bounded is now bounded by the source credential's own RBAC: a request this gate permits still
// fails 403 at the apiserver if the credential cannot read that namespace, which is a better
// answer than a policy field that restates the credential in the one place that cannot revoke it.
//
// A non-NotFound ClusterProvider read error is returned as err so the caller requeues instead of
// tearing down a running stream on a transient apiserver failure.
func ResolveWatchRuleSourceScope(
	ctx context.Context,
	reader client.Reader,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
) (ResolvedSourceScope, error) {
	gate := &itemGate{reader: reader, rule: rule, target: target}
	items := make([]SourceNamespaceDecision, 0, len(rule.Spec.Rules))
	for i := range rule.Spec.Rules {
		decision, err := gate.decide(ctx, i, &rule.Spec.Rules[i])
		if err != nil {
			return ResolvedSourceScope{}, err
		}
		items = append(items, decision)
	}
	return aggregateSourceScope(items), nil
}

// itemGate carries the per-rule inputs so each item's decision reads as one step. The
// ClusterProvider verdict is memoised: every overriding item asks the same question of the same
// provider, and re-reading it per item would multiply apiserver reads by the rule's item count for
// an answer that cannot differ within one compile.
type itemGate struct {
	reader client.Reader
	rule   *configv1alpha3.WatchRule
	target *configv1alpha3.GitTarget

	delegation      *SourceNamespaceDecision
	delegationAsked bool
}

// decide resolves ONE rule item.
func (g *itemGate) decide(
	ctx context.Context,
	index int,
	item *configv1alpha3.ResourceRule,
) (SourceNamespaceDecision, error) {
	base := SourceNamespaceDecision{Index: index, Requested: item.SourceNamespace}

	if !item.OverridesSourceNamespace(g.rule.Namespace) {
		own := item.EffectiveSourceNamespace(g.rule.Namespace)
		base.Namespaces = []string{own}
		base.Allowed = true
		base.Reason = ReasonLegacySourceNamespace
		base.Message = fmt.Sprintf(
			"%s watches this WatchRule's own namespace %q, which needs no authorization",
			g.describeItem(index, item), own)
		return base, nil
	}

	refusal, refused, err := g.overrideDelegated(ctx, index, item)
	if err != nil {
		return SourceNamespaceDecision{}, err
	}
	if refused {
		return refusal, nil
	}

	base.Allowed = true
	base.Reason = ReasonSourceNamespaceAllowed
	if item.IsSourceNamespaceWildcard() {
		// The empty namespace IS the cluster-wide cell: openTargetList and openTargetWatch branch on
		// it, and for a namespaced GVR it is the all-namespaces collection. It is a peer of any
		// named-namespace cell on the same type, never a replacement for one.
		base.Namespaces = []string{""}
		base.Message = fmt.Sprintf(
			"%s: ClusterProvider %q sets spec.allowAnySourceNamespace, so this item watches every "+
				"namespace its source credential can read, as one cluster-wide stream",
			g.describeItem(index, item), g.target.SourceCluster())
		return base, nil
	}

	candidate := item.EffectiveSourceNamespace(g.rule.Namespace)
	base.Namespaces = []string{candidate}
	base.Message = fmt.Sprintf(
		"%s: ClusterProvider %q sets spec.allowAnySourceNamespace, so this item may watch source "+
			"namespace %q; whether it can is decided by that credential's RBAC",
		g.describeItem(index, item), g.target.SourceCluster(), candidate)
	return base, nil
}

// overrideDelegated applies the two provider-side halves of the gate: the provider must admit the
// GitTarget's own namespace, and it must set the delegation flag. Its verdict is identical for
// every item, so it is computed once per rule.
//
// It returns refused=true with the refusal to publish (retargeted at this item), or refused=false
// when the item is authorized. A read error is returned as err so the caller requeues.
func (g *itemGate) overrideDelegated(
	ctx context.Context,
	index int,
	item *configv1alpha3.ResourceRule,
) (SourceNamespaceDecision, bool, error) {
	if !g.delegationAsked {
		refusal, err := g.evaluateDelegation(ctx)
		if err != nil {
			return SourceNamespaceDecision{}, false, err
		}
		g.delegation = refusal
		g.delegationAsked = true
	}
	if g.delegation == nil {
		return SourceNamespaceDecision{}, false, nil
	}

	refusal := *g.delegation
	refusal.Index = index
	refusal.Requested = item.SourceNamespace
	refusal.Message = fmt.Sprintf("%s: %s", g.describeItem(index, item), refusal.Message)
	return refusal, true, nil
}

// evaluateDelegation returns a refusal template when the provider side of the gate denies, or nil
// when it permits. The message is item-agnostic; decide prefixes the item that asked.
func (g *itemGate) evaluateDelegation(ctx context.Context) (*SourceNamespaceDecision, error) {
	providerName := g.target.SourceCluster()

	var provider configv1alpha3.ClusterProvider
	if err := g.reader.Get(ctx, k8stypes.NamespacedName{Name: providerName}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return &SourceNamespaceDecision{
				Reason: ReasonSourceNamespaceNotAllowed,
				Message: fmt.Sprintf(
					"referenced ClusterProvider %q was not found, so it delegates nothing; a "+
						"WatchRule may watch a namespace other than its own only through an "+
						"existing provider that sets spec.allowAnySourceNamespace",
					providerName),
			}, nil
		}
		// Transient: requeue rather than tear down a running stream.
		return nil, fmt.Errorf("read ClusterProvider %q: %w", providerName, err)
	}

	// The GitTarget itself must be admitted by that provider before it can delegate anything.
	admitted, err := GitTargetAdmitted(ctx, g.reader, g.target)
	if err != nil {
		return nil, err
	}
	if !admitted.Allowed {
		return &SourceNamespaceDecision{
			Reason: ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"GitTarget %s/%s may not mirror through ClusterProvider %q at all: %s",
				g.target.Namespace, g.target.Name, providerName, admitted.Message),
		}, nil
	}

	if !provider.AllowsAnySourceNamespace() {
		return &SourceNamespaceDecision{
			Reason: ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"ClusterProvider %q does not set spec.allowAnySourceNamespace; a WatchRule may "+
					"watch only its own namespace %q until a platform admin delegates that choice",
				providerName, g.rule.Namespace),
		}, nil
	}

	return nil, nil //nolint:nilnil // nil refusal means "the provider side permits"; see the godoc.
}

// describeItem names an item by index AND by what it selects. The index alone goes stale the moment
// somebody reorders the list while reading the message, so both are always present.
func (g *itemGate) describeItem(index int, item *configv1alpha3.ResourceRule) string {
	return fmt.Sprintf("spec.rules[%d] (resources %s, sourceNamespace %s)",
		index, strings.Join(item.Resources, ","), item.DescribeSourceNamespace(g.rule.Namespace))
}

// aggregateSourceScope folds the per-item decisions into the one SourceNamespaceAuthorized verdict
// the object publishes. The precedence is stated rather than derived, because two implementations
// of "worst wins" would otherwise disagree about mixed rules:
//
//  1. any item denied → False / SourceNamespaceNotAllowed / Stalled=True
//  2. every item admitted, at least one naming a namespace other than the rule's own → True /
//     SourceNamespaceAllowed
//  3. every item on its own namespace → True / LegacySourceNamespace
//
// There is no "cannot say yet" row. Every input is a control-plane object this reconcile already
// read, so an item that is not denied is decided.
func aggregateSourceScope(items []SourceNamespaceDecision) ResolvedSourceScope {
	out := ResolvedSourceScope{Items: items}
	if len(items) == 0 {
		out.Allowed = true
		out.Reason = ReasonLegacySourceNamespace
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
	out.Reason, out.Message = admittedAggregate(items)
	return out
}

// admittedAggregate picks the True reason once every item was admitted.
func admittedAggregate(items []SourceNamespaceDecision) (string, string) {
	legacy := true
	for _, item := range items {
		if item.Reason != ReasonLegacySourceNamespace {
			legacy = false
		}
	}
	if legacy {
		return ReasonLegacySourceNamespace, items[0].Message
	}
	return ReasonSourceNamespaceAllowed, summariseAdmitted(items)
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
	all = sortedUnique(all)
	return fmt.Sprintf("all %d rule item(s) are authorized; watching source namespace(s) %s",
		len(items), strings.Join(all, ", "))
}

func sortedUnique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
