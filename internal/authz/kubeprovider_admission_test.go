// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const (
	testNS       = "team-a"
	platformNS   = "platform"
	platformPrev = "platform-old"
)

func admissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// targetIn builds a GitTarget in testNS referencing the KubeProvider providerName in platformNS
// ("" sets no kubeProviderRef at all).
func targetIn(providerName string) *configv1alpha3.GitTarget {
	t := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: testNS},
	}
	if providerName != "" {
		t.Spec.KubeProviderRef = meta.NamespacedObjectReference{Name: providerName, Namespace: platformNS}
	}
	return t
}

// providerNamed builds a KubeProvider in platformNS, so the GitTarget in testNS is admitted only
// through policy.
func providerNamed(name string, policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.KubeProvider {
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNS},
		Spec:       configv1alpha3.KubeProviderSpec{AccessFrom: policy},
	}
}

func namespaceLabeled(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: labels}}
}

func TestGitTargetAdmitted_Policy(t *testing.T) {
	tests := []struct {
		name        string
		objects     []client.Object
		target      *configv1alpha3.GitTarget
		wantAllowed bool
		wantReason  string
		wantMessage string
	}{
		{
			name:        "provider missing is a hard denial, not an implicit allow",
			objects:     []client.Object{namespaceLabeled(nil)},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "referenced KubeProvider platform/prod-eu-1 was not found",
		},
		{
			// No reference names nothing; there is no provider an omitted reference falls back to.
			name:        "an unset kubeProviderRef is denied, never defaulted",
			objects:     []client.Object{namespaceLabeled(nil)},
			target:      targetIn(""),
			wantAllowed: false,
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "spec.kubeProviderRef names no KubeProvider",
		},
		{
			// A provider admits its own namespace without any accessFrom, like a GitProvider.
			name: "a provider in the target's own namespace is admitted without accessFrom",
			objects: []client.Object{
				&configv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: testNS}},
				namespaceLabeled(nil),
			},
			target: &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: testNS},
				Spec: configv1alpha3.GitTargetSpec{
					KubeProviderRef: meta.NamespacedObjectReference{Name: "local"},
				},
			},
			wantAllowed: true,
		},
		{
			name: "nil accessFrom denies another namespace by default",
			objects: []client.Object{
				providerNamed("prod-eu-1", nil),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: `namespace "team-a" is not permitted to reference KubeProvider platform/prod-eu-1`,
		},
		{
			name: "empty policy struct denies by default",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{}),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
		},
		{
			name: "names entry admits",
			objects: []client.Object{
				providerNamed("prod-eu-1",
					&configv1alpha3.NamespaceMatcher{Names: []string{"team-b", "team-a"}}),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name: "names list not containing the namespace denies",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{Names: []string{"team-b"}}),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
		},
		{
			// The chart default. An empty selector matches every label set, so it admits every
			// namespace — the single-cluster install's "no policy configured" shape.
			name: "empty selector admits every namespace",
			objects: []client.Object{
				providerNamed("prod-eu-1",
					&configv1alpha3.NamespaceMatcher{Selector: &metav1.LabelSelector{}}),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name: "selector matching namespace labels admits",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
				namespaceLabeled(map[string]string{"tier": "prod"}),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name: "selector not matching namespace labels denies",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
				namespaceLabeled(map[string]string{"tier": "dev"}),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
		},
		{
			// Names and selector are ORed: a listed namespace is admitted even when the selector
			// rejects it. Guards against a future refactor turning the OR into an AND.
			name: "names admits even when the selector does not match",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{
					Names:    []string{"team-a"},
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
				namespaceLabeled(map[string]string{"tier": "dev"}),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name: "invalid selector denies with a legible message rather than admitting",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: "tier", Operator: "NotARealOperator",
					}}},
				}),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "accessFrom selector is invalid",
		},
		{
			// A missing Namespace object is not an error: `names` is evaluated against the NAME,
			// which exists in the reference even before the namespace object does.
			name: "absent namespace object still admitted by a names entry",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			// ...but a selector has no labels to match against, so it correctly does not admit.
			name: "absent namespace object is not admitted by a label selector",
			objects: []client.Object{
				providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				}),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tc.objects...).Build()

			decision, err := GitTargetAdmitted(context.Background(), cl, tc.target)

			require.NoError(t, err)
			assert.Equal(t, tc.wantAllowed, decision.Allowed)
			if tc.wantAllowed {
				assert.Empty(t, decision.Reason, "an admission carries no reason")
				assert.Empty(t, decision.Message, "an admission carries no message")
				return
			}
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.NotEmpty(t, decision.Message, "a denial must always explain itself")
			if tc.wantMessage != "" {
				assert.Contains(t, decision.Message, tc.wantMessage)
			}
		})
	}
}

// TestGitTargetAdmitted_ReadErrorsSurfaceAsError pins the difference that decides whether a
// transient apiserver failure tears down a running data plane: a read error must come back as err
// (caller requeues, keeps the stream) and never as a denial (caller stops the stream).
func TestGitTargetAdmitted_ReadErrorsSurfaceAsError(t *testing.T) {
	boom := errors.New("apiserver unavailable")

	tests := []struct {
		name    string
		failOn  func(key client.ObjectKey, obj client.Object) bool
		wantMsg string
	}{
		{
			name: "KubeProvider read failure",
			failOn: func(_ client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*configv1alpha3.KubeProvider)
				return ok
			},
			wantMsg: "read KubeProvider platform/prod-eu-1",
		},
		{
			name: "Namespace read failure",
			failOn: func(_ client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*corev1.Namespace)
				return ok
			},
			wantMsg: `read namespace "team-a"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
				WithObjects(
					providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}),
					namespaceLabeled(nil),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						ctx context.Context, c client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption,
					) error {
						if tc.failOn(key, obj) {
							return boom
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			decision, err := GitTargetAdmitted(context.Background(), cl, targetIn("prod-eu-1"))

			require.Error(t, err)
			require.ErrorIs(t, err, boom)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.False(t, decision.Allowed, "an errored decision must not read as admitted")
			assert.Empty(t, decision.Reason,
				"an errored decision must carry no denial reason: callers must requeue, not refuse")
		})
	}
}

// TestApplierAdmitted: an applier is reached with its trigger's KubeProvider's credential, so
// that provider must admit the namespace too, and the source's denial comes first.
func TestApplierAdmitted(t *testing.T) {
	admits := func(name string, namespaces ...string) *configv1alpha3.KubeProvider {
		return providerNamed(name, &configv1alpha3.NamespaceMatcher{Names: namespaces})
	}
	triggerOn := func(cluster string) configv1alpha3.ApplyTrigger {
		return &configv1alpha3.FluxTrigger{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "apps"},
			Spec: configv1alpha3.FluxTriggerSpec{
				KubeProviderRef: meta.NamespacedObjectReference{Name: cluster, Namespace: platformNS},
			},
		}
	}
	tests := []struct {
		name        string
		objects     []client.Object
		trigger     configv1alpha3.ApplyTrigger
		wantAllowed bool
		wantReason  string
		wantMessage string
	}{
		{
			name:        "the trigger's provider is the source's",
			objects:     []client.Object{admits("prod-eu-1", testNS)},
			trigger:     triggerOn("prod-eu-1"),
			wantAllowed: true,
		},
		{
			name:        "another provider that admits the namespace",
			objects:     []client.Object{admits("prod-eu-1", testNS), admits("hub", testNS)},
			trigger:     triggerOn("hub"),
			wantAllowed: true,
		},
		{
			name:        "another provider that does not admit the namespace",
			objects:     []client.Object{admits("prod-eu-1", testNS), admits("hub", "elsewhere")},
			trigger:     triggerOn("hub"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "KubeProvider platform/hub",
		},
		{
			name:        "another provider that does not exist",
			objects:     []client.Object{admits("prod-eu-1", testNS)},
			trigger:     triggerOn("hub"),
			wantReason:  ReasonKubeProviderNotFound,
			wantMessage: "FluxTrigger " + testNS + "/apps spec.kubeProviderRef",
		},
		{
			name:        "the source's denial comes first",
			objects:     []client.Object{admits("prod-eu-1", "elsewhere"), admits("hub", testNS)},
			trigger:     triggerOn("hub"),
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: "KubeProvider platform/prod-eu-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tc.objects...).Build()
			decision, err := ApplierAdmitted(context.Background(), reader, targetIn("prod-eu-1"), tc.trigger)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAllowed, decision.Allowed, decision.Message)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Contains(t, decision.Message, tc.wantMessage)
		})
	}
}

// TestAuditRouteHeld pins that a provider whose route another namespace holds is not used by any
// consumer, and that the holder and its own namespace's siblings are.
func TestAuditRouteHeld(t *testing.T) {
	older := metav1.NewTime(metav1.Now().Add(-time.Hour))
	withRoute := func(namespace, name, route string, created metav1.Time) *configv1alpha3.KubeProvider {
		p := &configv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: created},
			Spec: configv1alpha3.KubeProviderSpec{
				AccessFrom: &configv1alpha3.NamespaceMatcher{Names: []string{testNS}},
			},
		}
		if route != "" {
			p.Spec.Attribution = &configv1alpha3.KubeProviderAttribution{AuditRoute: route}
		}
		return p
	}
	holder := withRoute(platformPrev, "prod", "shared", older)
	sibling := withRoute(platformPrev, "prod-2", "shared", metav1.Now())
	borrower := withRoute(platformNS, "prod-eu-1", "shared", metav1.Now())

	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
		WithObjects(holder, sibling, borrower, namespaceLabeled(nil)).Build()

	for _, p := range []*configv1alpha3.KubeProvider{holder, sibling} {
		decision, err := AuditRouteHeld(context.Background(), reader, p)
		require.NoError(t, err)
		assert.True(t, decision.Allowed, "%s shares a route its own namespace holds", p.Key())
	}

	decision, err := AuditRouteHeld(context.Background(), reader, borrower)
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ReasonAuditRouteConflict, decision.Reason)
	assert.Contains(t, decision.Message, `namespace "platform-old"`)
	assert.Contains(t, decision.Message, `"platform.prod-eu-1"`, "the message names the default to fall back to")

	// The consumer of the borrowing provider is refused with the same reason, before any watch.
	admitted, err := GitTargetAdmitted(context.Background(), reader, targetIn("prod-eu-1"))
	require.NoError(t, err)
	assert.False(t, admitted.Allowed)
	assert.Equal(t, ReasonAuditRouteConflict, admitted.Reason)
}

// TestAuditRouteHeld_ListErrorSurfacesAsError: a failed list must requeue, never refuse.
func TestAuditRouteHeld_ListErrorSurfacesAsError(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()
	decision, err := AuditRouteHeld(context.Background(), reader, providerNamed("prod-eu-1", nil))
	require.ErrorIs(t, err, boom)
	assert.False(t, decision.Allowed)
	assert.Empty(t, decision.Reason)
}
