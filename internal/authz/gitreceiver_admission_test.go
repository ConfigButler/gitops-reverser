// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// gitProviderIn is a GitProvider in testNS whose spec.gitReceiverRef names receiverName in
// refNamespace ("" names no receiver at all).
func gitProviderIn(receiverName, refNamespace string) *configv1alpha3.GitProvider {
	p := &configv1alpha3.GitProvider{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: testNS}}
	if receiverName != "" {
		p.Spec.GitReceiverRef = &meta.NamespacedObjectReference{Name: receiverName, Namespace: refNamespace}
	}
	return p
}

// gitReceiverIn is the GitReceiver "hook" in namespace, admitting other namespaces through policy.
func gitReceiverIn(namespace string, policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.GitReceiver {
	return &configv1alpha3.GitReceiver{
		ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: namespace},
		Spec:       configv1alpha3.GitReceiverSpec{AccessFrom: policy},
	}
}

func TestGitReceiverAdmitted(t *testing.T) {
	labeled := namespaceLabeled(map[string]string{"tier": "trusted"})
	tests := []struct {
		name         string
		provider     *configv1alpha3.GitProvider
		objects      []client.Object
		wantAllowed  bool
		wantReason   string
		wantReceiver bool
	}{
		{
			name:     "no reference admits nothing and names no reason",
			provider: gitProviderIn("", ""),
		},
		{
			name:       "a missing receiver",
			provider:   gitProviderIn("hook", platformNS),
			wantReason: ReasonGitReceiverNotFound,
		},
		{
			name:         "the receiver's own namespace is always admitted",
			provider:     gitProviderIn("hook", ""),
			objects:      []client.Object{gitReceiverIn(testNS, nil)},
			wantAllowed:  true,
			wantReceiver: true,
		},
		{
			name:         "another namespace without accessFrom is refused",
			provider:     gitProviderIn("hook", platformNS),
			objects:      []client.Object{gitReceiverIn(platformNS, nil), labeled},
			wantReason:   ReasonNamespaceNotAuthorized,
			wantReceiver: true,
		},
		{
			name:     "another namespace accessFrom names",
			provider: gitProviderIn("hook", platformNS),
			objects: []client.Object{
				gitReceiverIn(platformNS, &configv1alpha3.NamespaceMatcher{Names: []string{testNS}}),
			},
			wantAllowed:  true,
			wantReceiver: true,
		},
		{
			name:     "another namespace the selector matches",
			provider: gitProviderIn("hook", platformNS),
			objects: []client.Object{gitReceiverIn(platformNS, &configv1alpha3.NamespaceMatcher{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}},
			}), labeled},
			wantAllowed:  true,
			wantReceiver: true,
		},
		{
			name:     "a selector that does not convert is a refusal, never an allow",
			provider: gitProviderIn("hook", platformNS),
			objects: []client.Object{gitReceiverIn(platformNS, &configv1alpha3.NamespaceMatcher{
				Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "tier", Operator: "Bogus",
				}}},
			}), labeled},
			wantReason:   ReasonNamespaceNotAuthorized,
			wantReceiver: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tc.objects...).Build()
			decision, receiver, err := GitReceiverAdmitted(context.Background(), reader, tc.provider)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAllowed, decision.Allowed)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Equal(t, tc.wantReceiver, receiver != nil)
			if tc.wantReason != "" {
				assert.NotEmpty(t, decision.Message)
			}
		})
	}
}

// TestGitReceiverAdmitted_ReadErrors returns a failed read as an error, so the caller retries rather
// than reporting a refusal it never decided.
func TestGitReceiverAdmitted_ReadErrors(t *testing.T) {
	for name, failOn := range map[string]client.Object{
		"receiver":  &configv1alpha3.GitReceiver{},
		"namespace": &corev1.Namespace{},
	} {
		t.Run(name, func(t *testing.T) {
			reader := fake.NewClientBuilder().
				WithScheme(admissionScheme(t)).
				WithObjects(gitReceiverIn(platformNS, &configv1alpha3.NamespaceMatcher{Names: []string{"x"}})).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
						opts ...client.GetOption) error {
						if sameKind(obj, failOn) {
							return errors.New("boom")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			_, _, err := GitReceiverAdmitted(context.Background(), reader, gitProviderIn("hook", platformNS))
			require.Error(t, err)
		})
	}
}

func sameKind(a, b client.Object) bool {
	switch a.(type) {
	case *configv1alpha3.GitReceiver:
		_, ok := b.(*configv1alpha3.GitReceiver)
		return ok
	case *corev1.Namespace:
		_, ok := b.(*corev1.Namespace)
		return ok
	default:
		return false
	}
}
