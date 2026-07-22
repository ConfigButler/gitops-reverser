// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

const cpOperatorNS = "gitops-reverser-system"

func clusterProviderWithKubeConfig(name, secretName, key string) *configbutleraiv1alpha3.ClusterProvider {
	p := &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if secretName != "" {
		p.Spec.KubeConfig = &meta.KubeConfigReference{
			SecretRef: &meta.SecretKeyReference{Name: secretName, Key: key},
		}
	}
	return p
}

func TestValidateProviderKubeConfig_AllScenarios(t *testing.T) {
	tests := []struct {
		name       string
		provider   *configbutleraiv1alpha3.ClusterProvider
		secretData map[string][]byte
		safety     kubeconfig.SafetyPolicy
		wantOK     bool
		wantReason string
	}{
		{
			name:     "in-cluster default (no kubeConfig) is valid",
			provider: clusterProviderWithKubeConfig("default", "", ""),
			wantOK:   true, wantReason: ReasonInCluster,
		},
		{
			name:     "missing Secret",
			provider: clusterProviderWithKubeConfig("prod-eu-1", "absent", ""),
			wantOK:   false, wantReason: kubeconfig.ReasonSecretNotFound,
		},
		{
			name:       "missing key",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", "value"),
			secretData: map[string][]byte{"elsewhere": []byte(scValidKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonKeyNotFound,
		},
		{
			name:       "unparseable",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", ""),
			secretData: map[string][]byte{"value": []byte("not a kubeconfig")},
			wantOK:     false, wantReason: kubeconfig.ReasonInvalid,
		},
		{
			name:       "exec rejected",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", ""),
			secretData: map[string][]byte{"value": []byte(scExecKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonExecNotAllowed,
		},
		{
			name:       "insecure TLS rejected",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", ""),
			secretData: map[string][]byte{"value": []byte(scInsecureKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonInsecureTLSNotAllowed,
		},
		{
			name:       "valid via value.yaml fallback",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", ""),
			secretData: map[string][]byte{"value.yaml": []byte(scValidKubeConfig)},
			wantOK:     true, wantReason: ReasonValidated,
		},
		{
			name:       "exec allowed when opted in",
			provider:   clusterProviderWithKubeConfig("prod-eu-1", "kc", ""),
			secretData: map[string][]byte{"value": []byte(scExecKubeConfig)},
			safety:     kubeconfig.SafetyPolicy{AllowExec: true},
			wantOK:     true, wantReason: ReasonValidated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.secretData != nil {
				builder = builder.WithObjects(&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Namespace: cpOperatorNS, Name: "kc"},
					Data:       tc.secretData,
				})
			}
			r := &ClusterProviderReconciler{
				Client:            builder.Build(),
				OperatorNamespace: cpOperatorNS,
				KubeConfigSafety:  tc.safety,
			}
			ok, reason, msg, err := r.validateProviderKubeConfig(context.Background(), tc.provider)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantReason, reason)
			assert.NotEmpty(t, msg)
		})
	}
}

// TestClusterProviderReconcile_InClusterDefault checks the reserved "default" provider validates
// and goes Ready with no kubeConfig Secret to read.
func TestClusterProviderReconcile_InClusterDefault(t *testing.T) {
	provider := clusterProviderWithKubeConfig("default", "", "")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "default"}})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.ClusterProvider
	require.NoError(t, cl.Get(context.Background(), k8stypes.NamespacedName{Name: "default"}, &got))
	assert.Equal(
		t,
		metav1.ConditionTrue,
		findCondition(got.Status.Conditions, ClusterProviderConditionValidated).Status,
	)
	assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}

// TestClusterProviderReconcile_TakesNoFinalizer pins the contract: this controller never makes a
// ClusterProvider undeletable. Nothing has to happen before one goes away — attribution facts are
// keyed by (audit route, group/resource, uid, resourceVersion) and expire on their own, so a
// re-provisioned cluster reusing a provider name mints different object UIDs and cannot join a
// stale fact. The reason a finalizer must never come back is on LegacyClusterProviderFinalizer:
// helm uninstall would strand the object in Terminating.
func TestClusterProviderReconcile_TakesNoFinalizer(t *testing.T) {
	provider := clusterProviderWithKubeConfig("default", "", "")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		WithObjects(provider).
		Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "default"}})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.ClusterProvider
	require.NoError(t, cl.Get(context.Background(), k8stypes.NamespacedName{Name: "default"}, &got))
	assert.Empty(t, got.Finalizers, "a ClusterProvider must never be held by this controller")
}

// TestClusterProviderReconcile_ShedsLegacyFinalizer is the upgrade path. An object created by an
// older operator carries the retired fact-purge finalizer; after this upgrade nothing would ever
// remove it, so it would strand in Terminating forever and block reinstalling. The controller has
// to actively shed it — including while the object is ALREADY deleting, which is the state a
// stranded object is in.
func TestClusterProviderReconcile_ShedsLegacyFinalizer(t *testing.T) {
	tests := []struct {
		name     string
		deleting bool
	}{
		{name: "live object carrying the retired finalizer", deleting: false},
		{name: "object already stranded in Terminating", deleting: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := clusterProviderWithKubeConfig("prod-eu-1", "kc", "")
			provider.Finalizers = []string{LegacyClusterProviderFinalizer}
			if tt.deleting {
				now := metav1.Now()
				provider.DeletionTimestamp = &now
			}
			cl := fake.NewClientBuilder().
				WithScheme(scScheme(t)).
				WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
				WithObjects(provider).
				Build()
			r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

			_, err := r.Reconcile(
				context.Background(),
				ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "prod-eu-1"}},
			)
			require.NoError(t, err)

			var got configbutleraiv1alpha3.ClusterProvider
			getErr := cl.Get(context.Background(), k8stypes.NamespacedName{Name: "prod-eu-1"}, &got)
			if tt.deleting {
				// Shedding the last finalizer on a deleting object lets it go immediately.
				assert.True(t, apierrors.IsNotFound(getErr),
					"a stranded object must be released, not left in Terminating")
				return
			}
			require.NoError(t, getErr)
			assert.Empty(t, got.Finalizers, "the retired finalizer must be shed")
		})
	}
}

// TestClusterProviderReconcile_ShedFinalizerUpdateFails pins the retry contract for the upgrade
// path. A stranded object gets no further events of its own, so if the finalizer-shedding Update
// fails the reconcile MUST return the error — that is the only thing that re-queues it. Swallowing
// the failure would leave the object in Terminating until the operator restarts.
func TestClusterProviderReconcile_ShedFinalizerUpdateFails(t *testing.T) {
	provider := clusterProviderWithKubeConfig("prod-eu-1", "kc", "")
	provider.Finalizers = []string{LegacyClusterProviderFinalizer}
	now := metav1.Now()
	provider.DeletionTimestamp = &now

	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		WithObjects(provider).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(
				_ context.Context,
				_ client.WithWatch,
				_ client.Object,
				_ ...client.UpdateOption,
			) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "configbutler.ai", Resource: "clusterproviders"},
					"prod-eu-1", errors.New("conflict"),
				)
			},
		}).
		Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

	_, err := r.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "prod-eu-1"}},
	)
	require.Error(t, err, "a failed finalizer shed must requeue, not silently give up")
	assert.Contains(t, err.Error(), "remove retired finalizer")

	// The object is still held, which is exactly why the retry has to happen.
	var got configbutleraiv1alpha3.ClusterProvider
	require.NoError(t, cl.Get(context.Background(), k8stypes.NamespacedName{Name: "prod-eu-1"}, &got))
	assert.Contains(t, got.Finalizers, LegacyClusterProviderFinalizer)
}

// TestClusterProviderReconcile_InvalidStatusWriteFails checks that a failed status write on the
// INVALID path is propagated. The verdict itself (a bad kubeconfig) is not an error, but failing to
// record it is: without the error the provider would report a stale status for a whole steady
// interval before anything looked again.
func TestClusterProviderReconcile_InvalidStatusWriteFails(t *testing.T) {
	provider := clusterProviderWithKubeConfig("prod-eu-1", "absent-kc", "")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				_ context.Context,
				_ client.Client,
				_ string,
				_ client.Object,
				_ client.Patch,
				_ ...client.SubResourcePatchOption,
			) error {
				return errors.New("status write boom")
			},
		}).
		Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

	_, err := r.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "prod-eu-1"}},
	)
	require.Error(t, err, "a failed status write must requeue rather than report success")
	assert.Contains(t, err.Error(), "status write boom")
}

// TestClusterProviderReconcile_NotFound checks a deleted provider reconciles to a no-op.
func TestClusterProviderReconcile_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "gone"}})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
}

// TestValidateProviderKubeConfig_NilSecretRef checks a remote provider whose kubeConfig has no
// secretRef is rejected (belt to the CEL configMapRef rejection).
func TestValidateProviderKubeConfig_NilSecretRef(t *testing.T) {
	provider := &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
		Spec:       configbutleraiv1alpha3.ClusterProviderSpec{KubeConfig: &meta.KubeConfigReference{}},
	}
	r := &ClusterProviderReconciler{
		Client:            fake.NewClientBuilder().WithScheme(scScheme(t)).Build(),
		OperatorNamespace: cpOperatorNS,
	}
	ok, reason, msg, err := r.validateProviderKubeConfig(context.Background(), provider)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, ReasonKubeConfigInvalid, reason)
	assert.NotEmpty(t, msg)
}

// TestClusterProviderUpdateStatus_DeletedObject checks the shared status writer treats a vanished
// object as done rather than erroring: a reconcile that raced a delete must not fail the workqueue.
func TestClusterProviderUpdateStatus_DeletedObject(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		Build()
	provider := clusterProviderWithKubeConfig("gone", "", "")
	st := beginStatus(cl, nil, provider, &provider.Status.Conditions)
	st.set(ConditionTypeReady, metav1.ConditionTrue, ReasonSucceeded, "gone but written")

	// The object was never created, so the patch returns NotFound -> treated as done, no error.
	require.NoError(t, st.commit(context.Background()))
}

// TestClusterProviderReconcile_RemoteInvalidKubeConfig checks a remote provider whose kubeconfig
// is missing goes Validated=False / Stalled, not Ready.
func TestClusterProviderReconcile_RemoteInvalidKubeConfig(t *testing.T) {
	provider := clusterProviderWithKubeConfig("prod-eu-1", "absent-kc", "")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.ClusterProvider{}).
		Build()
	r := &ClusterProviderReconciler{Client: cl, OperatorNamespace: cpOperatorNS}

	_, err := r.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "prod-eu-1"}},
	)
	require.NoError(t, err)

	var got configbutleraiv1alpha3.ClusterProvider
	require.NoError(t, cl.Get(context.Background(), k8stypes.NamespacedName{Name: "prod-eu-1"}, &got))
	assert.Equal(
		t,
		metav1.ConditionFalse,
		findCondition(got.Status.Conditions, ClusterProviderConditionValidated).Status,
	)
	assert.Equal(
		t,
		kubeconfig.ReasonSecretNotFound,
		findCondition(got.Status.Conditions, ClusterProviderConditionValidated).Reason,
	)
	assert.Equal(t, metav1.ConditionFalse, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
	assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeStalled).Status)
}
