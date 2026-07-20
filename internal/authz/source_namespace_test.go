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

func snRule(sourceNamespace string) *configv1alpha3.WatchRule {
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: snRuleName, Namespace: snTenantNS},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef:       configv1alpha3.LocalTargetReference{Name: snTargetName},
			SourceNamespace: sourceNamespace,
			Rules:           []configv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
		},
	}
}

// snClusterProvider admits snTenantNS by name, so the "provider admits the target's namespace" leg
// of the gate passes and each case isolates the leg it is actually about.
func snClusterProvider(delegate bool) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: snProvider},
		Spec: configv1alpha3.ClusterProviderSpec{
			AllowedNamespaces:                     &configv1alpha3.NamespaceMatcher{Names: []string{snTenantNS}},
			AllowWatchRuleSourceNamespaceOverride: delegate,
		},
	}
}

// stubResolver is a source-scope service stand-in. The gate only ever asks it SELECTOR questions —
// authz answers the exact-name half itself — so a test that expects it to be consulted is also
// asserting that the name fast-path did not swallow the question.
type stubResolver struct {
	result authz.SourceScopeResult
	calls  int
}

func (s *stubResolver) ResolveSourceNamespace(
	context.Context, *configv1alpha3.GitTarget, string,
) authz.SourceScopeResult {
	s.calls++
	return s.result
}

func admitting() *stubResolver {
	return &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeAdmitted}}
}

func denying() *stubResolver {
	return &stubResolver{result: authz.SourceScopeResult{Verdict: authz.SourceScopeDenied}}
}

// TestWatchRuleSourceNamespaceAdmitted is the gate's truth table, modelled on
// TestCheckSourceAuthorization. The first two rows are the LEGACY guarantee: if they ever fail,
// deny-by-default has broken every existing WatchRule on upgrade.
func TestWatchRuleSourceNamespaceAdmitted(t *testing.T) {
	labelled := map[string]string{"gitops.configbutler.ai/mirrorable": "true"}
	selectorPolicy := &configv1alpha3.NamespaceMatcher{
		Selector: &metav1.LabelSelector{MatchLabels: labelled},
	}

	tests := []struct {
		name string
		// sourceNamespace is WatchRule.spec.sourceNamespace ("" = omitted).
		sourceNamespace string
		policy          *configv1alpha3.NamespaceMatcher
		delegate        bool
		resolver        *stubResolver
		wantVerdict     authz.SourceScopeVerdict
		wantReason      string
	}{{
		// THE test. A rule that omits sourceNamespace must pass with no policy and no flag.
		name:            "omitted, no policy, flag false: allowed (legacy, and must stay free)",
		sourceNamespace: "",
		policy:          nil,
		delegate:        false,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonLegacySourceNamespace,
	}, {
		name:            "explicitly equals its own namespace, no policy, flag false: allowed",
		sourceNamespace: snTenantNS,
		policy:          nil,
		delegate:        false,
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonLegacySourceNamespace,
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
	}, {
		name:            "differs, flag true, target selector matches: allowed",
		sourceNamespace: snSourceNS,
		policy:          selectorPolicy,
		delegate:        true,
		resolver:        admitting(),
		wantVerdict:     authz.SourceScopeAdmitted,
		wantReason:      authz.ReasonSourceNamespaceAllowed,
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
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := snTarget(tc.policy)
			cl := fake.NewClientBuilder().
				WithScheme(snScheme(t)).
				WithObjects(
					target,
					snClusterProvider(tc.delegate),
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}},
				).
				Build()

			var resolver authz.SourceNamespaceResolver
			if tc.resolver != nil {
				resolver = tc.resolver
			}

			decision, err := authz.WatchRuleSourceNamespaceAdmitted(
				context.Background(), cl, snRule(tc.sourceNamespace), target, resolver)

			require.NoError(t, err)
			assert.Equal(t, tc.wantVerdict, decision.Verdict, "verdict (message: %s)", decision.Message)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.NotEmpty(t, decision.Message, "every verdict must carry an operator-legible message")

			wantNS := tc.sourceNamespace
			if wantNS == "" {
				wantNS = snTenantNS
			}
			assert.Equal(t, wantNS, decision.Namespace)
		})
	}
}

// TestWatchRuleSourceNamespaceAdmitted_TargetIsolation is the multi-tenant invariant: a GitTarget's
// policy bounds ONLY that target. zen's policy admitting acme's namespace must not let a rule
// writing to ACME's target reach it.
func TestWatchRuleSourceNamespaceAdmitted_TargetIsolation(t *testing.T) {
	// acme's target admits only its own workspace; a sibling tenant's target admits "shared".
	acme := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{"acme-workspace"}})

	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(acme, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		Build()

	decision, err := authz.WatchRuleSourceNamespaceAdmitted(
		context.Background(), cl, snRule("shared"), acme, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, decision.Verdict,
		"another target's policy must never widen this one")
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, decision.Reason)
}

// TestWatchRuleSourceNamespaceAdmitted_UnadmittedGitTargetCannotDelegate closes the first leg of
// the three-part gate: a provider that does not admit the GitTarget's own namespace delegates
// nothing to it, even with the flag on.
func TestWatchRuleSourceNamespaceAdmitted_UnadmittedGitTargetCannotDelegate(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})
	provider := snClusterProvider(true)
	provider.Spec.AllowedNamespaces = &configv1alpha3.NamespaceMatcher{Names: []string{"some-other-tenant"}}

	cl := fake.NewClientBuilder().
		WithScheme(snScheme(t)).
		WithObjects(target, provider,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).
		Build()

	decision, err := authz.WatchRuleSourceNamespaceAdmitted(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, decision.Verdict)
	assert.Contains(t, decision.Message, "may not mirror through ClusterProvider")
}

// TestWatchRuleSourceNamespaceAdmitted_MissingClusterProviderDeniesOverride: an absent provider
// delegates nothing. It must not be an implicit allow.
func TestWatchRuleSourceNamespaceAdmitted_MissingClusterProviderDeniesOverride(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})

	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).WithObjects(target).Build()

	decision, err := authz.WatchRuleSourceNamespaceAdmitted(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeDenied, decision.Verdict)
	assert.Equal(t, authz.ReasonSourceNamespaceNotAllowed, decision.Reason)
}

// TestWatchRuleSourceNamespaceAdmitted_ProviderReadErrorIsRequeued: a transient apiserver failure
// must surface as an ERROR the caller requeues on, never as a silent denial that would tear down a
// running stream.
func TestWatchRuleSourceNamespaceAdmitted_ProviderReadErrorIsRequeued(t *testing.T) {
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

	_, err := authz.WatchRuleSourceNamespaceAdmitted(
		context.Background(), cl, snRule(snSourceNS), target, nil)

	require.Error(t, err, "a non-NotFound read error must requeue, not deny")
	assert.ErrorIs(t, err, boom)
}

// TestWatchRuleSourceNamespaceAdmitted_ExactNamesNeedNoResolver is the DEGRADATION PATH, and the
// half most likely to regress unnoticed: with no source-scope service wired at all (standing in
// for a source cluster whose Namespace access is denied), an exact-NAME entry still admits while a
// SELECTOR entry fails safe as "cannot say yet" rather than as a denial.
func TestWatchRuleSourceNamespaceAdmitted_ExactNamesNeedNoResolver(t *testing.T) {
	ctx := context.Background()

	t.Run("exact name still admits", func(t *testing.T) {
		target := snTarget(&configv1alpha3.NamespaceMatcher{Names: []string{snSourceNS}})
		cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
			WithObjects(target, snClusterProvider(true),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

		decision, err := authz.WatchRuleSourceNamespaceAdmitted(ctx, cl, snRule(snSourceNS), target, nil)

		require.NoError(t, err)
		assert.Equal(t, authz.SourceScopeAdmitted, decision.Verdict,
			"a name-based policy must not depend on source-cluster Namespace access")
	})

	t.Run("selector without a resolver is retryable, never denied", func(t *testing.T) {
		target := snTarget(&configv1alpha3.NamespaceMatcher{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
		})
		cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
			WithObjects(target, snClusterProvider(true),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

		decision, err := authz.WatchRuleSourceNamespaceAdmitted(ctx, cl, snRule(snSourceNS), target, nil)

		require.NoError(t, err)
		assert.Equal(t, authz.SourceScopeUnknown, decision.Verdict)
		assert.Equal(t, authz.ReasonCheckingSourceNamespacePolicy, decision.Reason)
	})
}

// TestWatchRuleSourceNamespaceAdmitted_NameFastPathSkipsTheResolver pins the degradation path's
// mechanism, not just its outcome: a name match must be answered WITHOUT consulting the
// source-scope service at all, or "exact names keep working without Namespace access" is only
// accidentally true.
func TestWatchRuleSourceNamespaceAdmitted_NameFastPathSkipsTheResolver(t *testing.T) {
	target := snTarget(&configv1alpha3.NamespaceMatcher{
		Names:    []string{snSourceNS},
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"never": "consulted"}},
	})
	cl := fake.NewClientBuilder().WithScheme(snScheme(t)).
		WithObjects(target, snClusterProvider(true),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: snTenantNS}}).Build()

	resolver := denying()
	decision, err := authz.WatchRuleSourceNamespaceAdmitted(
		context.Background(), cl, snRule(snSourceNS), target, resolver)

	require.NoError(t, err)
	assert.Equal(t, authz.SourceScopeAdmitted, decision.Verdict)
	assert.Zero(t, resolver.calls, "an exact-name match must never reach the source-scope service")
}

// TestWatchRuleSourceNamespaceDecision_TerminalClassification pins which verdicts stop a rule while
// ESTABLISHING a grant. Denied and Unavailable are terminal; Unknown must never be, or a transient
// outage becomes a permanent Stalled=True.
func TestWatchRuleSourceNamespaceDecision_TerminalClassification(t *testing.T) {
	for verdict, wantTerminal := range map[authz.SourceScopeVerdict]bool{
		authz.SourceScopeAdmitted:    false,
		authz.SourceScopeUnknown:     false,
		authz.SourceScopeDenied:      true,
		authz.SourceScopeUnavailable: true,
	} {
		decision := authz.SourceNamespaceDecision{Verdict: verdict}
		assert.Equal(t, wantTerminal, decision.Terminal(), "verdict %d", verdict)
	}
}
