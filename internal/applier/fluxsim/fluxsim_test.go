// SPDX-License-Identifier: Apache-2.0

package fluxsim

import (
	"context"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// The fake is what every test of the suspended lifecycle trusts, so it is held to the source facts
// it claims, one test each.

var (
	ksKey     = types.NamespacedName{Namespace: "flux-system", Name: "apps"}
	sourceKey = types.NamespacedName{Namespace: "flux-system", Name: "repo"}
)

const (
	oldRevision = "main@sha1:0000000000000000000000000000000000000001"
	newRevision = "main@sha1:4f2c1ab9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f"
	manager     = "gitops-reverser"
)

func newSim(t *testing.T, suspended bool) *Sim {
	t.Helper()
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": kindKustomization,
		"metadata": map[string]any{"namespace": ksKey.Namespace, "name": ksKey.Name},
		"spec":     map[string]any{"sourceRef": map[string]any{"kind": kindGitRepository, "name": sourceKey.Name}},
	}}
	ks.SetGeneration(1)
	if suspended {
		ks = OwnSuspend(t, ks, true, manager, metav1.ManagedFieldsOperationApply)
	}
	SetReady(ks, metav1.ConditionTrue, fluxmeta.ReconciliationSucceededReason, "applied")
	source := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": kindGitRepository,
		"metadata": map[string]any{"namespace": sourceKey.Namespace, "name": sourceKey.Name},
		"status":   map[string]any{"artifact": map[string]any{"revision": oldRevision}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			KustomizationGVR(): "KustomizationList", GitRepositoryGVR(): "GitRepositoryList",
		}, ks, source)
	sim := New(t, client)
	sim.Fetches(sourceKey, newRevision)
	return sim
}

func (s *Sim) apply(t *testing.T, suspend *bool, token string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("kustomize.toolkit.fluxcd.io/v1")
	obj.SetKind(kindKustomization)
	obj.SetNamespace(ksKey.Namespace)
	obj.SetName(ksKey.Name)
	if token != "" {
		obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: token})
	}
	if suspend != nil {
		require.NoError(t, unstructured.SetNestedField(obj.Object, *suspend, "spec", "suspend"))
	}
	_, err := s.Client.Resource(KustomizationGVR()).Namespace(ksKey.Namespace).
		Apply(context.Background(), ksKey.Name, obj, metav1.ApplyOptions{FieldManager: manager, Force: true})
	require.NoError(t, err)
}

func (s *Sim) stampSource(t *testing.T, token string) {
	t.Helper()
	patch := []byte(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"` + token + `"}}}`)
	_, err := s.Client.Resource(GitRepositoryGVR()).Namespace(sourceKey.Namespace).
		Patch(context.Background(), sourceKey.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)
}

func handled(s *Sim, token string) func() bool {
	return func() bool {
		return statusString(s.Kustomization(ksKey), "lastHandledReconcileAt") == token
	}
}

func eventually(t *testing.T, cond func() bool, why string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, time.Millisecond, why)
}

func ptr(b bool) *bool { return &b }

// A suspended object returns before it resolves its source, and finalizeStatus still marks the
// token it read handled (:189-192, :143). Its Ready is left as it was.
func TestSim_ASuspendedReconcileHandlesItsTokenAndAppliesNothing(t *testing.T) {
	sim := newSim(t, true)
	sim.stampSource(t, "S1")
	eventually(t, func() bool {
		return statusString(sim.Kustomization(ksKey), "lastHandledReconcileAt") == "" &&
			sim.Reconciles(ksKey) > 0
	}, "a new artifact enqueues the consumer")
	sim.apply(t, ptr(true), "T1")
	eventually(t, handled(sim, "T1"), "the token is handled")
	assert.Empty(t, sim.Applied(), "nothing applied")
	assert.Equal(t, string(metav1.ConditionTrue), readyStatus(sim.Kustomization(ksKey)))
}

// One write that sets suspend: false and a token enqueues a reconcile that reads both, and
// applies whatever the artifact holds then; the generation it read is observed on success.
func TestSim_AResumeAppliesTheArtifact(t *testing.T) {
	sim := newSim(t, true)
	sim.apply(t, ptr(false), "T1")
	eventually(t, handled(sim, "T1"), "the token is handled")
	assert.Equal(t, []string{"flux-system/apps " + oldRevision}, sim.Applied())
	ks := sim.Kustomization(ksKey)
	assert.Equal(t, oldRevision, statusString(ks, "lastAppliedRevision"))
	observed, _, _ := unstructured.NestedInt64(ks.Object, "status", "observedGeneration")
	assert.Equal(t, ks.GetGeneration(), observed)
}

// spec.suspend is read when a reconcile starts: a suspend written while an apply runs does not
// stop it, and the next reconcile, which reads the suspend, handles its token after it.
func TestSim_ASuspendDoesNotStopARunningApply(t *testing.T) {
	sim := newSim(t, true)
	held := sim.HoldNextApply()
	sim.apply(t, ptr(false), "T1")
	<-held
	assert.Equal(t, oldRevision, statusString(sim.Kustomization(ksKey), "lastAttemptedRevision"),
		"attempted before it builds")
	sim.apply(t, ptr(true), "T2")
	sim.Release()
	eventually(t, handled(sim, "T2"), "the suspend's token is handled after the apply")
	assert.Len(t, sim.Applied(), 1, "the running apply finished")
}

// A failed apply and a dependency that is not ready are Ready=False, each with its reason, and
// mark the token handled all the same; a requeue into a suspended object applies nothing.
func TestSim_FailuresAndRequeues(t *testing.T) {
	sim := newSim(t, true)
	sim.FailApplies("dry-run failed")
	sim.apply(t, ptr(false), "T1")
	eventually(t, handled(sim, "T1"), "a failure handles the token")
	assert.Equal(t, string(metav1.ConditionFalse), readyStatus(sim.Kustomization(ksKey)))

	sim.FailApplies("")
	sim.BlockDependencies(true)
	sim.apply(t, ptr(false), "T2")
	eventually(t, handled(sim, "T2"), "so does a dependency not ready")
	conditions, _, _ := unstructured.NestedSlice(sim.Kustomization(ksKey).Object, "status", "conditions")
	require.Len(t, conditions, 1)
	reason, _ := conditions[0].(map[string]any)["reason"].(string)
	assert.Equal(t, fluxmeta.DependencyNotReadyReason, reason)

	sim.BlockDependencies(false)
	sim.apply(t, ptr(true), "T3")
	eventually(t, handled(sim, "T3"), "suspended")
	before := sim.Reconciles(ksKey)
	sim.Requeue(ksKey)
	eventually(t, func() bool { return sim.Reconciles(ksKey) > before }, "the requeue runs")
	assert.Empty(t, sim.Applied())
}

// The ownership a real API server keeps (measured in envtest,
// internal/controller/fluxclaim_suspend_apiserver_test.go): a merge patch owns what it changes,
// as an update; an apply of the value a field holds shares it, even forced; a write that changes
// the value takes it from every other owner; an apply that leaves a field out gives up its share,
// and removes the field only when nobody else owns it.
func TestSim_ManagedFields(t *testing.T) {
	sim := newSim(t, false)
	update := owner{manager: "flux", operation: metav1.ManagedFieldsOperationUpdate}
	apply := owner{manager: manager, operation: metav1.ManagedFieldsOperationApply}
	mergePatch := func(suspend string) {
		t.Helper()
		_, err := sim.Client.Resource(KustomizationGVR()).Namespace(ksKey.Namespace).Patch(context.Background(),
			ksKey.Name, types.MergePatchType, []byte(`{"spec":{"suspend":`+suspend+`}}`),
			metav1.PatchOptions{FieldManager: "flux"})
		require.NoError(t, err)
	}
	suspendOwners := func() []owner { return owners(sim.Kustomization(ksKey))[simFieldSuspend] }

	mergePatch("true")
	assert.Equal(t, []owner{update}, suspendOwners())

	sim.apply(t, ptr(true), "T1")
	assert.ElementsMatch(t, []owner{update, apply}, suspendOwners(), "the same value is shared, forced or not")

	sim.apply(t, nil, "")
	ks := sim.Kustomization(ksKey)
	assert.True(t, suspended(ks), "the other owner keeps it")
	assert.Equal(t, []owner{update}, suspendOwners())
	assert.Empty(t, ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])

	sim.apply(t, ptr(false), "T2")
	assert.Equal(t, []owner{apply}, suspendOwners(), "another value is taken from every owner")
	mergePatch("false")
	assert.Equal(t, []owner{apply}, suspendOwners(), "an update of the value the field holds takes nothing")

	sim.apply(t, nil, "")
	ks = sim.Kustomization(ksKey)
	_, found, _ := unstructured.NestedFieldNoCopy(ks.Object, "spec", "suspend")
	assert.False(t, found, "released")
	assert.Empty(t, owners(ks))
}

// A status write and a patch are each one step, as the API server's resourceVersion check makes
// them: a resume that arrives while a reconcile is writing its status lands after that write, and is
// not written away by it. CI once lost a resume this way, and the operation never ended.
func TestSim_AStatusWriteNeverWritesAResumeAway(t *testing.T) {
	sim := newSim(t, true)
	defer sim.PauseKustomizations()()

	read, release := make(chan struct{}), make(chan struct{})
	written := make(chan struct{})
	go func() {
		defer close(written)
		sim.WriteStatus(context.Background(), KustomizationGVR(), ksKey, func(ks *unstructured.Unstructured) {
			close(read)
			<-release
			_ = unstructured.SetNestedField(ks.Object, "T0", "status", "lastHandledReconcileAt")
		})
	}()
	<-read

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("kustomize.toolkit.fluxcd.io/v1")
	obj.SetKind(kindKustomization)
	obj.SetNamespace(ksKey.Namespace)
	obj.SetName(ksKey.Name)
	obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: "T1"})
	require.NoError(t, unstructured.SetNestedField(obj.Object, false, "spec", "suspend"))
	resumed := make(chan error, 1)
	go func() {
		_, err := sim.Client.Resource(KustomizationGVR()).Namespace(ksKey.Namespace).Apply(context.Background(),
			ksKey.Name, obj, metav1.ApplyOptions{FieldManager: manager, Force: true})
		resumed <- err
	}()
	select {
	case err := <-resumed:
		t.Fatalf("the resume landed inside the status write (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-written
	require.NoError(t, <-resumed)

	ks := sim.Kustomization(ksKey)
	assert.False(t, suspended(ks), "the resume stands")
	assert.Equal(t, "T1", ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation])
	handled, _, _ := unstructured.NestedString(ks.Object, "status", "lastHandledReconcileAt")
	assert.Equal(t, "T0", handled, "and so does the status write")
}

// A write that changes the object moves its resourceVersion, and one that changes nothing does
// not. A server-side apply that carries a resourceVersion or a uid has them checked as the API
// server checks them (measured in envtest): a resourceVersion that moved is a Conflict, another
// object's uid is Invalid, and a uid with no object is a Conflict rather than a create.
func TestSim_ApplyPreconditions(t *testing.T) {
	sim := newSim(t, true)
	sim.Edit(ksKey, func(ks *unstructured.Unstructured) { ks.SetUID("apps-1") })
	read := sim.Kustomization(ksKey)
	require.NotEmpty(t, read.GetResourceVersion())
	sim.Edit(ksKey, func(*unstructured.Unstructured) {})
	assert.Equal(t, read.GetResourceVersion(), sim.Kustomization(ksKey).GetResourceVersion(),
		"a write that changes nothing")

	resources := sim.Client.Resource(KustomizationGVR()).Namespace(ksKey.Namespace)
	applyOver := func(resourceVersion, uid string) error {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("kustomize.toolkit.fluxcd.io/v1")
		obj.SetKind(kindKustomization)
		obj.SetNamespace(ksKey.Namespace)
		obj.SetName(ksKey.Name)
		obj.SetResourceVersion(resourceVersion)
		obj.SetUID(types.UID(uid))
		require.NoError(t, unstructured.SetNestedField(obj.Object, false, "spec", "suspend"))
		_, err := resources.Apply(context.Background(), ksKey.Name, obj,
			metav1.ApplyOptions{FieldManager: manager, Force: true})
		return err
	}

	sim.Edit(ksKey, func(ks *unstructured.Unstructured) { ks.SetLabels(map[string]string{"team": "a"}) })
	require.True(t, apierrors.IsConflict(applyOver(read.GetResourceVersion(), "apps-1")))
	assert.True(t, suspended(sim.Kustomization(ksKey)), "not applied")
	read = sim.Kustomization(ksKey)
	require.True(t, apierrors.IsInvalid(applyOver(read.GetResourceVersion(), "apps-2")))
	require.NoError(t, applyOver(read.GetResourceVersion(), "apps-1"))
	assert.False(t, suspended(sim.Kustomization(ksKey)))
	assert.NotEqual(t, read.GetResourceVersion(), sim.Kustomization(ksKey).GetResourceVersion())

	require.NoError(t, resources.Delete(context.Background(), ksKey.Name, metav1.DeleteOptions{}))
	require.True(t, apierrors.IsConflict(applyOver("", "apps-1")), "no create")
}

func suspended(ks *unstructured.Unstructured) bool {
	value, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	return value
}

func TestTickingClock_NeverRepeats(t *testing.T) {
	start := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	clock := TickingClock(start)
	assert.Equal(t, start, clock())
	assert.Equal(t, start.Add(time.Millisecond), clock())
}
