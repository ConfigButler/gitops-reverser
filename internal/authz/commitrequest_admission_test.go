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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A GitTarget admits a CommitRequest by the namespace it is in and that namespace's labels, and gives it
// the most any admitting grant allows of a window whose writes name no author.
func TestCommitRequestAdmitted(t *testing.T) {
	tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: testNS, Labels: map[string]string{"platform.example.com/tenant": "true"},
	}}
	request := &configv1alpha3.CommitRequest{ObjectMeta: metav1.ObjectMeta{Name: "save", Namespace: testNS}}
	target := func(grants ...configv1alpha3.GitTargetCommitRequestGrant) *configv1alpha3.GitTarget {
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: platformNS},
			Spec:       configv1alpha3.GitTargetSpec{AllowedCommitRequests: grants},
		}
	}
	bySelector := func(policy configv1alpha3.UnattributedWindowPolicy) configv1alpha3.GitTargetCommitRequestGrant {
		return configv1alpha3.GitTargetCommitRequestGrant{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
				From:     configv1alpha3.GrantFromSelector,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"platform.example.com/tenant": "true"}},
			},
			UnattributedWindows: policy,
		}
	}
	all := func(policy configv1alpha3.UnattributedWindowPolicy) configv1alpha3.GitTargetCommitRequestGrant {
		return configv1alpha3.GitTargetCommitRequestGrant{
			ConsumerNamespaces:  configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromAll},
			UnattributedWindows: policy,
		}
	}
	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tenant).Build()

	decision, _, err := CommitRequestAdmitted(context.Background(), reader, request, target())
	require.NoError(t, err)
	assert.False(t, decision.Allowed, "the default admits only the GitTarget's own namespace")
	assert.Equal(t, ReasonNamespaceNotAuthorized, decision.Reason)
	assert.Contains(t, decision.Message, "spec.allowedCommitRequests")

	decision, admission, err := CommitRequestAdmitted(context.Background(), reader, request,
		target(bySelector(configv1alpha3.UnattributedWindowsSavedBy)))
	require.NoError(t, err)
	assert.True(t, decision.Allowed, "the namespace's labels match")
	assert.Equal(t, configv1alpha3.UnattributedWindowsSavedBy, admission.UnattributedWindows)

	_, admission, err = CommitRequestAdmitted(context.Background(), reader, request,
		target(all(configv1alpha3.UnattributedWindowsKeep), bySelector(configv1alpha3.UnattributedWindowsTakeOver)))
	require.NoError(t, err)
	assert.Equal(t, configv1alpha3.UnattributedWindowsTakeOver, admission.UnattributedWindows,
		"the most any admitting grant allows")

	failing := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	_, _, err = CommitRequestAdmitted(context.Background(), failing, request, target(all("")))
	require.Error(t, err, "a namespace that cannot be read is retried, never refused")
}
