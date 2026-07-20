// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"testing"

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

const testNS = "team-a"

func admissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// targetIn builds a GitTarget in testNS referencing providerName ("" omits
// clusterProviderRef entirely, which resolves to the reserved "default" provider).
func targetIn(providerName string) *configv1alpha3.GitTarget {
	t := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: testNS},
	}
	if providerName != "" {
		t.Spec.ClusterProviderRef = &configv1alpha3.ClusterProviderReference{Name: providerName}
	}
	return t
}

func providerNamed(name string, policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       configv1alpha3.ClusterProviderSpec{AllowedNamespaces: policy},
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
			wantReason:  ReasonClusterProviderNotFound,
			wantMessage: `referenced ClusterProvider "prod-eu-1" was not found`,
		},
		{
			// A GitTarget with no clusterProviderRef resolves to "default"; that name is not a
			// bypass — an undeclared "default" is denied exactly like any other missing provider.
			name:        "omitted clusterProviderRef resolves to default and is still gated",
			objects:     []client.Object{namespaceLabeled(nil)},
			target:      targetIn(""),
			wantAllowed: false,
			wantReason:  ReasonClusterProviderNotFound,
			wantMessage: `referenced ClusterProvider "default" was not found`,
		},
		{
			name: "omitted clusterProviderRef is admitted by an admitting default provider",
			objects: []client.Object{
				providerNamed(configv1alpha3.DefaultClusterProviderName,
					&configv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}),
				namespaceLabeled(nil),
			},
			target:      targetIn(""),
			wantAllowed: true,
		},
		{
			name: "nil allowedNamespaces denies by default",
			objects: []client.Object{
				providerNamed("prod-eu-1", nil),
				namespaceLabeled(nil),
			},
			target:      targetIn("prod-eu-1"),
			wantAllowed: false,
			wantReason:  ReasonNamespaceNotAuthorized,
			wantMessage: `namespace "team-a" is not permitted to reference ClusterProvider "prod-eu-1"`,
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
			wantMessage: "allowedNamespaces selector is invalid",
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
			name: "ClusterProvider read failure",
			failOn: func(_ client.ObjectKey, obj client.Object) bool {
				_, ok := obj.(*configv1alpha3.ClusterProvider)
				return ok
			},
			wantMsg: `read ClusterProvider "prod-eu-1"`,
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
