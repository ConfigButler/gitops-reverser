// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

// kpNamespace is the namespace the KubeProvider fixtures below live in.
const kpNamespace = "team-a"

// inClusterKubeProvider is a KubeProvider in kpNamespace that asks for the operator's own identity.
// It is honored only by a reconciler whose KubeConfigSafety lists it (allowInCluster).
func inClusterKubeProvider(name string) *configbutleraiv1alpha3.KubeProvider {
	return &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kpNamespace},
		Spec:       configbutleraiv1alpha3.KubeProviderSpec{InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{}},
	}
}

// remoteKubeProvider is the KubeProvider kpNamespace/prod-eu-1 whose kubeconfig Secret, in that
// same namespace, is secretName.
func remoteKubeProvider(secretName, key string) *configbutleraiv1alpha3.KubeProvider {
	return &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1", Namespace: kpNamespace},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			SecretRef: &meta.SecretKeyReference{Name: secretName, Key: key},
		},
	}
}

// allowInCluster is the installation's --in-cluster-kube-providers listing exactly providers.
func allowInCluster(providers ...*configbutleraiv1alpha3.KubeProvider) kubeconfig.SafetyPolicy {
	allowed := map[k8stypes.NamespacedName]struct{}{}
	for _, p := range providers {
		allowed[p.Key()] = struct{}{}
	}
	return kubeconfig.SafetyPolicy{InClusterProviders: allowed}
}

// kubeconfigSecret is the Secret "kc" in namespace holding data.
func kubeconfigSecret(namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "kc"}, Data: data}
}

func TestValidateCredential_AllScenarios(t *testing.T) {
	listed := inClusterKubeProvider("local")
	tests := []struct {
		name       string
		provider   *configbutleraiv1alpha3.KubeProvider
		secret     *corev1.Secret
		safety     kubeconfig.SafetyPolicy
		wantOK     bool
		wantReason string
	}{
		{
			name:     "inCluster listed by the installation is valid",
			provider: listed,
			safety:   allowInCluster(listed),
			wantOK:   true, wantReason: ReasonInCluster,
		},
		{
			name:     "inCluster the installation lists none of is refused",
			provider: inClusterKubeProvider("local"),
			wantOK:   false, wantReason: kubeconfig.ReasonInClusterNotAuthorized,
		},
		{
			name:     "inCluster under another name than the one listed is refused",
			provider: inClusterKubeProvider("sneaky"),
			safety:   allowInCluster(listed),
			wantOK:   false, wantReason: kubeconfig.ReasonInClusterNotAuthorized,
		},
		{
			name: "neither arm set is invalid",
			provider: &configbutleraiv1alpha3.KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: kpNamespace},
			},
			wantOK:     false,
			wantReason: ReasonKubeConfigInvalid,
		},
		{
			name:     "missing Secret",
			provider: remoteKubeProvider("absent", ""),
			wantOK:   false, wantReason: kubeconfig.ReasonSecretNotFound,
		},
		{
			// The Secret is read from the provider's OWN namespace, never the operator's or any
			// other: a provider must not be able to wield a credential its namespace does not hold.
			name:     "Secret only in another namespace is not found",
			provider: remoteKubeProvider("kc", ""),
			secret: kubeconfigSecret("gitops-reverser-system",
				map[string][]byte{"value": []byte(scValidKubeConfig)}),
			wantOK: false, wantReason: kubeconfig.ReasonSecretNotFound,
		},
		{
			name:       "missing key",
			provider:   remoteKubeProvider("kc", "value"),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"elsewhere": []byte(scValidKubeConfig)}),
			wantOK:     false,
			wantReason: kubeconfig.ReasonKeyNotFound,
		},
		{
			name:       "unparseable",
			provider:   remoteKubeProvider("kc", ""),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"value": []byte("not a kubeconfig")}),
			wantOK:     false,
			wantReason: kubeconfig.ReasonInvalid,
		},
		{
			name:       "exec rejected",
			provider:   remoteKubeProvider("kc", ""),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"value": []byte(scExecKubeConfig)}),
			wantOK:     false,
			wantReason: kubeconfig.ReasonExecNotAllowed,
		},
		{
			name:       "insecure TLS rejected",
			provider:   remoteKubeProvider("kc", ""),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"value": []byte(scInsecureKubeConfig)}),
			wantOK:     false,
			wantReason: kubeconfig.ReasonInsecureTLSNotAllowed,
		},
		{
			name:       "valid via value.yaml fallback in the provider's namespace",
			provider:   remoteKubeProvider("kc", ""),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"value.yaml": []byte(scValidKubeConfig)}),
			wantOK:     true,
			wantReason: ReasonSucceeded,
		},
		{
			name:       "exec allowed when opted in",
			provider:   remoteKubeProvider("kc", ""),
			secret:     kubeconfigSecret(kpNamespace, map[string][]byte{"value": []byte(scExecKubeConfig)}),
			safety:     kubeconfig.SafetyPolicy{AllowExec: true},
			wantOK:     true,
			wantReason: ReasonSucceeded,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.secret != nil {
				builder = builder.WithObjects(tc.secret)
			}
			r := &KubeProviderReconciler{Client: builder.Build(), KubeConfigSafety: tc.safety}
			ok, reason, msg, err := r.validateCredential(context.Background(), tc.provider)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantReason, reason)
			assert.NotEmpty(t, msg)
		})
	}
}

// TestValidateCredential_SecretReadErrorRequeues pins that a transient Secret read failure is an
// error, not a verdict: reporting it as SecretNotFound would flap the provider's status.
func TestValidateCredential_SecretReadErrorRequeues(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object,
				_ ...client.GetOption) error {
				return errors.New("api server unavailable")
			},
		}).Build()
	r := &KubeProviderReconciler{Client: cl}
	ok, reason, _, err := r.validateCredential(context.Background(), remoteKubeProvider("kc", ""))
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, reason)
}

// reconcileKubeProviders seeds a fake client with providers (and any extra objects), reconciles
// each provider once with safety, and returns the client to read the results from.
func reconcileKubeProviders(
	t *testing.T,
	safety kubeconfig.SafetyPolicy,
	providers []*configbutleraiv1alpha3.KubeProvider,
	extra ...client.Object,
) client.Client {
	t.Helper()
	objects := append([]client.Object{}, extra...)
	for _, p := range providers {
		objects = append(objects, p)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.KubeProvider{}).
		Build()
	r := &KubeProviderReconciler{Client: cl, KubeConfigSafety: safety}
	for _, p := range providers {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: p.Key()})
		require.NoError(t, err)
	}
	return cl
}

func getKubeProvider(t *testing.T, cl client.Client, key k8stypes.NamespacedName) configbutleraiv1alpha3.KubeProvider {
	t.Helper()
	var got configbutleraiv1alpha3.KubeProvider
	require.NoError(t, cl.Get(context.Background(), key, &got))
	return got
}

// TestKubeProviderReconcile_InCluster checks the installation's consent decides an inCluster
// provider: listed in --in-cluster-kube-providers it validates and goes Ready; not listed it is
// Validated=False and Stalled with InClusterNotAuthorized, and is never used.
func TestKubeProviderReconcile_InCluster(t *testing.T) {
	tests := []struct {
		name          string
		allowed       bool
		wantValidated metav1.ConditionStatus
		wantReason    string
		wantReady     metav1.ConditionStatus
		wantStalled   metav1.ConditionStatus
	}{
		{
			name: "listed", allowed: true,
			wantValidated: metav1.ConditionTrue, wantReason: ReasonInCluster,
			wantReady: metav1.ConditionTrue, wantStalled: metav1.ConditionFalse,
		},
		{
			name: "not listed", allowed: false,
			wantValidated: metav1.ConditionFalse, wantReason: kubeconfig.ReasonInClusterNotAuthorized,
			wantReady: metav1.ConditionFalse, wantStalled: metav1.ConditionTrue,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := inClusterKubeProvider("local")
			safety := kubeconfig.SafetyPolicy{}
			if tc.allowed {
				safety = allowInCluster(provider)
			}
			cl := reconcileKubeProviders(t, safety, []*configbutleraiv1alpha3.KubeProvider{provider})

			got := getKubeProvider(t, cl, provider.Key())
			validated := findCondition(got.Status.Conditions, KubeProviderConditionValidated)
			require.NotNil(t, validated)
			assert.Equal(t, tc.wantValidated, validated.Status)
			assert.Equal(t, tc.wantReason, validated.Reason)
			assert.Equal(t, tc.wantReady, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
			stalled := findCondition(got.Status.Conditions, ConditionTypeStalled)
			assert.Equal(t, tc.wantStalled, stalled.Status)
			if tc.wantStalled == metav1.ConditionTrue {
				assert.Equal(t, tc.wantReason, stalled.Reason)
				assert.Contains(t, stalled.Message, "--in-cluster-kube-providers")
			}
			assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
		})
	}
}

// TestKubeProviderReconcile_SecretInOwnNamespace checks a secretRef provider reads its kubeconfig
// from its OWN namespace: present there it is Ready, present only in another namespace it is
// Validated=False with SecretNotFound.
func TestKubeProviderReconcile_SecretInOwnNamespace(t *testing.T) {
	tests := []struct {
		name            string
		secretNamespace string
		wantValidated   metav1.ConditionStatus
		wantReason      string
	}{
		{"Secret in the provider's namespace", kpNamespace, metav1.ConditionTrue, ReasonSucceeded},
		{"Secret only in another namespace", "team-b", metav1.ConditionFalse, kubeconfig.ReasonSecretNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := remoteKubeProvider("kc", "")
			secret := kubeconfigSecret(tc.secretNamespace, map[string][]byte{"value": []byte(scValidKubeConfig)})
			cl := reconcileKubeProviders(t, kubeconfig.SafetyPolicy{},
				[]*configbutleraiv1alpha3.KubeProvider{provider}, secret)

			got := getKubeProvider(t, cl, provider.Key())
			validated := findCondition(got.Status.Conditions, KubeProviderConditionValidated)
			require.NotNil(t, validated)
			assert.Equal(t, tc.wantValidated, validated.Status)
			assert.Equal(t, tc.wantReason, validated.Reason)
			assert.Equal(t, tc.wantValidated, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
		})
	}
}

// TestKubeProviderReconcile_AuditRouteConflict covers two providers in different namespaces that
// spell out one route. The route names neither namespace, so the older provider's namespace holds
// it: the holder stays Ready, and the younger one is Stalled with AuditRouteConflict while its
// Validated keeps what its credential earned, because nothing is wrong with the credential.
func TestKubeProviderReconcile_AuditRouteConflict(t *testing.T) {
	older := inClusterKubeProvider("first")
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	older.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "shared"}
	younger := inClusterKubeProvider("second")
	younger.Namespace = "team-b"
	younger.CreationTimestamp = metav1.NewTime(time.Now())
	younger.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "shared"}

	cl := reconcileKubeProviders(t, allowInCluster(older, younger),
		[]*configbutleraiv1alpha3.KubeProvider{older, younger})

	holder := getKubeProvider(t, cl, older.Key())
	assert.Equal(t, metav1.ConditionTrue, findCondition(holder.Status.Conditions, ConditionTypeReady).Status)
	assert.Equal(t, metav1.ConditionFalse, findCondition(holder.Status.Conditions, ConditionTypeStalled).Status)

	loser := getKubeProvider(t, cl, younger.Key())
	assert.Equal(t, metav1.ConditionTrue,
		findCondition(loser.Status.Conditions, KubeProviderConditionValidated).Status,
		"a route held elsewhere says nothing about the credential")
	stalled := findCondition(loser.Status.Conditions, ConditionTypeStalled)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionTrue, stalled.Status)
	assert.Equal(t, authz.ReasonAuditRouteConflict, stalled.Reason)
	assert.Contains(t, stalled.Message, `"team-a"`, "the message names the namespace holding the route")
	ready := findCondition(loser.Status.Conditions, ConditionTypeReady)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, authz.ReasonAuditRouteConflict, ready.Reason)
}

// TestKubeProviderReconcile_AuditRouteListFails pins that a failed read of the other providers is
// an error the workqueue retries, never a verdict on the route.
func TestKubeProviderReconcile_AuditRouteListFails(t *testing.T) {
	provider := inClusterKubeProvider("local")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.KubeProvider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("list boom")
			},
		}).
		Build()
	r := &KubeProviderReconciler{Client: cl, KubeConfigSafety: allowInCluster(provider)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: provider.Key()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list boom")
}

// TestKubeProviderReconcile_TakesNoFinalizer pins the contract: this controller never makes a
// KubeProvider undeletable. Nothing has to happen before one goes away — attribution facts are
// keyed by (audit route, group/resource, uid, resourceVersion) and expire on their own, and several
// providers may share one route, so there is nothing to purge per object.
func TestKubeProviderReconcile_TakesNoFinalizer(t *testing.T) {
	provider := inClusterKubeProvider("local")
	cl := reconcileKubeProviders(t, allowInCluster(provider), []*configbutleraiv1alpha3.KubeProvider{provider})

	got := getKubeProvider(t, cl, provider.Key())
	assert.Empty(t, got.Finalizers, "a KubeProvider must never be held by this controller")
}

// TestKubeProviderReconcile_DeletingIsNoOp checks a provider already being deleted (held by some
// other controller's finalizer) gets no status write: it is on its way out.
func TestKubeProviderReconcile_DeletingIsNoOp(t *testing.T) {
	provider := inClusterKubeProvider("local")
	now := metav1.Now()
	provider.DeletionTimestamp = &now
	provider.Finalizers = []string{"example.com/elsewhere"}
	cl := reconcileKubeProviders(t, allowInCluster(provider), []*configbutleraiv1alpha3.KubeProvider{provider})

	got := getKubeProvider(t, cl, provider.Key())
	assert.Empty(t, got.Status.Conditions)
}

// TestKubeProviderReconcile_InvalidStatusWriteFails checks that a failed status write on the
// INVALID path is propagated. The verdict itself (a bad kubeconfig) is not an error, but failing to
// record it is: without the error the provider would report a stale status for a whole steady
// interval before anything looked again.
func TestKubeProviderReconcile_InvalidStatusWriteFails(t *testing.T) {
	provider := remoteKubeProvider("absent-kc", "")
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithObjects(provider).
		WithStatusSubresource(&configbutleraiv1alpha3.KubeProvider{}).
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
	r := &KubeProviderReconciler{Client: cl}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: provider.Key()})
	require.Error(t, err, "a failed status write must requeue rather than report success")
	assert.Contains(t, err.Error(), "status write boom")
}

// TestKubeProviderReconcile_NotFound checks a deleted provider reconciles to a no-op.
func TestKubeProviderReconcile_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).Build()
	r := &KubeProviderReconciler{Client: cl}
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: kpNamespace, Name: "gone"}})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
}

// TestKubeProviderReconcile_GetFails checks a non-NotFound read error is returned for a retry.
func TestKubeProviderReconcile_GetFails(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object,
				_ ...client.GetOption) error {
				return errors.New("get boom")
			},
		}).Build()
	r := &KubeProviderReconciler{Client: cl}
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: kpNamespace, Name: "local"}})
	require.Error(t, err)
}

// TestReconcileStatusCommit_DeletedObject checks the shared status writer treats a vanished
// object as done rather than erroring: a reconcile that raced a delete must not fail the workqueue.
func TestReconcileStatusCommit_DeletedObject(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(scScheme(t)).
		WithStatusSubresource(&configbutleraiv1alpha3.KubeProvider{}).
		Build()
	provider := inClusterKubeProvider("gone")
	st := beginStatus(cl, nil, provider)
	st.set(ConditionTypeReady, metav1.ConditionTrue, ReasonSucceeded, "gone but written")

	// The object was never created, so the patch returns NotFound -> treated as done, no error.
	require.NoError(t, st.commit(context.Background()))
}

// TestKubeProviderReconcile_RemoteInvalidKubeConfig checks a remote provider whose kubeconfig
// is missing goes Validated=False / Stalled, not Ready.
func TestKubeProviderReconcile_RemoteInvalidKubeConfig(t *testing.T) {
	provider := remoteKubeProvider("absent-kc", "")
	cl := reconcileKubeProviders(t, kubeconfig.SafetyPolicy{}, []*configbutleraiv1alpha3.KubeProvider{provider})

	got := getKubeProvider(t, cl, provider.Key())
	validated := findCondition(got.Status.Conditions, KubeProviderConditionValidated)
	assert.Equal(t, metav1.ConditionFalse, validated.Status)
	assert.Equal(t, kubeconfig.ReasonSecretNotFound, validated.Reason)
	assert.Equal(t, metav1.ConditionFalse, findCondition(got.Status.Conditions, ConditionTypeReady).Status)
	assert.Equal(t, metav1.ConditionTrue, findCondition(got.Status.Conditions, ConditionTypeStalled).Status)
}

// drainRequests empties q and returns what was queued.
func drainRequests(q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) []k8stypes.NamespacedName {
	var out []k8stypes.NamespacedName
	for q.Len() > 0 {
		item, _ := q.Get()
		q.Done(item)
		out = append(out, item.NamespacedName)
	}
	return out
}

// TestAuditRouteSiblingsHandler checks the wake path for AuditRouteConflict: a provider created,
// deleted or moved to another route enqueues every OTHER provider carrying the route it held or now
// holds, since each may have just won or lost it. An update keeping the route enqueues nothing, so
// status writes cost no fan-out.
func TestAuditRouteSiblingsHandler(t *testing.T) {
	withRoute := func(namespace, name, route string) *configbutleraiv1alpha3.KubeProvider {
		p := inClusterKubeProvider(name)
		p.Namespace = namespace
		if route != "" {
			p.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: route}
		}
		return p
	}
	self := withRoute("team-a", "self", "shared")
	sharedSibling := withRoute("team-b", "sibling", "shared")
	otherSibling := withRoute("team-b", "other", "other")
	unrelated := withRoute("team-c", "unrelated", "")

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(self, sharedSibling, otherSibling, unrelated).Build()
	h := auditRouteSiblingsHandler(cl)

	moved := self.DeepCopy()
	moved.Spec.Attribution.AuditRoute = "other"
	statusOnly := self.DeepCopy()
	statusOnly.Status.AuditRoute = "shared"

	tests := []struct {
		name string
		fire func(context.Context, workqueue.TypedRateLimitingInterface[ctrlreconcile.Request])
		want []k8stypes.NamespacedName
	}{
		{
			name: "create wakes the siblings on its route, not itself",
			fire: func(ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: self}, q)
			},
			want: []k8stypes.NamespacedName{sharedSibling.Key()},
		},
		{
			name: "delete frees the route for its siblings",
			fire: func(ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
				h.Delete(ctx, event.DeleteEvent{Object: self}, q)
			},
			want: []k8stypes.NamespacedName{sharedSibling.Key()},
		},
		{
			name: "a route change wakes the carriers of the old and the new route",
			fire: func(ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{ObjectOld: self, ObjectNew: moved}, q)
			},
			want: []k8stypes.NamespacedName{sharedSibling.Key(), otherSibling.Key()},
		},
		{
			name: "an update keeping the route wakes nothing",
			fire: func(ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{ObjectOld: self, ObjectNew: statusOnly}, q)
			},
		},
		{
			name: "an object that is not a KubeProvider wakes nothing",
			fire: func(ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: &corev1.Namespace{}}, q)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := workqueue.NewTypedRateLimitingQueue(
				workqueue.DefaultTypedControllerRateLimiter[ctrlreconcile.Request]())
			defer q.ShutDown()
			tc.fire(context.Background(), q)
			assert.ElementsMatch(t, tc.want, drainRequests(q))
		})
	}
}
