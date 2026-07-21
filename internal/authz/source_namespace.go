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

// Reasons for the WatchRule SourceNamespaceAuthorized condition. They are the rule-side names, so
// a reader never has to know which of the three gate inputs produced the verdict — the Message
// carries that.
const (
	// ReasonLegacySourceNamespace is the True reason when EVERY item watches the rule's OWN
	// namespace and the GitTarget declares no allowedSourceNamespaces policy. No authorization was
	// needed.
	ReasonLegacySourceNamespace = "LegacySourceNamespace"

	// ReasonSourceNamespaceAllowed is the True reason when every item passed the policy and at
	// least one names a namespace other than the rule's own — including an own-namespace item that
	// a DECLARED policy explicitly admits.
	ReasonSourceNamespaceAllowed = "SourceNamespaceAllowed"

	// ReasonNoAdmittedSourceNamespaces is the True reason when every item was admitted but the
	// resolved scope is EMPTY — a "*" item against a policy that currently admits nothing. The rule
	// is not stalled (nothing is wrong with it) but it mirrors nothing, and a rule that mirrors
	// nothing while reporting Ready=True with no explanation is a silent no-op.
	ReasonNoAdmittedSourceNamespaces = "NoAdmittedSourceNamespaces"

	// ReasonSourceNamespaceNotAllowed is the TERMINAL False reason for a refusal: the delegation
	// flag is off, the GitTarget declares no policy for an override, or a declared policy
	// evaluated and does not admit the namespace. The policy was READ; this is a decision, not an
	// inability to decide, and it must never share a code path with the unevaluatable case.
	ReasonSourceNamespaceNotAllowed = "SourceNamespaceNotAllowed"

	// ReasonSourceNamespacePolicyUnavailable is the reason when a SELECTOR policy cannot be
	// evaluated at all — an invalid selector, or Namespace reads permanently Forbidden on the
	// source cluster. Its STATUS depends on whether a scope was ever established for the rule:
	// False/Stalled=True while establishing (nothing runs, and only an operator change will fix
	// it), Unknown/Stalled=False while maintaining an already-resolved scope (which is retained).
	ReasonSourceNamespacePolicyUnavailable = "SourceNamespacePolicyUnavailable"

	// ReasonCheckingSourceNamespacePolicy is the Unknown reason while the answer is still being
	// established: the source-cluster Namespace cache has not synced, or a retryable read error is
	// being retried. It is NOT a denial — encoding "cannot say yet" as "denied" is exactly how a
	// transient outage becomes a terminal Stalled=True and a stopped stream.
	ReasonCheckingSourceNamespacePolicy = "CheckingSourceNamespacePolicy"

	// ReasonSourceNamespaceFieldRemoved is the TERMINAL False reason for a STORED WatchRule that
	// still carries the removed top-level spec.sourceNamespace. Admission rejects the field, but an
	// object written before this release keeps its value in etcd; refusing it here is what stops
	// the rule from silently watching its own namespace instead of the one it asked for.
	ReasonSourceNamespaceFieldRemoved = "SourceNamespaceFieldRemoved"
)

// SourceScopeVerdict is the THREE-valued answer a source-namespace policy evaluation produces.
// Three-valued is the whole point: a two-valued interface forces "cannot say" to be encoded as
// "denied", which turns a transient source-cluster outage into a terminal failure and a stopped
// stream — and makes the Unknown row of the status contract unimplementable.
type SourceScopeVerdict int

const (
	// SourceScopeUnknown means the policy could not be evaluated YET and the cause is retryable
	// (cache still syncing, source cluster momentarily unreachable). Retry; do not deny.
	SourceScopeUnknown SourceScopeVerdict = iota
	// SourceScopeAdmitted means the policy was evaluated and admits the namespace.
	SourceScopeAdmitted
	// SourceScopeDenied means the policy was evaluated and does NOT admit the namespace.
	SourceScopeDenied
	// SourceScopeUnavailable means the policy can never be evaluated as written without an
	// operator change — an invalid selector, or Namespace reads Forbidden for a selector policy.
	SourceScopeUnavailable
)

// SourceScopeResult is a policy evaluation's outcome plus an operator-legible explanation.
type SourceScopeResult struct {
	Verdict SourceScopeVerdict
	Message string
}

// SourceNamespaceResolver evaluates a GitTarget's allowedSourceNamespaces against namespaces in
// that target's SOURCE cluster. It is an interface here — and implemented by the watch manager —
// because the labels a selector needs live in the source cluster, whose connection and cache the
// watch manager already owns. A reconciler that dialled the source cluster itself on every pass
// would duplicate both.
//
// Implementations MUST answer an exact-NAME policy without consulting the label cache, so a source
// cluster whose Namespace access is denied still supports name-based policies. That degradation
// path is deliberate, and it is the half most likely to regress unnoticed.
type SourceNamespaceResolver interface {
	// ResolveSourceNamespace answers whether ONE candidate namespace is admitted.
	ResolveSourceNamespace(
		ctx context.Context,
		target *configv1alpha3.GitTarget,
		namespace string,
	) SourceScopeResult

	// EnumerateSourceNamespaces expands a target's SELECTOR half into the concrete set of source
	// namespaces it currently admits. It answers the "*" case, which has no single candidate to
	// test.
	//
	// The returned slice is meaningful only when the result is SourceScopeAdmitted; an empty slice
	// with that verdict is a real answer ("the selector currently admits nothing"), which is
	// exactly why the verdict must not be inferred from the length. Unknown and Unavailable mean
	// the set could not be computed and MUST NOT be read as the empty set — an empty resolved scope
	// is the input to a resync sweep.
	EnumerateSourceNamespaces(
		ctx context.Context,
		target *configv1alpha3.GitTarget,
	) ([]string, SourceScopeResult)
}

// SourceNamespaceDecision is one rule ITEM's source-namespace verdict, plus the concrete namespace
// set it resolved to.
type SourceNamespaceDecision struct {
	// Index is the item's position in spec.rules.
	Index int
	// Requested is what the item asked for, verbatim: "" (omitted), a name, or "*".
	Requested string
	// Namespaces is the RESOLVED, concrete namespace set for this item. It is meaningful only when
	// the verdict is admitted, and it is deliberately allowed to be empty for a wildcard whose
	// policy currently admits nothing.
	Namespaces []string
	// Verdict is admitted / denied / cannot-say-yet / permanently-unevaluatable.
	Verdict SourceScopeVerdict
	// Reason is the SourceNamespaceAuthorized condition reason this item would produce.
	Reason string
	// Message explains the verdict to an operator.
	Message string
}

// Admitted reports whether this item may contribute selections.
func (d SourceNamespaceDecision) Admitted() bool { return d.Verdict == SourceScopeAdmitted }

// Terminal reports whether the verdict is a REFUSAL the controller should publish as Stalled=True
// while establishing a grant — as opposed to a retryable "cannot say yet". A permanently
// unevaluatable policy is terminal here only because this gate ESTABLISHES grants; a caller
// maintaining an already-resolved scope must retain it instead (see the establishing/maintaining
// contract in the PR 4 design), which is why that decision is the caller's and not encoded here.
func (d SourceNamespaceDecision) Terminal() bool {
	return d.Verdict == SourceScopeDenied || d.Verdict == SourceScopeUnavailable
}

// ResolvedSourceScope is a WHOLE WatchRule's source-namespace verdict: one decision per spec.rules
// item, index-aligned, plus the aggregate the SourceNamespaceAuthorized condition publishes.
//
// It is a pure function of (rule spec, target policy, source Namespace snapshot), recomputed on
// every compile and replaced atomically. Nothing per-item is persisted across a spec change, which
// is what lets rule items have no stable API identity: no state outlives the spec that produced it.
type ResolvedSourceScope struct {
	// Items is index-aligned with spec.rules.
	Items []SourceNamespaceDecision
	// Verdict is the aggregate over Items, per the status contract's reason precedence.
	Verdict SourceScopeVerdict
	// Reason is the aggregate SourceNamespaceAuthorized reason.
	Reason string
	// Message explains the aggregate, naming the deciding item when one item decided it.
	Message string
}

// Admitted reports whether the whole rule may compile.
func (s ResolvedSourceScope) Admitted() bool { return s.Verdict == SourceScopeAdmitted }

// Terminal reports whether the aggregate is a refusal rather than a retryable "cannot say yet".
func (s ResolvedSourceScope) Terminal() bool {
	return s.Verdict == SourceScopeDenied || s.Verdict == SourceScopeUnavailable
}

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
// This is the SILENT hazard the design calls out: a wildcard's inputs — the GitTarget policy and
// the source cluster's Namespace labels — are not rule state, so a mapper that merely requeues the
// WatchRule is not enough. If the fingerprint hashed the rule spec instead of the RESOLVED set,
// reconciliation would run, the fingerprint would be unchanged, the table rebuild would be skipped,
// and every stream would carry on at its old width with no visible failure anywhere.
func (s ResolvedSourceScope) Fingerprint() string {
	parts := make([]string, 0, len(s.Items))
	for _, item := range s.Items {
		parts = append(parts, fmt.Sprintf("%d=%s", item.Index, strings.Join(item.Namespaces, ",")))
	}
	return strings.Join(parts, ";")
}

// ResolveWatchRuleSourceScope is the WatchRule source-namespace gate: which source-cluster
// namespaces may each of this rule's items watch, in its GitTarget's source cluster?
//
// It is CROSS-OBJECT authorization — WatchRule → GitTarget → ClusterProvider — and the selector
// half needs remote state, so it is not expressible in CEL and is deliberately a reconciler check
// rather than a webhook (docs/spec/where-validation-lives.md). Like GitTargetAdmitted it runs on
// every reconcile, so a policy TIGHTENED after a rule was accepted revokes it.
//
// The per-candidate ordering is the contract, unchanged from the single-namespace gate it
// generalizes:
//
//  1. Own namespace + NO declared GitTarget policy → allowed, with no delegation flag and no
//     policy. This is the legacy case and it must stay free: gating it would break every existing
//     WatchRule on upgrade.
//  2. A DIFFERENT namespace — including "*" — additionally requires the GitTarget's namespace to be
//     admitted by its ClusterProvider, and that provider to set allowSourceNamespaceOverride.
//  3. Whenever a policy is declared it is EXHAUSTIVE — evaluated even for an own-namespace item,
//     with no self-namespace carve-out — and an override against a target with NO policy is
//     denied by default.
//
// A non-NotFound ClusterProvider read error is returned as err so the caller requeues instead of
// tearing down a running stream on a transient apiserver failure.
func ResolveWatchRuleSourceScope(
	ctx context.Context,
	reader client.Reader,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
	resolver SourceNamespaceResolver,
) (ResolvedSourceScope, error) {
	// A STORED object carrying the removed top-level field is refused before anything else: it
	// asked for a namespace this controller no longer reads, and resolving the items as if it had
	// not asked is precisely the silent scope change this design exists to remove.
	if rule.DeclaresRemovedSourceNamespace() {
		return removedFieldScope(rule), nil
	}

	gate := &itemGate{reader: reader, rule: rule, target: target, resolver: resolver}
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

// removedFieldScope is the terminal refusal for a stored spec.sourceNamespace. Reading the
// deprecated field is the entire point: it is retained precisely so a stored value stays visible to
// Go and can be refused instead of silently ignored.
func removedFieldScope(rule *configv1alpha3.WatchRule) ResolvedSourceScope {
	msg := fmt.Sprintf(
		"WatchRule %s/%s still sets spec.sourceNamespace (%q): that field moved to "+
			"spec.rules[].sourceNamespace; move the value onto the rule items it applies to",
		rule.Namespace, rule.Name, rule.Spec.SourceNamespace) //nolint:staticcheck // deliberate: see above
	return ResolvedSourceScope{
		Verdict: SourceScopeDenied,
		Reason:  ReasonSourceNamespaceFieldRemoved,
		Message: msg,
	}
}

// itemGate carries the per-rule inputs so each item's decision reads as one step rather than a
// six-argument call. The ClusterProvider verdict is memoised: every overriding item asks the same
// question of the same provider, and re-reading it per item would multiply apiserver reads by the
// rule's item count for an answer that cannot differ within one compile.
type itemGate struct {
	reader   client.Reader
	rule     *configv1alpha3.WatchRule
	target   *configv1alpha3.GitTarget
	resolver SourceNamespaceResolver

	delegation      *SourceNamespaceDecision
	delegationAsked bool
}

// decide resolves ONE rule item: its requested namespace (or wildcard) through the three-part gate.
func (g *itemGate) decide(
	ctx context.Context,
	index int,
	item *configv1alpha3.ResourceRule,
) (SourceNamespaceDecision, error) {
	base := SourceNamespaceDecision{Index: index, Requested: item.SourceNamespace}
	overrides := item.OverridesSourceNamespace(g.rule.Namespace)

	// (1) The legacy case: own namespace, no policy. Free, and it must stay free.
	if !overrides && !g.target.DeclaresSourceNamespacePolicy() {
		own := item.EffectiveSourceNamespace(g.rule.Namespace)
		base.Namespaces = []string{own}
		base.Verdict = SourceScopeAdmitted
		base.Reason = ReasonLegacySourceNamespace
		base.Message = fmt.Sprintf(
			"%s watches this WatchRule's own namespace %q; the GitTarget declares no "+
				"allowedSourceNamespaces policy, so no authorization is required",
			g.describeItem(index, item), own)
		return base, nil
	}

	// (2) Anything other than the rule's own namespace needs provider admission of the target AND
	//     the explicit delegation.
	if overrides {
		refusal, refused, err := g.overrideDelegated(ctx, index, item)
		if err != nil {
			return SourceNamespaceDecision{}, err
		}
		if refused {
			return refusal, nil
		}
	}

	// (3) A declared policy is exhaustive; an override against no policy is denied by default.
	if !g.target.DeclaresSourceNamespacePolicy() {
		base.Verdict = SourceScopeDenied
		base.Reason = ReasonSourceNamespaceNotAllowed
		base.Message = fmt.Sprintf(
			"%s: GitTarget %s/%s declares no spec.allowedSourceNamespaces, so no source namespace "+
				"other than this WatchRule's own may be mirrored into it; declare that policy and "+
				"add %s to it",
			g.describeItem(index, item), g.target.Namespace, g.target.Name,
			item.DescribeSourceNamespace(g.rule.Namespace))
		return base, nil
	}

	// (4) A declared policy whose names could never BE namespace names is unevaluatable, not
	//     narrower. The schema rejects them at admission, but a policy stored before that validation
	//     shipped is still in etcd — and the two ways to carry on are both worse than refusing:
	//     honouring the valid subset silently mirrors less than the operator asked for, and
	//     resolving a wildcard THROUGH such a name produces a scope pointing at a namespace that
	//     cannot exist. Unavailable is the accurate verdict: only an operator edit fixes it, and the
	//     establishing/maintaining contract already handles it correctly in both directions.
	if err := g.target.Spec.AllowedSourceNamespaces.ValidateNames(); err != nil {
		base.Verdict = SourceScopeUnavailable
		base.Reason = ReasonSourceNamespacePolicyUnavailable
		base.Message = fmt.Sprintf(
			"%s: GitTarget %s/%s spec.allowedSourceNamespaces cannot be evaluated: %v; a policy "+
				"admits namespaces by NAME or by selector, never by pattern — use `selector: {}` to "+
				"admit every source namespace",
			g.describeItem(index, item), g.target.Namespace, g.target.Name, err)
		return base, nil
	}

	if item.IsSourceNamespaceWildcard() {
		return g.expandWildcard(ctx, index, item, base), nil
	}
	return g.evaluateCandidate(ctx, index, item, base), nil
}

// overrideDelegated applies the two provider-side halves of the gate: the provider must admit the
// GitTarget's own namespace, and it must set the delegation flag. Its verdict is identical for
// every item, so it is computed once per rule.
//
// It returns refused=true with the refusal to publish (retargeted at this item), or refused=false
// when the caller should carry on to the GitTarget policy. A read error is returned as err so the
// caller requeues.
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
				Verdict: SourceScopeDenied,
				Reason:  ReasonSourceNamespaceNotAllowed,
				Message: fmt.Sprintf(
					"referenced ClusterProvider %q was not found, so it delegates nothing; a "+
						"WatchRule may watch a namespace other than its own only through an "+
						"existing provider that sets spec.allowSourceNamespaceOverride",
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
			Verdict: SourceScopeDenied,
			Reason:  ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"GitTarget %s/%s may not mirror through ClusterProvider %q at all: %s",
				g.target.Namespace, g.target.Name, providerName, admitted.Message),
		}, nil
	}

	if !provider.AllowsSourceNamespaceOverride() {
		return &SourceNamespaceDecision{
			Verdict: SourceScopeDenied,
			Reason:  ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"ClusterProvider %q does not set spec.allowSourceNamespaceOverride; a WatchRule may "+
					"watch only its own namespace %q until a platform admin delegates that choice",
				providerName, g.rule.Namespace),
		}, nil
	}

	return nil, nil //nolint:nilnil // nil refusal means "the provider side permits"; see the godoc.
}

// evaluateCandidate runs ONE named candidate through the GitTarget's declared policy and maps the
// three-valued answer onto the condition's reasons.
func (g *itemGate) evaluateCandidate(
	ctx context.Context,
	index int,
	item *configv1alpha3.ResourceRule,
	base SourceNamespaceDecision,
) SourceNamespaceDecision {
	candidate := item.EffectiveSourceNamespace(g.rule.Namespace)
	result := resolveWith(ctx, g.resolver, g.target, candidate)
	label := g.describeItem(index, item)

	switch result.Verdict {
	case SourceScopeAdmitted:
		base.Namespaces = []string{candidate}
		base.Verdict = SourceScopeAdmitted
		base.Reason = ReasonSourceNamespaceAllowed
		base.Message = fmt.Sprintf(
			"%s: source namespace %q is admitted by GitTarget %s/%s spec.allowedSourceNamespaces",
			label, candidate, g.target.Namespace, g.target.Name)
	case SourceScopeDenied:
		base.Verdict = SourceScopeDenied
		base.Reason = ReasonSourceNamespaceNotAllowed
		base.Message = label + ": " + g.deniedMessage(item, candidate, result.Message)
	case SourceScopeUnavailable:
		base.Verdict = SourceScopeUnavailable
		base.Reason = ReasonSourceNamespacePolicyUnavailable
		base.Message = fmt.Sprintf(
			"%s: GitTarget %s/%s spec.allowedSourceNamespaces cannot be evaluated for %q: %s",
			label, g.target.Namespace, g.target.Name, candidate, result.Message)
	case SourceScopeUnknown:
		fallthrough
	default:
		base.Verdict = SourceScopeUnknown
		base.Reason = ReasonCheckingSourceNamespacePolicy
		base.Message = fmt.Sprintf(
			"%s: still establishing whether source namespace %q is admitted by GitTarget %s/%s: %s",
			label, candidate, g.target.Namespace, g.target.Name, result.Message)
	}
	return base
}

// expandWildcard resolves a "*" item to exactly the set the GitTarget's policy admits — never to
// every namespace that exists.
//
// The names half is answered here, with no source-cluster access at all, so a "*" against a
// names-only policy keeps resolving on a cluster whose Namespace list is Forbidden. That
// degradation path is the half most likely to regress unnoticed. The selector half needs the
// snapshot, and a selector that cannot be evaluated yields Unknown/Unavailable for the whole item
// rather than the names it did manage to read: a partial set would silently narrow the watch.
func (g *itemGate) expandWildcard(
	ctx context.Context,
	index int,
	item *configv1alpha3.ResourceRule,
	base SourceNamespaceDecision,
) SourceNamespaceDecision {
	label := g.describeItem(index, item)
	policy := g.target.Spec.AllowedSourceNamespaces
	admitted := append([]string(nil), policy.Names...)

	if policy.HasSelector() {
		selected, result := enumerateWith(ctx, g.resolver, g.target)
		switch result.Verdict {
		case SourceScopeAdmitted:
			admitted = append(admitted, selected...)
		case SourceScopeUnavailable:
			base.Verdict = SourceScopeUnavailable
			base.Reason = ReasonSourceNamespacePolicyUnavailable
			base.Message = fmt.Sprintf(
				"%s: GitTarget %s/%s spec.allowedSourceNamespaces cannot be enumerated for %q: %s",
				label, g.target.Namespace, g.target.Name, configv1alpha3.SourceNamespaceWildcard,
				result.Message)
			return base
		case SourceScopeDenied, SourceScopeUnknown:
			fallthrough
		default:
			base.Verdict = SourceScopeUnknown
			base.Reason = ReasonCheckingSourceNamespacePolicy
			base.Message = fmt.Sprintf(
				"%s: still enumerating which source namespaces GitTarget %s/%s admits: %s",
				label, g.target.Namespace, g.target.Name, result.Message)
			return base
		}
	}

	base.Namespaces = sortedUnique(admitted)
	base.Verdict = SourceScopeAdmitted
	base.Reason = ReasonSourceNamespaceAllowed
	if len(base.Namespaces) == 0 {
		base.Reason = ReasonNoAdmittedSourceNamespaces
		base.Message = fmt.Sprintf(
			"%s: GitTarget %s/%s spec.allowedSourceNamespaces currently admits no source namespace, "+
				"so this item watches nothing",
			label, g.target.Namespace, g.target.Name)
		return base
	}
	base.Message = fmt.Sprintf(
		"%s: expands to the %d source namespace(s) GitTarget %s/%s admits (%s)",
		label, len(base.Namespaces), g.target.Namespace, g.target.Name,
		strings.Join(base.Namespaces, ", "))
	return base
}

// deniedMessage names the SPECIFIC fix, which matters most in the case the design calls a genuine
// authoring footgun: declaring a policy for one item silently denies a co-resident LEGACY item
// unless its own namespace is listed. A denial you are told about, in the terms of the fix, is the
// price of a field that means what it says — so that case gets its own wording.
func (g *itemGate) deniedMessage(item *configv1alpha3.ResourceRule, candidate, detail string) string {
	if !item.OverridesSourceNamespace(g.rule.Namespace) {
		return fmt.Sprintf(
			"namespace %s is not in the GitTarget's allowedSourceNamespaces; add it to keep "+
				"watching this rule's own namespace (GitTarget %s/%s declares a policy, and a "+
				"declared policy is exhaustive — there is no self-namespace exception)",
			candidate, g.target.Namespace, g.target.Name)
	}
	msg := fmt.Sprintf(
		"source namespace %q is not admitted by GitTarget %s/%s spec.allowedSourceNamespaces; "+
			"add it to that policy",
		candidate, g.target.Namespace, g.target.Name)
	if detail != "" {
		msg += ": " + detail
	}
	return msg
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
//  2. any item permanently unevaluatable → False / SourceNamespacePolicyUnavailable / Stalled=True
//     (the caller downgrades this to Unknown when it is MAINTAINING a retained scope)
//  3. any item still resolving → Unknown / CheckingSourceNamespacePolicy
//  4. every item admitted, at least one naming a namespace other than the rule's own → True /
//     SourceNamespaceAllowed — or NoAdmittedSourceNamespaces when the whole resolved scope is empty
//  5. every item omitted → True / LegacySourceNamespace
func aggregateSourceScope(items []SourceNamespaceDecision) ResolvedSourceScope {
	out := ResolvedSourceScope{Items: items}
	if len(items) == 0 {
		out.Verdict = SourceScopeAdmitted
		out.Reason = ReasonLegacySourceNamespace
		out.Message = "no rule items to authorize"
		return out
	}

	for _, verdict := range []SourceScopeVerdict{SourceScopeDenied, SourceScopeUnavailable, SourceScopeUnknown} {
		if worst, ok := firstWithVerdict(items, verdict); ok {
			out.Verdict = worst.Verdict
			out.Reason = worst.Reason
			out.Message = worst.Message
			return out
		}
	}

	out.Verdict = SourceScopeAdmitted
	out.Reason, out.Message = admittedAggregate(items)
	return out
}

func firstWithVerdict(items []SourceNamespaceDecision, verdict SourceScopeVerdict) (SourceNamespaceDecision, bool) {
	for _, item := range items {
		if item.Verdict == verdict {
			return item, true
		}
	}
	return SourceNamespaceDecision{}, false
}

// admittedAggregate picks the True reason once every item was admitted. An empty resolved scope
// gets its own reason: the rule is not stalled, but a rule that mirrors nothing while reporting
// Ready=True with no explanation is a silent no-op.
func admittedAggregate(items []SourceNamespaceDecision) (string, string) {
	total := 0
	legacy := true
	for _, item := range items {
		total += len(item.Namespaces)
		if item.Reason != ReasonLegacySourceNamespace {
			legacy = false
		}
	}
	switch {
	case total == 0:
		return ReasonNoAdmittedSourceNamespaces,
			"every rule item is authorized, but the resolved source-namespace scope is empty, so " +
				"this WatchRule currently mirrors nothing"
	case legacy:
		return ReasonLegacySourceNamespace, items[0].Message
	default:
		return ReasonSourceNamespaceAllowed, summariseAdmitted(items)
	}
}

func summariseAdmitted(items []SourceNamespaceDecision) string {
	all := make([]string, 0, len(items))
	for _, item := range items {
		all = append(all, item.Namespaces...)
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

// resolveWith calls the resolver, treating a MISSING resolver as "cannot say yet" rather than as a
// denial. A nil resolver means the source-scope service is not wired (a zero-value manager in
// tests, or a controller running before the data plane is up); answering "denied" there would stop
// streams for an entirely unrelated reason. Exact-NAME policies are answered inline first so they
// remain usable with no resolver and no source-cluster access at all.
func resolveWith(
	ctx context.Context,
	resolver SourceNamespaceResolver,
	target *configv1alpha3.GitTarget,
	namespace string,
) SourceScopeResult {
	if target.Spec.AllowedSourceNamespaces.MatchesName(namespace) {
		return SourceScopeResult{
			Verdict: SourceScopeAdmitted,
			Message: "admitted by an exact name entry",
		}
	}
	if !target.Spec.AllowedSourceNamespaces.HasSelector() {
		// A declared policy with no selector and no matching name is a complete answer: nothing
		// remote is needed to know it denies.
		return SourceScopeResult{
			Verdict: SourceScopeDenied,
			Message: "the policy lists no matching name and declares no selector",
		}
	}
	if resolver == nil {
		return SourceScopeResult{
			Verdict: SourceScopeUnknown,
			Message: "no source-scope service is wired yet to evaluate the selector",
		}
	}
	return resolver.ResolveSourceNamespace(ctx, target, namespace)
}

// enumerateWith is the wildcard twin of resolveWith: it asks the resolver to expand the selector
// half, and treats a missing resolver as "cannot say yet".
func enumerateWith(
	ctx context.Context,
	resolver SourceNamespaceResolver,
	target *configv1alpha3.GitTarget,
) ([]string, SourceScopeResult) {
	if resolver == nil {
		return nil, SourceScopeResult{
			Verdict: SourceScopeUnknown,
			Message: "no source-scope service is wired yet to enumerate the selector",
		}
	}
	return resolver.EnumerateSourceNamespaces(ctx, target)
}
