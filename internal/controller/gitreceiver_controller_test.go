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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// receiverNamespace is the namespace the GitReceiver fixtures below live in.
const receiverNamespace = "platform"

// testReceiver is the GitReceiver platform/github-app, whose token is the Secret "receiver-token".
func testReceiver() *configbutleraiv1alpha3.GitReceiver {
	return &configbutleraiv1alpha3.GitReceiver{
		ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: receiverNamespace, Generation: 1},
		Spec: configbutleraiv1alpha3.GitReceiverSpec{
			Type:      configbutleraiv1alpha3.ReceiverTypeGitHub,
			SecretRef: meta.LocalObjectReference{Name: "receiver-token"},
		},
	}
}

// receiverTokenSecret is the Secret testReceiver reads, holding data.
func receiverTokenSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver-token", Namespace: receiverNamespace},
		Data:       data,
	}
}

func receiverTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	return scheme
}

func newReceiverTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(receiverTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.GitReceiver{}).
		Build()
}

func reconcileReceiver(
	t *testing.T, r *GitReceiverReconciler, key k8stypes.NamespacedName,
) (configbutleraiv1alpha3.GitReceiver, ctrl.Result) {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	var got configbutleraiv1alpha3.GitReceiver
	require.NoError(t, r.Get(context.Background(), key, &got))
	return got, result
}

// TestGitReceiverReconcile_PublishesPathFromToken pins status.webhookPath to the token: derived from
// it while the Secret and its key exist, moved when the token rotates, and cleared when either goes,
// so a stale path never points a Git host at a route that has stopped serving.
func TestGitReceiverReconcile_PublishesPathFromToken(t *testing.T) {
	ctx := context.Background()
	receiver := testReceiver()
	secret := receiverTokenSecret(map[string][]byte{configbutleraiv1alpha3.ReceiverTokenKey: []byte("first")})
	c := newReceiverTestClient(t, receiver, secret)
	r := &GitReceiverReconciler{Client: c, ListenerEnabled: true}

	got, result := reconcileReceiver(t, r, receiver.Key())
	first := got.Status.WebhookPath
	assert.Equal(t, configbutleraiv1alpha3.ReceiverPath([]byte("first"), receiverNamespace, "github-app"), first)
	assert.NotContains(t, first, receiverNamespace, "the path must not name the namespace")
	assert.NotContains(t, first, "github-app", "the path must not name the receiver")
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeReady))
	assert.Equal(t, int64(1), got.Status.ObservedGeneration)
	assert.Equal(t, RequeueSteadyInterval, result.RequeueAfter)

	secret.Data[configbutleraiv1alpha3.ReceiverTokenKey] = []byte("second")
	require.NoError(t, c.Update(ctx, secret))
	got, _ = reconcileReceiver(t, r, receiver.Key())
	assert.NotEqual(t, first, got.Status.WebhookPath, "a rotated token moves the path")

	secret.Data = map[string][]byte{"password": []byte("second")}
	require.NoError(t, c.Update(ctx, secret))
	got, result = reconcileReceiver(t, r, receiver.Key())
	assert.Empty(t, got.Status.WebhookPath, "no token, no path")
	assertStalled(t, got.Status.Conditions, ReasonSecretMalformed)
	assert.Equal(t, RequeueReceiverTokenInterval, result.RequeueAfter,
		"nothing watches the Secret, so a receiver without a token looks again soon")

	require.NoError(t, c.Delete(ctx, secret))
	got, result = reconcileReceiver(t, r, receiver.Key())
	assert.Empty(t, got.Status.WebhookPath, "no Secret, no path")
	assertStalled(t, got.Status.Conditions, ReasonSecretNotFound)
	assert.Equal(t, RequeueReceiverTokenInterval, result.RequeueAfter)
}

// TestGitReceiverReconcile_ListenerDisabled publishes the path, so the hook can be registered, and
// stalls: no delivery reaches a receiver the operator does not serve.
func TestGitReceiverReconcile_ListenerDisabled(t *testing.T) {
	receiver := testReceiver()
	secret := receiverTokenSecret(map[string][]byte{configbutleraiv1alpha3.ReceiverTokenKey: []byte("t")})
	r := &GitReceiverReconciler{Client: newReceiverTestClient(t, receiver, secret)}

	got, result := reconcileReceiver(t, r, receiver.Key())
	assert.NotEmpty(t, got.Status.WebhookPath, "the path is published even while nothing serves it")
	assertStalled(t, got.Status.Conditions, GitReceiverReasonListenerDisabled)
	assert.Equal(t, RequeueSteadyInterval, result.RequeueAfter)
}

// TestGitReceiverReconcile_SecretReadFails keeps what is published and retries: a read that failed
// says nothing about the token.
func TestGitReceiverReconcile_SecretReadFails(t *testing.T) {
	receiver := testReceiver()
	receiver.Status.WebhookPath = "/git-push/kept"
	c := fake.NewClientBuilder().
		WithScheme(receiverTestScheme(t)).
		WithObjects(receiver).
		WithStatusSubresource(&configbutleraiv1alpha3.GitReceiver{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.New("get boom")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &GitReceiverReconciler{Client: c, ListenerEnabled: true}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: receiver.Key()})
	require.Error(t, err)
	var got configbutleraiv1alpha3.GitReceiver
	require.NoError(t, c.Get(context.Background(), receiver.Key(), &got))
	assert.Equal(t, "/git-push/kept", got.Status.WebhookPath)
}

// TestGitReceiverReconcile_Gone is not an error.
func TestGitReceiverReconcile_Gone(t *testing.T) {
	r := &GitReceiverReconciler{Client: newReceiverTestClient(t)}
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: k8stypes.NamespacedName{Namespace: receiverNamespace, Name: "gone"},
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func assertStalled(t *testing.T, conditions []metav1.Condition, reason string) {
	t.Helper()
	stalled := findCondition(conditions, ConditionTypeStalled)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionTrue, stalled.Status)
	assert.Equal(t, reason, stalled.Reason)
	assert.True(t, conditionIsFalse(conditions, ConditionTypeReady))
}
