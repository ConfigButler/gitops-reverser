// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

const (
	snTenantNS   = "tenant-acme"
	snSourceNS   = "repo-config"
	snTargetName = "acme"
	snRuleName   = "repo-config-rule"
	snProvider   = "workspaces"
	snWildcard   = configv1alpha3.SourceNamespaceWildcard
)

func snScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, configv1alpha3.AddToScheme(s))
	return s
}

func snTarget(policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: snTargetName, Namespace: snTenantNS},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef:             configv1alpha3.GitProviderReference{Name: "acme-git"},
			ClusterProviderRef:      &configv1alpha3.ClusterProviderReference{Name: snProvider},
			Branch:                  "main",
			Path:                    "tenants/acme",
			AllowedSourceNamespaces: policy,
		},
	}
}

// snRule builds a WatchRule with one item per given sourceNamespace ("" = omitted).
func snRule(sourceNamespaces ...string) *configv1alpha3.WatchRule {
	items := make([]configv1alpha3.ResourceRule, 0, len(sourceNamespaces))
	for _, ns := range sourceNamespaces {
		items = append(items, configv1alpha3.ResourceRule{
			Resources: []string{"configmaps"}, SourceNamespace: ns,
		})
	}
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: snRuleName, Namespace: snTenantNS},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: snTargetName},
			Rules:     items,
		},
	}
}

// snClusterProvider admits snTenantNS by name, so the "provider admits the target's namespace" leg
// of the gate passes and each case isolates the leg it is actually about.
func snClusterProvider(delegate bool) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: snProvider},
		Spec: configv1alpha3.ClusterProviderSpec{
			AllowedNamespaces:            &configv1alpha3.NamespaceMatcher{Names: []string{snTenantNS}},
			AllowSourceNamespaceOverride: delegate,
		},
	}
}

// stubResolver is a source-scope service stand-in. The gate only ever asks it SELECTOR questions —
// authz answers the exact-name half itself — so a test that expects it to be consulted is also
// asserting that the name fast-path did not swallow the question.
type stubResolver struct {
	result      authz.SourceScopeResult
	enumerated  []string
	enumeration authz.SourceScopeResult
	calls       int
	enumCalls   int
}

func (s *stubResolver) ResolveSourceNamespace(
	context.Context, *configv1alpha3.GitTarget, string,
) authz.SourceScopeResult {
	s.calls++
	return s.result
}

func (s *stubResolver) EnumerateSourceNamespaces(
	context.Context, *configv1alpha3.GitTarget,
) ([]string, authz.SourceScopeResult) {
	s.enumCalls++
	return s.enumerated, s.enumeration
}

func admitting() *stubResolver {
	return &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeAdmitted}}
}

func denying() *stubResolver {
	return &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeDenied}}
}

func enumerating(names ...string) *stubResolver {
	return &stubResolver{
		enumerated:  names,
		enumeration: authz.SourceScopeResult{Verdict: authz.SourceScopeAdmitted},
	}
}

// resolve is the one-item shorthand every truth-table row uses.
func resolveOne(
	t *testing.T,
	sourceNamespace string,
	policy *configv1alpha3.NamespaceMatcher,
	delegate bool,
	resolver authz.SourceNamespaceResolver,
) authz.ResolvedSourceScope {
	t.Helper()
	target := snTarget(policy)
	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(
			target,
			snClusterProvider(delegate),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}},
		).
		Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(sourceNamespace), target, resolver)
	require.NoError(t, err)
	return resolved
}

// TestResolveWatchRuleSourceScope is the gate's truth table, per rule ITEM, modelled on
// TestCheckSourceAuthorization. The first two rows are the LEGACY guarantee: if they ever fail,
// deny-by-default has broken every existing WatchRule on upgrade.
func TestResolveWatchRuleSourceScope(t *testing.T) {
	labelled := map[string]string{"gitops.configbutler.ai/mirrorable": "true"}
	selectorPolicy := &configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: labelled},
	}

	tests := []struct {
		name string
		// sourceNamespace is spec.rules[0].sourceNamespace ("" = omitted).
		sourceNamespace string
		policy          *configv1alpha3.NamespaceMatcher
		delegate        bool
		resolver        *stubResolver
		wantVerdict     authz.SourceScopeVerdict
		wantReason      string
		wantNamespaces  []string
	}{{
		// THE test. An item that omits sourceNamespace must pass with no policy and no flag.
		name:            "omitted, no policy, flag false: allowed (legacy, and must stay free)",
		sourceNamespace: "",
		policy:          nil,
		delegate:        false,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonLegacySourceNamespace,
		wantNamespaces:  []string{snTenantNS},
	}, {
		name:            "explicitly equals its own namespace, no policy, flag false: allowed",
		sourceNamespace: snTenantNS,
		policy:          nil,
		delegate:        false,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonLegacySourceNamespace,
		wantNamespaces:  []string{snTenantNS},
	}, {
		// The no-self-namespace-exception rule: the case most likely to be implemented as an
		// accidental carve-out. A DECLARED policy is exhaustive, own namespace included.
		name:            "omitted, policy declared but omits the rule's own namespace: denied",
		sourceNamespace: "",
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}},
		delegate:        false,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "omitted, policy declared and lists the rule's own namespace: allowed",
		sourceNamespace: "",
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{snTenantNS}},
		delegate:        false,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
		wantNamespaces:  []string{snTenantNS},
	}, {
		name:            "differs, flag false: denied even though the target names it",
		sourceNamespace: snSourceNS,
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}},
		delegate:        false,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "differs, flag true, target policy absent: denied (deny-by-default)",
		sourceNamespace: snSourceNS,
		policy:          nil,
		delegate:        true,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "differs, flag true, target policy empty: denied (empty is not unrestricted)",
		sourceNamespace: snSourceNS,
		policy:          &configv1alpha3.NamespaceMatcher{},
		delegate:        true,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "differs, flag true, target names it: allowed",
		sourceNamespace: snSourceNS,
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}},
		delegate:        true,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
		wantNamespaces:  []string{snSourceNS},
	}, {
		name:            "differs, flag true, target selector matches: allowed",
		sourceNamespace: snSourceNS,
		policy:          selectorPolicy,
		delegate:        true,
		resolver:        admitting(),
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
		wantNamespaces:  []string{snSourceNS},
	}, {
		name:            "differs, flag true, target names a DIFFERENT namespace: denied",
		sourceNamespace: snSourceNS,
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{"someone-elses-namespace"}},
		delegate:        true,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "differs, flag true, selector does not match: denied",
		sourceNamespace: snSourceNS,
		policy:          selectorPolicy,
		delegate:        true,
		resolver:        denying(),
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		// An unevaluatable policy is NOT a refusal and must not share its code path.
		name:            "differs, flag true, selector permanently unevaluatable: policy unavailable",
		sourceNamespace: snSourceNS,
		policy:          selectorPolicy,
		delegate:        true,
		resolver: &stubResolver{result: authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnavailable, Message: "namespaces list is forbidden",
		}},
		wantVerdict: authz.SourceScopeUnavailable,
		wantReason:  authz.ReasonSourceNamespacePolicyUnavailable,
	}, {
		name:            "differs, flag true, selector answer not ready yet: retryable, not denied",
		sourceNamespace: snSourceNS,
		policy:          selectorPolicy,
		delegate:        true,
		resolver: &stubResolver{result: authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnknown, Message: "cache still syncing",
		}},
		wantVerdict: authz.SourceScopeUnknown,
		wantReason:  authz.ReasonCheckingSourceNamespacePolicy,
	}, {
		// "*" is deny-by-default too: it follows the policy's set, so with no policy there is no set.
		name:            "wildcard, flag true, no policy: denied — a wildcard is not a backdoor",
		sourceNamespace: snWildcard,
		policy:          nil,
		delegate:        true,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "wildcard, flag false: denied even with a policy that would admit",
		sourceNamespace: snWildcard,
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}},
		delegate:        false,
		wantVerdict:     authz.SourceScopeDenied,
		wantReason:      authz.ReasonSourceNamespaceNotAllowed,
	}, {
		name:            "wildcard against a names policy: expands to exactly those names",
		sourceNamespace: snWildcard,
		policy:          &configv1alpha3.NamespaceMatcher{Names: []string{"team-payments", snSourceNS}},
		delegate:        true,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
		wantNamespaces:  []string{snSourceNS, "team-payments"},
	}, {
		name:            "wildcard against a declared-but-empty policy: admits nothing, but is not a refusal",
		sourceNamespace: snWildcard,
		policy:          &configv1alpha3.NamespaceMatcher{},
		delegate:        true,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonNoAdmittedSourceNamespaces,
		wantNamespaces:  []string{},
	}, {
		name:            "wildcard against a selector policy: expands to the enumerated set",
		sourceNamespace: snWildcard,
		policy:          selectorPolicy,
		delegate:        true,
		resolver:        enumerating("beta", "alpha"),
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
		wantNamespaces:  []string{"alpha", "beta"},
	}, {
		name:            "wildcard, selector enumeration unavailable: never read as the empty set",
		sourceNamespace: snWildcard,
		policy:          selectorPolicy,
		delegate:        true,
		resolver: &stubResolver{enumeration: authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnavailable, Message: "namespaces list is forbidden",
		}},
		wantVerdict: authz.SourceScopeUnavailable,
		wantReason:  authz.ReasonSourceNamespacePolicyUnavailable,
	}, {
		name:            "wildcard, selector enumeration not ready: retryable, not denied",
		sourceNamespace: snWildcard,
		policy:          selectorPolicy,
		delegate:        true,
		resolver: &stubResolver{enumeration: authz.SourceScopeResult{
			Verdict: authz.SourceScopeUnknown, Message: "cache still syncing",
		}},
		wantVerdict: authz.SourceScopeUnknown,
		wantReason:  authz.ReasonCheckingSourceNamespacePolicy,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resolver authz.SourceNamespaceResolver
			if tc.resolver != nil {
				resolver = tc.resolver
			}

			resolved := resolveOne(t, tc.sourceNamespace, tc.policy, tc.delegate, resolver)

			assert.Equal(t, tc.wantVerdict, resolved.Verdict, "verdict (message: %s)", resolved.Message)
			assert.Equal(t, tc.wantReason, resolved.Reason)
			assert.NotEmpty(t, resolved.Message, "every verdict must carry an operator-legible message")
			require.Len(t, resolved.Items, 1)
			if tc.wantNamespaces != nil {
				assert.Equal(t, tc.wantNamespaces, resolved.NamespacesFor(0))
			}
		})
	}
}

// TestResolveWatchRuleSourceScope_MixedItemsResolveIndependently is the point of moving the field
// onto the items: one rule can follow configmaps in its own namespace, secrets in a named one, and
// deployments everywhere the target admits.
func TestResolveWatchRuleSourceScope_MixedItemsResolveIndependently(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{
		Names: []string{snTenantNS, snSourceNS, "team-payments"},
	})
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule("", snSourceNS, snWildcard), target, nil)

	require.NoError(t, err)
	require.True(t, resolved.Admitted(), "message: %s", resolved.Message)
	assert.Equal(t, authz.ReasonSourceNamespaceAllowed, resolved.Reason)
	assert.Equal(t, []string{snTenantNS}, resolved.NamespacesFor(0))
	assert.Equal(t, []string{snSourceNS}, resolved.NamespacesFor(1))
	assert.Equal(t, []string{snSourceNS, "team-payments", snTenantNS}, resolved.NamespacesFor(2))
}

// TestResolveWatchRuleSourceScope_DeniedItemRefusesTheWholeRule is decision 5: a denied explicit
// name is never trimmed away and run as a partial rule. Mirroring two of the three namespaces a rule
// asked for is worse than a loud failure — and the message must name the offending item.
func TestResolveWatchRuleSourceScope_DeniedItemRefusesTheWholeRule(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snTenantNS, snSourceNS}})
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule("", snSourceNS, "tenant-zen"), target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict)
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
	assert.Contains(t, resolved.Message, "spec.rules[2]",
		"the aggregate message must name the failing item by index...")
	assert.Contains(t, resolved.Message, "tenant-zen",
		"...and by what it asked for, because an index alone goes stale on a reorder")
}

// TestResolveWatchRuleSourceScope_EmptyWildcardIsVisibleNotStalled is the other half of decision 5:
// a "*" that currently admits nothing is valid and does not stall the rule, but it must not read as
// healthy either — a rule that mirrors nothing while reporting Ready=True is a silent no-op.
func TestResolveWatchRuleSourceScope_EmptyWildcardIsVisibleNotStalled(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	})
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(snWildcard), target, enumerating())

	require.NoError(t, err)
	assert.True(t, resolved.Admitted(), "an empty admitted set is not a refusal")
	assert.False(t, resolved.Terminal())
	assert.Equal(t, authz.ReasonNoAdmittedSourceNamespaces, resolved.Reason)
	assert.Empty(t, resolved.NamespacesFor(0))
}

// TestResolveWatchRuleSourceScope_WildcardOverNamesNeedsNoResolver is the degradation path applied
// to the wildcard: a names-only policy is enumerable with no source-cluster access at all, so "*"
// keeps resolving on a cluster whose Namespace list is Forbidden.
func TestResolveWatchRuleSourceScope_WildcardOverNamesNeedsNoResolver(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS, "team-payments"}})
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	resolver := denying()
	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(snWildcard), target, resolver)

	require.NoError(t, err)
	assert.True(t, resolved.Admitted())
	assert.Equal(t, []string{snSourceNS, "team-payments"}, resolved.NamespacesFor(0))
	assert.Zero(t, resolver.enumCalls,
		"a names-only policy must never reach the source-scope service")
}

// TestResolveWatchRuleSourceScope_StoredTopLevelFieldIsRefused covers decision 10's stored-object
// half: admission rejects spec.sourceNamespace, but a pre-release object keeps its value in etcd and
// resolving the items as if it had not asked would silently watch the wrong namespace.
func TestResolveWatchRuleSourceScope_StoredTopLevelFieldIsRefused(t *testing.T) {
	target := snTarget(nil)
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true)).Build()

	rule := snRule("")
	rule.Spec.SourceNamespace = snSourceNS //nolint:staticcheck // the point of the test

	resolved, err := authz.ResolveWatchRuleSourceScope(context.Background(), cl, rule, target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict)
	assert.Equal(t, authz.ReasonSourceNamespaceFieldRemoved, resolved.Reason)
	assert.Contains(t, resolved.Message, "spec.rules[].sourceNamespace",
		"the refusal must name the replacement, because the move is not automatic")
	assert.Empty(t, resolved.Items, "a refused rule resolves no items")
}

// TestResolveWatchRuleSourceScope_TargetIsolation is the multi-tenant invariant: a GitTarget's
// policy bounds ONLY that target. zen's policy admitting acme's namespace must not let a rule
// writing to ACME's target reach it.
func TestResolveWatchRuleSourceScope_TargetIsolation(t *testing.T) {
	// acme's target admits only its own workspace; a sibling tenant's target admits "shared".
	acme := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{"acme-workspace"}})

	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(acme, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule("shared"), acme, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict,
		"another target's policy must never widen this one")
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
}

// TestResolveWatchRuleSourceScope_UnadmittedGitTargetCannotDelegate closes the first leg of the
// three-part gate: a provider that does not admit the GitTarget's own namespace delegates nothing to
// it, even with the flag on.
func TestResolveWatchRuleSourceScope_UnadmittedGitTargetCannotDelegate(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})
	provider := snClusterProvider(true)
	provider.Spec.AllowedNamespaces = &configv1alpha3.NamespaceMatcher{Names: []string{"some-other-tenant"}}

	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(target, provider,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict)
	assert.Contains(t, resolved.Message, "may not mirror through ClusterProvider")
}

// TestResolveWatchRuleSourceScope_MissingClusterProviderDeniesOverride: an absent provider delegates
// nothing. It must not be an implicit allow.
func TestResolveWatchRuleSourceScope_MissingClusterProviderDeniesOverride(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})

	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).WithObjects(target).Build()

	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, resolved.Verdict)
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)
}

// TestResolveWatchRuleSourceScope_ProviderReadErrorIsRequeued: a transient apiserver failure must
// surface as an ERROR the caller requeues on, never as a silent denial that would tear down a
// running stream.
func TestResolveWatchRuleSourceScope_ProviderReadErrorIsRequeued(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})
	boom := errors.New("etcdserver: request timed out")

	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*configv1alpha3.ClusterProvider); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	_, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.Error(t, err, "a non-NotFound read error must requeue, not deny")
	assert.ErrorIs(t, err, boom)
}

// TestResolveWatchRuleSourceScope_ExactNamesNeedNoResolver is the DEGRADATION PATH, and the half
// most likely to regress unnoticed: with no source-scope service wired at all (standing in for a
// source cluster whose Namespace access is denied), an exact-NAME entry still admits while a
// SELECTOR entry fails safe as "cannot say yet" rather than as a denial.
func TestResolveWatchRuleSourceScope_ExactNamesNeedNoResolver(t *testing.T) {
	t.Run("exact name still admits", func(t *testing.T) {
		resolved := resolveOne(t, snSourceNS,
			&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}}, true, nil)

		assert.Equal(t, authz.SourceScopeAdmitted, resolved.Verdict,
			"a name-based policy must not depend on source-cluster Namespace access")
	})

	t.Run("selector without a resolver is retryable, never denied", func(t *testing.T) {
		resolved := resolveOne(t, snSourceNS, &configv1alpha3.NamespaceMatcher{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
		}, true, nil)

		assert.Equal(t, authz.SourceScopeUnknown, resolved.Verdict)
		assert.Equal(t, authz.ReasonCheckingSourceNamespacePolicy, resolved.Reason)
	})

	t.Run("wildcard over a selector without a resolver is retryable, never empty", func(t *testing.T) {
		resolved := resolveOne(t, snWildcard, &configv1alpha3.NamespaceMatcher{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
		}, true, nil)

		assert.Equal(t, authz.SourceScopeUnknown, resolved.Verdict)
		assert.Empty(t, resolved.NamespacesFor(0),
			"and it resolves NO namespaces, so nothing can be compiled from it")
	})
}

// TestResolveWatchRuleSourceScope_PatternInPolicyNamesCannotBeEvaluated covers the policy the
// schema now rejects but etcd may still hold.
//
// `names: ["*"]` reads like "every namespace" and is nothing of the sort: `*` is a literal name
// Kubernetes matches against nothing, so honouring it would resolve a wildcard item to a namespace
// that cannot exist — an authorized-looking rule mirroring zero objects. The verdict must therefore
// be UNAVAILABLE (an operator edit is required) rather than a smaller admitted scope, and it must
// condemn the whole policy: admitting the entries that happen to be well-formed is silent
// narrowing, which is the failure this design refuses everywhere else.
func TestResolveWatchRuleSourceScope_PatternInPolicyNamesCannotBeEvaluated(t *testing.T) {
	t.Run("a wildcard item cannot resolve through it", func(t *testing.T) {
		resolved := resolveOne(t, snWildcard,
			&configv1alpha3.NamespaceMatcher{Names: []string{"*"}}, true, admitting())

		assert.Equal(t, authz.SourceScopeUnavailable, resolved.Verdict)
		assert.Equal(t, authz.ReasonSourceNamespacePolicyUnavailable, resolved.Reason)
		assert.Empty(t, resolved.NamespacesFor(0),
			"no scope may be resolved from a policy that cannot be evaluated")
		assert.Contains(t, resolved.Message, "selector: {}",
			"the message must name the form that actually admits every namespace")
	})

	t.Run("a valid name alongside it does not rescue the policy", func(t *testing.T) {
		resolved := resolveOne(t, snSourceNS,
			&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS, "*"}}, true, admitting())

		assert.Equal(t, authz.SourceScopeUnavailable, resolved.Verdict,
			"a partially valid policy is not a smaller policy")
	})

	t.Run("a legacy own-namespace rule against no policy is untouched", func(t *testing.T) {
		resolved := resolveOne(t, "", nil, false, nil)

		assert.Equal(t, authz.SourceScopeAdmitted, resolved.Verdict,
			"validation applies to a DECLARED policy; it must not disturb the legacy path")
		assert.Equal(t, authz.ReasonLegacySourceNamespace, resolved.Reason)
	})
}

// TestResolveWatchRuleSourceScope_NameFastPathSkipsTheResolver pins the degradation path's
// mechanism, not just its outcome: a name match must be answered WITHOUT consulting the
// source-scope service at all, or "exact names keep working without Namespace access" is only
// accidentally true.
func TestResolveWatchRuleSourceScope_NameFastPathSkipsTheResolver(t *testing.T) {
	resolver := denying()
	resolved := resolveOne(t, snSourceNS, &configv1alpha3.NamespaceMatcher{
		Names:    []string{snSourceNS},
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"never": "consulted"}},
	}, true, resolver)

	assert.Equal(t, authz.SourceScopeAdmitted, resolved.Verdict)
	assert.Zero(t, resolver.calls, "an exact-name match must never reach the source-scope service")
}

// TestResolvedSourceScope_Fingerprint pins the §4.3 hazard at its source: the fingerprint must move
// when the RESOLVED set changes, even though the rule spec that produced it is byte-identical.
func TestResolvedSourceScope_Fingerprint(t *testing.T) {
	narrow := authz.ResolvedSourceScope{Items: []authz.SourceNamespaceDecision{
		{Index: 0, Namespaces: []string{"a"}},
	}}
	wide := authz.ResolvedSourceScope{Items: []authz.SourceNamespaceDecision{
		{Index: 0, Namespaces: []string{"a", "b"}},
	}}

	assert.NotEqual(t, narrow.Fingerprint(), wide.Fingerprint(),
		"a policy edit that widens a wildcard MUST change the fingerprint, or the watched-type "+
			"table is never re-projected and the streams silently keep their old width")
	assert.Equal(t, narrow.Fingerprint(), authz.ResolvedSourceScope{
		Items: []authz.SourceNamespaceDecision{{Index: 0, Namespaces: []string{"a"}}},
	}.Fingerprint(), "and an unchanged set must be stable, or every reconcile rebuilds the table")
}

// TestSourceNamespaceDecision_TerminalClassification pins which verdicts stop a rule while
// ESTABLISHING a grant. Denied and Unavailable are terminal; Unknown must never be, or a transient
// outage becomes a permanent Stalled=True.
func TestSourceNamespaceDecision_TerminalClassification(t *testing.T) {
	for verdict, wantTerminal := range map[authz.SourceScopeVerdict]bool{
		authz.SourceScopeAdmitted:    false,
		authz.SourceScopeUnknown:     false,
		authz.SourceScopeDenied:      true,
		authz.SourceScopeUnavailable: true,
	} {
		decision := authz.SourceNamespaceDecision{Verdict: verdict}
		assert.Equal(t, wantTerminal, decision.Terminal(), "verdict %d", verdict)
		assert.Equal(t, wantTerminal, authz.ResolvedSourceScope{Verdict: verdict}.Terminal())
	}
}

// TestAggregateSourceScope_ReasonPrecedence pins the §5 order. Without a stated precedence two
// implementations disagree about mixed rules, and "worst wins" is ambiguous between a denial and an
// unevaluatable policy.
func TestAggregateSourceScope_ReasonPrecedence(t *testing.T) {
	selectorPolicy := &configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	}
	target := snTarget(selectorPolicy)
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	// One item the selector denies, one it cannot evaluate: DENIAL wins, because it is a decision
	// while "cannot say" is the absence of one.
	mixed := &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeDenied}}
	resolved, err := authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule("a-namespace", "b-namespace"), target, mixed)
	require.NoError(t, err)
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, resolved.Reason)

	// Unavailable outranks Unknown for the same reason, one level down.
	unavailable := &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeUnavailable}}
	resolved, err = authz.ResolveWatchRuleSourceScope(
		context.Background(), cl, snRule("a-namespace"), target, unavailable)
	require.NoError(t, err)
	assert.Equal(t, authz.ReasonSourceNamespacePolicyUnavailable, resolved.Reason)
}
