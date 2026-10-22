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
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// receiverNamespace is the namespace the GitReceiver fixtures below live in.
const receiverNamespace = "platform"

// testReceiver is the GitReceiver platform/github-app, whose token is the Secret "receiver-token".
func testReceiver(accessFrom *configbutleraiv1alpha3.NamespaceMatcher) *configbutleraiv1alpha3.GitReceiver {
	return &configbutleraiv1alpha3.GitReceiver{
		ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: receiverNamespace, Generation: 1},
		Spec: configbutleraiv1alpha3.GitReceiverSpec{
			Type:       configbutleraiv1alpha3.ReceiverTypeGitHub,
			SecretRef:  meta.LocalObjectReference{Name: "receiver-token"},
			AccessFrom: accessFrom,
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
		WithStatusSubresource(&configbutleraiv1alpha3.GitReceiver{}, &configbutleraiv1alpha3.GitProvider{}).
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
	receiver := testReceiver(nil)
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
	receiver := testReceiver(nil)
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
	receiver := testReceiver(nil)
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

// providerReferencing is a GitProvider in namespace whose spec.gitReceiverRef names
// platform/github-app (refNamespace "" leaves the reference's namespace to default).
func providerReferencing(namespace, refNamespace string) *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "customer-config", Namespace: namespace},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			URL:             "https://git.example.com/acme/config.git",
			AllowedBranches: []string{"main"},
			GitReceiverRef:  &meta.NamespacedObjectReference{Name: "github-app", Namespace: refNamespace},
		},
	}
}

// readyReceiver is testReceiver with Ready=True, as its reconciler would publish it.
func readyReceiver(accessFrom *configbutleraiv1alpha3.NamespaceMatcher) *configbutleraiv1alpha3.GitReceiver {
	receiver := testReceiver(accessFrom)
	receiver.Status.Conditions = []metav1.Condition{{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
		LastTransitionTime: metav1.Now(),
	}}
	return receiver
}

// observeReceiverCondition runs observeGitReceiver on provider against objects and returns the
// GitReceiverReady condition it left, or nil.
func observeReceiverCondition(
	t *testing.T, provider *configbutleraiv1alpha3.GitProvider, objects ...client.Object,
) *metav1.Condition {
	t.Helper()
	r := &GitProviderReconciler{Client: newReceiverTestClient(t, objects...)}
	st := beginStatus(r.Client, nil, provider)
	require.NoError(t, r.observeGitReceiver(context.Background(), st, provider))
	return findCondition(provider.Status.Conditions, GitProviderConditionGitReceiverReady)
}

func TestObserveGitReceiver(t *testing.T) {
	tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Labels: map[string]string{
		"tier": "trusted",
	}}}
	trusted := &configbutleraiv1alpha3.NamespaceMatcher{Selector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "trusted"},
	}}

	tests := []struct {
		name       string
		provider   *configbutleraiv1alpha3.GitProvider
		objects    []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "same namespace, receiver Ready",
			provider:   providerReferencing(receiverNamespace, ""),
			objects:    []client.Object{readyReceiver(nil)},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonSucceeded,
		},
		{
			name:       "receiver missing",
			provider:   providerReferencing(receiverNamespace, ""),
			wantStatus: metav1.ConditionFalse,
			wantReason: authz.ReasonGitReceiverNotFound,
		},
		{
			name:       "receiver not reconciled",
			provider:   providerReferencing(receiverNamespace, ""),
			objects:    []client.Object{testReceiver(nil)},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonGitReceiverNotReady,
		},
		{
			name:       "another namespace without accessFrom",
			provider:   providerReferencing("tenant-a", receiverNamespace),
			objects:    []client.Object{readyReceiver(nil), tenant},
			wantStatus: metav1.ConditionFalse,
			wantReason: authz.ReasonNamespaceNotAuthorized,
		},
		{
			name:       "another namespace the selector admits",
			provider:   providerReferencing("tenant-a", receiverNamespace),
			objects:    []client.Object{readyReceiver(trusted), tenant},
			wantStatus: metav1.ConditionTrue,
			wantReason: ReasonSucceeded,
		},
		{
			name: "an omitted reference namespace is the provider's own",
			// tenant-a/github-app does not exist; platform/github-app does and would admit it.
			provider:   providerReferencing("tenant-a", ""),
			objects:    []client.Object{readyReceiver(trusted), tenant},
			wantStatus: metav1.ConditionFalse,
			wantReason: authz.ReasonGitReceiverNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cond := observeReceiverCondition(t, tc.provider, tc.objects...)
			require.NotNil(t, cond)
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
			assert.Nil(t, findCondition(tc.provider.Status.Conditions, ConditionTypeReady),
				"GitReceiverReady is reported on its own and never writes the trio")
		})
	}
}

// TestObserveGitReceiver_RemovedWithoutReference drops the condition once the reference goes, so a
// provider that no longer names a receiver carries no stale answer about one.
func TestObserveGitReceiver_RemovedWithoutReference(t *testing.T) {
	provider := providerReferencing(receiverNamespace, "")
	provider.Spec.GitReceiverRef = nil
	provider.Status.Conditions = []metav1.Condition{{
		Type: GitProviderConditionGitReceiverReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
	}}
	assert.Nil(t, observeReceiverCondition(t, provider))
}

// TestGitReceiverToGitProviders enqueues every provider naming the receiver, from any namespace, and
// resolves an omitted reference namespace to the provider's own before comparing.
func TestGitReceiverToGitProviders(t *testing.T) {
	same := providerReferencing(receiverNamespace, "")
	same.Name = "same"
	other := providerReferencing("tenant-a", receiverNamespace)
	other.Name = "other"
	unrelated := providerReferencing("tenant-a", "")
	unrelated.Name = "unrelated" // names tenant-a/github-app
	none := providerReferencing(receiverNamespace, "")
	none.Name = "none"
	none.Spec.GitReceiverRef = nil

	r := &GitProviderReconciler{Client: newReceiverTestClient(t, same, other, unrelated, none)}
	requests := r.gitReceiverToGitProviders(context.Background(), testReceiver(nil))
	var got []string
	for _, req := range requests {
		got = append(got, req.String())
	}
	assert.ElementsMatch(t, []string{"platform/same", "tenant-a/other"}, got)
}

// TestNamespaceToGitProviders enqueues only the providers in the namespace whose receiver lives
// elsewhere: a label change can grant or revoke only an accessFrom admission.
func TestNamespaceToGitProviders(t *testing.T) {
	crossNamespace := providerReferencing("tenant-a", receiverNamespace)
	crossNamespace.Name = "cross"
	ownNamespace := providerReferencing("tenant-a", "")
	ownNamespace.Name = "own"
	elsewhere := providerReferencing("tenant-b", receiverNamespace)
	elsewhere.Name = "elsewhere"

	r := &GitProviderReconciler{Client: newReceiverTestClient(t, crossNamespace, ownNamespace, elsewhere)}
	requests := r.namespaceToGitProviders(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"}})
	require.Len(t, requests, 1)
	assert.Equal(t, "tenant-a/cross", requests[0].String())
}

// TestGitReceiverReadyOrSpecChanged filters the receiver's own status writes that move neither its
// spec nor its Ready verdict.
func TestGitReceiverReadyOrSpecChanged(t *testing.T) {
	p := gitReceiverReadyOrSpecChanged()
	base := readyReceiver(nil)

	assert.True(t, p.Create(event.CreateEvent{Object: base}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}))
	assert.False(t, p.Generic(event.GenericEvent{Object: base}))

	pathOnly := base.DeepCopy()
	pathOnly.Status.WebhookPath = "/git-push/moved"
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: pathOnly}))

	spec := base.DeepCopy()
	spec.Generation = 2
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: spec}))

	notReady := base.DeepCopy()
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: notReady}))
}
