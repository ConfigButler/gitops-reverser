// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Reasons for the WatchRule SourceNamespaceAuthorized condition. They are the rule-side names, so
// a reader never has to know which of the three gate inputs produced the verdict — the Message
// carries that.
const (
	// ReasonLegacySourceNamespace is the True reason when the rule watches its OWN namespace and
	// the GitTarget declares no allowedSourceNamespaces policy. No authorization was needed.
	ReasonLegacySourceNamespace = "LegacySourceNamespace"

	// ReasonSourceNamespaceAllowed is the True reason when the effective source namespace passed
	// the policy — including an own-namespace rule that a DECLARED policy explicitly admits.
	ReasonSourceNamespaceAllowed = "SourceNamespaceAllowed"

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

// SourceNamespaceResolver evaluates a GitTarget's allowedSourceNamespaces against a namespace in
// that target's SOURCE cluster. It is an interface here — and implemented by the watch manager —
// because the labels a selector needs live in the source cluster, whose connection and cache the
// watch manager already owns. A reconciler that dialled the source cluster itself on every pass
// would duplicate both.
//
// Implementations MUST answer an exact-NAME policy without consulting the label cache, so a source
// cluster whose Namespace access is denied still supports name-based policies. That degradation
// path is deliberate, and it is the half most likely to regress unnoticed.
type SourceNamespaceResolver interface {
	ResolveSourceNamespace(
		ctx context.Context,
		target *configv1alpha3.GitTarget,
		namespace string,
	) SourceScopeResult
}

// SourceNamespaceDecision is the outcome of the WatchRule source-namespace gate.
type SourceNamespaceDecision struct {
	// Namespace is the effective source namespace the gate ruled on.
	Namespace string
	// Verdict is admitted / denied / cannot-say-yet / permanently-unevaluatable.
	Verdict SourceScopeVerdict
	// Reason is the SourceNamespaceAuthorized condition reason.
	Reason string
	// Message explains the verdict to an operator.
	Message string
}

// Admitted reports whether the rule may compile.
func (d SourceNamespaceDecision) Admitted() bool { return d.Verdict == SourceScopeAdmitted }

// Terminal reports whether the verdict is a REFUSAL the controller should publish as Stalled=True
// while establishing a grant — as opposed to a retryable "cannot say yet". A permanently
// unevaluatable policy is terminal here only because this gate ESTABLISHES grants; a caller
// maintaining an already-resolved scope must retain it instead (see the establishing/maintaining
// contract in the PR 4 design), which is why that decision is the caller's and not encoded here.
func (d SourceNamespaceDecision) Terminal() bool {
	return d.Verdict == SourceScopeDenied || d.Verdict == SourceScopeUnavailable
}

// WatchRuleSourceNamespaceAdmitted is the WatchRule source-namespace gate: may this rule watch its
// effective source namespace in its GitTarget's source cluster?
//
// It is CROSS-OBJECT authorization — WatchRule → GitTarget → ClusterProvider — and the selector
// half needs remote state, so it is not expressible in CEL and is deliberately a reconciler check
// rather than a webhook (docs/spec/where-validation-lives.md). Like GitTargetAdmitted it runs on
// every reconcile, so a policy TIGHTENED after a rule was accepted revokes it.
//
// The ordering is the contract:
//
//  1. Own namespace + NO declared GitTarget policy → allowed, with no delegation flag and no
//     policy. This is the legacy case and it must stay free: gating it would break every existing
//     WatchRule on upgrade.
//  2. A DIFFERENT namespace additionally requires the GitTarget's namespace to be admitted by its
//     ClusterProvider, and that provider to set allowWatchRuleSourceNamespaceOverride.
//  3. Whenever a policy is declared it is EXHAUSTIVE — evaluated even for an own-namespace rule,
//     with no self-namespace carve-out — and an override against a target with NO policy is
//     denied by default.
//
// A non-NotFound ClusterProvider read error is returned as err so the caller requeues instead of
// tearing down a running stream on a transient apiserver failure.
func WatchRuleSourceNamespaceAdmitted(
	ctx context.Context,
	reader client.Reader,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
	resolver SourceNamespaceResolver,
) (SourceNamespaceDecision, error) {
	effective := rule.EffectiveSourceNamespace()

	// (1) The legacy case: own namespace, no policy. Free, and it must stay free.
	if !rule.OverridesSourceNamespace() && !target.DeclaresSourceNamespacePolicy() {
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeAdmitted,
			Reason:    ReasonLegacySourceNamespace,
			Message: fmt.Sprintf(
				"watching this WatchRule's own namespace %q; the GitTarget declares no "+
					"allowedSourceNamespaces policy, so no authorization is required",
				effective),
		}, nil
	}

	// (2) An override needs provider admission of the target AND the explicit delegation.
	if rule.OverridesSourceNamespace() {
		refusal, refused, err := overrideDelegated(ctx, reader, rule, target, effective)
		if err != nil {
			return SourceNamespaceDecision{}, err
		}
		if refused {
			return refusal, nil
		}
	}

	// (3) A declared policy is exhaustive; an override against no policy is denied by default.
	if !target.DeclaresSourceNamespacePolicy() {
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeDenied,
			Reason:    ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"GitTarget %s/%s declares no spec.allowedSourceNamespaces, so no source namespace "+
					"other than a rule's own may be mirrored into it; add %q to that policy",
				target.Namespace, target.Name, effective),
		}, nil
	}

	return evaluatePolicy(ctx, rule, target, effective, resolver), nil
}

// overrideDelegated applies the two provider-side halves of the gate to an override: the provider
// must admit the GitTarget's own namespace, and it must set the delegation flag.
//
// It returns refused=true with the refusal to publish, or refused=false when the caller should
// carry on to the GitTarget policy. A read error is returned as err so the caller requeues.
func overrideDelegated(
	ctx context.Context,
	reader client.Reader,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
	effective string,
) (SourceNamespaceDecision, bool, error) {
	providerName := target.SourceCluster()

	var provider configv1alpha3.ClusterProvider
	if err := reader.Get(ctx, k8stypes.NamespacedName{Name: providerName}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return SourceNamespaceDecision{
				Namespace: effective,
				Verdict:   SourceScopeDenied,
				Reason:    ReasonSourceNamespaceNotAllowed,
				Message: fmt.Sprintf(
					"referenced ClusterProvider %q was not found, so it delegates nothing; a "+
						"WatchRule may watch a namespace other than its own only through an "+
						"existing provider that sets spec.allowWatchRuleSourceNamespaceOverride",
					providerName),
			}, true, nil
		}
		// Transient: requeue rather than tear down a running stream.
		return SourceNamespaceDecision{}, false,
			fmt.Errorf("read ClusterProvider %q: %w", providerName, err)
	}

	// The GitTarget itself must be admitted by that provider before it can delegate anything.
	admitted, err := GitTargetAdmitted(ctx, reader, target)
	if err != nil {
		return SourceNamespaceDecision{}, false, err
	}
	if !admitted.Allowed {
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeDenied,
			Reason:    ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"GitTarget %s/%s may not mirror through ClusterProvider %q at all: %s",
				target.Namespace, target.Name, providerName, admitted.Message),
		}, true, nil
	}

	if !provider.AllowsWatchRuleSourceNamespaceOverride() {
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeDenied,
			Reason:    ReasonSourceNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"WatchRule %s/%s requests source namespace %q, but ClusterProvider %q does not set "+
					"spec.allowWatchRuleSourceNamespaceOverride; a WatchRule may watch only its own "+
					"namespace %q until a platform admin delegates that choice",
				rule.Namespace, rule.Name, effective, providerName, rule.Namespace),
		}, true, nil
	}

	return SourceNamespaceDecision{}, false, nil
}

// evaluatePolicy runs the GitTarget's declared allowedSourceNamespaces through the resolver and
// maps its three-valued answer onto the condition's reasons.
func evaluatePolicy(
	ctx context.Context,
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
	effective string,
	resolver SourceNamespaceResolver,
) SourceNamespaceDecision {
	result := resolveWith(ctx, resolver, target, effective)

	switch result.Verdict {
	case SourceScopeAdmitted:
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeAdmitted,
			Reason:    ReasonSourceNamespaceAllowed,
			Message: fmt.Sprintf(
				"source namespace %q is admitted by GitTarget %s/%s spec.allowedSourceNamespaces",
				effective, target.Namespace, target.Name),
		}
	case SourceScopeDenied:
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeDenied,
			Reason:    ReasonSourceNamespaceNotAllowed,
			Message:   deniedMessage(rule, target, effective, result.Message),
		}
	case SourceScopeUnavailable:
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeUnavailable,
			Reason:    ReasonSourceNamespacePolicyUnavailable,
			Message: fmt.Sprintf(
				"GitTarget %s/%s spec.allowedSourceNamespaces cannot be evaluated for %q: %s",
				target.Namespace, target.Name, effective, result.Message),
		}
	case SourceScopeUnknown:
		fallthrough
	default:
		return SourceNamespaceDecision{
			Namespace: effective,
			Verdict:   SourceScopeUnknown,
			Reason:    ReasonCheckingSourceNamespacePolicy,
			Message: fmt.Sprintf(
				"still establishing whether source namespace %q is admitted by GitTarget %s/%s: %s",
				effective, target.Namespace, target.Name, result.Message),
		}
	}
}

// deniedMessage names the SPECIFIC fix, which matters most in the case the design calls a genuine
// authoring footgun: declaring a policy for one override silently denies a co-resident LEGACY rule
// unless its own namespace is listed. A denial you are told about, in the terms of the fix, is the
// price of a field that means what it says — so that case gets its own wording.
func deniedMessage(
	rule *configv1alpha3.WatchRule,
	target *configv1alpha3.GitTarget,
	effective string,
	detail string,
) string {
	if !rule.OverridesSourceNamespace() {
		return fmt.Sprintf(
			"namespace %s is not in the GitTarget's allowedSourceNamespaces; add it to keep "+
				"watching this rule's own namespace (GitTarget %s/%s declares a policy, and a "+
				"declared policy is exhaustive — there is no self-namespace exception)",
			effective, target.Namespace, target.Name)
	}
	msg := fmt.Sprintf(
		"source namespace %q is not admitted by GitTarget %s/%s spec.allowedSourceNamespaces; "+
			"add it to that policy",
		effective, target.Namespace, target.Name)
	if detail != "" {
		msg += ": " + detail
	}
	return msg
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
