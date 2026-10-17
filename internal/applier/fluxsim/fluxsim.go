// SPDX-License-Identifier: Apache-2.0

/*
Package fluxsim is a fake source cluster running kustomize-controller and source-controller, for
tests of code that drives Flux. It applies the source facts docs/design/suspended-appliers-plan.md
lists (kustomize-controller v1.9.6), and nothing more.
*/
package fluxsim

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Sim is a source cluster running kustomize-controller and source-controller, as far as the
// source facts in docs/design/suspended-appliers-plan.md go (kustomize-controller v1.9.6,
// internal/controller/kustomization_controller.go). It is what the Flux client's lifecycle is tested
// against, so each fact it relies on is one this fake applies:
//
//   - A reconcile reads the object once, when it starts. finalizeStatus is deferred and copies the
//     requestedAt it read to status.lastHandledReconcileAt on every return (:143, :1335-1340).
//   - A suspended object returns right after that read, before the source is resolved (:189-192):
//     it fetches, builds and applies nothing, and its Ready is left as it was.
//   - spec.suspend is read only when a reconcile starts, so a suspend does not stop one running.
//   - lastAttemptedRevision is written before the build (:390); lastAppliedRevision and Ready=True
//     once the apply finished. A failed apply is Ready=False; a dependency not ready is Ready=False,
//     DependencyNotReady (:264-268).
//   - observedGeneration is set from the generation read whenever Ready is True at the end (:1345).
//   - A reconcile is enqueued by a new generation, a new requestedAt, or a new artifact of its
//     source (kustomization_manager.go:139-173), and reconciles of one object never overlap.
//
// Writes go through the fake dynamic client the client under test uses. spec.suspend and the
// requestedAt annotation keep managedFields, by manager and operation, as the API server does for a
// server-side apply (forced) and for a merge patch.
type Sim struct {
	t testing.TB
	// Client is the fake source cluster's client, which the code under test writes through.
	Client *dynamicfake.FakeDynamicClient

	mu sync.Mutex
	// remote is the revision a GitRepository's fetch finds, by GitRepository.
	remote map[types.NamespacedName]string
	// seen is what each object's last reconcile started from, by object, to tell whether another
	// is due.
	seen map[string]seenState
	// failing is the Ready=False message of every apply while set; dependencyNotReady fails every
	// apply with DependencyNotReady.
	failing            string
	dependencyNotReady bool
	// applied lists the revisions applied, in order, as "namespace/name revision".
	applied []string
	// hold, while armed, stops the next apply after lastAttemptedRevision until it is closed; holding
	// is closed once that apply is stopped there.
	hold    chan struct{}
	holding chan struct{}
	armed   bool
	// started counts reconciles started, by object.
	started map[string]int
}

// seenState is what one reconcile of an object started from.
type seenState struct {
	generation int64
	token      string
	artifact   string
}

// Field paths the fake keeps managedFields for.
const (
	simFieldSuspend     = "f:spec.f:suspend"
	simFieldRequestedAt = "f:metadata.f:annotations.f:" + fluxmeta.ReconcileRequestAnnotation
)

// New wraps a fake source cluster's client and starts its controllers, which stop with the
// test.
func New(t testing.TB, client *dynamicfake.FakeDynamicClient) *Sim {
	t.Helper()
	sim := &Sim{
		t: t, Client: client,
		remote:  map[types.NamespacedName]string{},
		seen:    map[string]seenState{},
		started: map[string]int{},
	}
	client.PrependReactor("patch", "kustomizations", sim.patchKustomization)
	// What the objects hold now is what their last reconcile started from: a test starts from a
	// cluster at rest, not from the controllers' first pass over it.
	for _, ks := range sim.list(KustomizationGVR()) {
		sim.seen[types.NamespacedName{Namespace: ks.GetNamespace(), Name: ks.GetName()}.String()] = sim.seenNow(ks)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sim.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		sim.Release()
		<-done
	})
	return sim
}

// Fetches sets what the next fetch of a GitRepository finds.
func (s *Sim) Fetches(source types.NamespacedName, revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remote[source] = revision
}

// FailApplies makes every apply fail with message, or succeed again when it is empty.
func (s *Sim) FailApplies(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = message
}

// BlockDependencies makes every apply wait on a dependency that is not ready.
func (s *Sim) BlockDependencies(blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dependencyNotReady = blocked
}

// HoldNextApply stops the next apply after it wrote lastAttemptedRevision, and returns a channel
// that is closed once it is held there. Release lets it finish.
func (s *Sim) HoldNextApply() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.holding, s.armed = make(chan struct{}), make(chan struct{}), true
	return s.holding
}

// Release lets a held apply finish.
func (s *Sim) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold = nil
	}
}

// Applied is every apply so far, as "namespace/name revision".
func (s *Sim) Applied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

// run is the two controllers: it reconciles whatever is due, one reconcile per object at a time.
func (s *Sim) run(ctx context.Context) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, source := range s.list(GitRepositoryGVR()) {
			s.reconcileSource(ctx, source)
		}
		for _, ks := range s.list(KustomizationGVR()) {
			s.reconcileKustomization(ctx, ks)
		}
	}
}

func (s *Sim) list(gvr schema.GroupVersionResource) []*unstructured.Unstructured {
	list, err := s.Client.Tracker().List(gvr, gvr.GroupVersion().WithKind(itemKind(gvr)), "")
	if err != nil {
		return nil
	}
	ulist, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return nil
	}
	out := make([]*unstructured.Unstructured, 0, len(ulist.Items))
	for i := range ulist.Items {
		out = append(out, &ulist.Items[i])
	}
	return out
}

// itemKind is the kind of the objects a resource lists, which the tracker names its list after.
func itemKind(gvr schema.GroupVersionResource) string {
	if gvr == GitRepositoryGVR() {
		return kindGitRepository
	}
	return kindKustomization
}

// reconcileSource fetches when a GitRepository's requestedAt is new: its artifact becomes what the
// remote holds, and the request is marked handled.
func (s *Sim) reconcileSource(ctx context.Context, source *unstructured.Unstructured) {
	token := source.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]
	if token == "" || token == statusString(source, "lastHandledReconcileAt") {
		return
	}
	key := types.NamespacedName{Namespace: source.GetNamespace(), Name: source.GetName()}
	s.mu.Lock()
	revision := s.remote[key]
	s.mu.Unlock()
	s.WriteStatus(ctx, GitRepositoryGVR(), key, func(obj *unstructured.Unstructured) {
		if revision != "" {
			_ = unstructured.SetNestedField(obj.Object, revision, "status", "artifact", "revision")
		}
		_ = unstructured.SetNestedField(obj.Object, token, "status", "lastHandledReconcileAt")
	})
}

// seenNow is what a reconcile of ks starting now would start from.
func (s *Sim) seenNow(ks *unstructured.Unstructured) seenState {
	now := seenState{generation: ks.GetGeneration(), token: ks.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]}
	if sourceRef, err := gitRepositoryRef(ks); err == nil {
		if source, err := s.Client.Tracker().Get(GitRepositoryGVR(), sourceRef.Namespace, sourceRef.Name); err == nil {
			if source, ok := source.(*unstructured.Unstructured); ok {
				now.artifact = statusString(source, "artifact", "revision")
			}
		}
	}
	return now
}

// Requeue makes a reconcile of the Kustomization due with nothing changed, as a requeue after
// retryInterval or interval does.
func (s *Sim) Requeue(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key.String())
}

// Reconciles is how many reconciles of the Kustomization started.
func (s *Sim) Reconciles(key types.NamespacedName) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started[key.String()]
}

// reconcileKustomization runs one reconcile of a Kustomization when one is due.
func (s *Sim) reconcileKustomization(ctx context.Context, ks *unstructured.Unstructured) {
	key := types.NamespacedName{Namespace: ks.GetNamespace(), Name: ks.GetName()}
	if _, err := gitRepositoryRef(ks); err != nil {
		return
	}
	now := s.seenNow(ks)
	artifact := now.artifact
	s.mu.Lock()
	last, known := s.seen[key.String()]
	if known && last == now {
		s.mu.Unlock()
		return
	}
	s.seen[key.String()] = now
	s.started[key.String()]++
	failing, blocked := s.failing, s.dependencyNotReady
	s.mu.Unlock()

	suspended, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	finalize := func(change func(*unstructured.Unstructured)) {
		s.WriteStatus(ctx, KustomizationGVR(), key, func(obj *unstructured.Unstructured) {
			if change != nil {
				change(obj)
			}
			if now.token != "" {
				_ = unstructured.SetNestedField(obj.Object, now.token, "status", "lastHandledReconcileAt")
			}
			if readyStatus(obj) == string(metav1.ConditionTrue) {
				_ = unstructured.SetNestedField(obj.Object, now.generation, "status", "observedGeneration")
			}
		})
	}
	switch {
	case suspended:
		finalize(nil)
		return
	case blocked:
		finalize(func(obj *unstructured.Unstructured) {
			SetReady(
				obj,
				metav1.ConditionFalse,
				fluxmeta.DependencyNotReadyReason,
				"dependency 'apps/infra' is not ready",
			)
		})
		return
	}

	s.WriteStatus(ctx, KustomizationGVR(), key, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, artifact, "status", "lastAttemptedRevision")
	})
	s.mu.Lock()
	hold, holding, armed := s.hold, s.holding, s.armed
	s.armed = false
	s.mu.Unlock()
	if armed {
		close(holding)
		select {
		case <-hold:
		case <-ctx.Done():
			return
		}
	}
	if failing != "" {
		finalize(func(obj *unstructured.Unstructured) {
			SetReady(obj, metav1.ConditionFalse, "ReconciliationFailed", failing)
		})
		return
	}
	s.mu.Lock()
	s.applied = append(s.applied, key.String()+" "+artifact)
	s.mu.Unlock()
	finalize(func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, artifact, "status", "lastAppliedRevision")
		SetReady(obj, metav1.ConditionTrue, fluxmeta.ReconciliationSucceededReason, "Applied revision: "+artifact)
	})
}

// WriteStatus changes an object's status as the controller's patcher does: on the object as it is
// now, leaving its spec and metadata alone.
func (s *Sim) WriteStatus(_ context.Context, gvr schema.GroupVersionResource, key types.NamespacedName,
	change func(*unstructured.Unstructured)) {
	current, err := s.Client.Tracker().Get(gvr, key.Namespace, key.Name)
	if err != nil {
		return
	}
	held, ok := current.(*unstructured.Unstructured)
	if !ok {
		return
	}
	obj := held.DeepCopy()
	change(obj)
	_ = s.Client.Tracker().Update(gvr, obj, key.Namespace)
}

// readyStatus is a Kustomization's Ready status, or "".
func readyStatus(obj *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		cond, _ := c.(map[string]any)
		if cond["type"] == fluxmeta.ReadyCondition {
			status, _ := cond["status"].(string)
			return status
		}
	}
	return ""
}

// SetReady sets a Kustomization's Ready condition.
func SetReady(obj *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) {
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
		"type": fluxmeta.ReadyCondition, "status": string(status), "reason": reason, "message": message,
	}}, "status", "conditions")
}

// patchKustomization serves a patch of a Kustomization: a server-side apply (always forced, as the
// client under test applies) or a merge patch, keeping managedFields for spec.suspend and the
// requestedAt annotation, and the generation for a spec change.
func (s *Sim) patchKustomization(action k8stesting.Action) (bool, runtime.Object, error) {
	patch, ok := action.(k8stesting.PatchActionImpl)
	if !ok {
		return false, nil, nil
	}
	current, err := s.Client.Tracker().Get(KustomizationGVR(), patch.GetNamespace(), patch.GetName())
	if err != nil {
		return true, nil, err
	}
	obj, ok := current.(*unstructured.Unstructured)
	if !ok {
		return true, nil, fmt.Errorf("a Kustomization the tracker holds as %T", current)
	}
	obj = obj.DeepCopy()
	var body map[string]any
	if err := json.Unmarshal(patch.Patch, &body); err != nil {
		return true, nil, err
	}
	manager := patch.PatchOptions.FieldManager
	if manager == "" {
		manager = "unnamed"
	}
	writer := owner{manager: manager, operation: metav1.ManagedFieldsOperationUpdate}
	if patch.PatchType == types.ApplyPatchType {
		writer.operation = metav1.ManagedFieldsOperationApply
	}
	byField := owners(obj)
	wasSuspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend")
	for _, field := range keptFields() {
		value, present, _ := unstructured.NestedFieldNoCopy(body, field.path...)
		switch {
		case present:
			setField(obj, field.path, value)
			byField[field.name] = writer
		case writer.operation == metav1.ManagedFieldsOperationApply && byField[field.name] == writer:
			// An apply that leaves out a field its manager owned removes it.
			setField(obj, field.path, nil)
			delete(byField, field.name)
		}
	}
	if isSuspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend"); isSuspended != wasSuspended {
		obj.SetGeneration(obj.GetGeneration() + 1)
	}
	setOwners(obj, byField)
	if err := s.Client.Tracker().Update(KustomizationGVR(), obj, obj.GetNamespace()); err != nil {
		return true, nil, err
	}
	return true, obj, nil
}

// keptField is a field the fake keeps managedFields for.
type keptField struct {
	name string
	path []string
}

// keptFields are spec.suspend and the requestedAt annotation.
func keptFields() []keptField {
	return []keptField{
		{name: simFieldSuspend, path: []string{"spec", "suspend"}},
		{name: simFieldRequestedAt, path: []string{"metadata", "annotations", fluxmeta.ReconcileRequestAnnotation}},
	}
}

// setField sets the field at path, or removes it when value is nil.
func setField(obj *unstructured.Unstructured, path []string, value any) {
	if value == nil {
		unstructured.RemoveNestedField(obj.Object, path...)
		return
	}
	if path[0] == "metadata" {
		value = fmt.Sprint(value)
	}
	_ = unstructured.SetNestedField(obj.Object, value, path...)
}

// owner is a managedFields entry's identity.
type owner struct {
	manager   string
	operation metav1.ManagedFieldsOperationType
}

// owners reads which entry owns each of the fields the fake keeps.
func owners(obj *unstructured.Unstructured) map[string]owner {
	owners := map[string]owner{}
	for _, entry := range obj.GetManagedFields() {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields) != nil {
			continue
		}
		owner := owner{manager: entry.Manager, operation: entry.Operation}
		if _, ok, _ := unstructured.NestedFieldNoCopy(fields, "f:spec", "f:suspend"); ok {
			owners[simFieldSuspend] = owner
		}
		if _, ok, _ := unstructured.NestedFieldNoCopy(fields, "f:metadata", "f:annotations",
			"f:"+fluxmeta.ReconcileRequestAnnotation); ok {
			owners[simFieldRequestedAt] = owner
		}
	}
	return owners
}

// setOwners writes one managedFields entry per owner.
func setOwners(obj *unstructured.Unstructured, byField map[string]owner) {
	byOwner := map[owner]map[string]any{}
	for field, owner := range byField {
		fields := byOwner[owner]
		if fields == nil {
			fields = map[string]any{}
			byOwner[owner] = fields
		}
		if field == simFieldSuspend {
			_ = unstructured.SetNestedField(fields, map[string]any{}, "f:spec", "f:suspend")
		} else {
			_ = unstructured.SetNestedField(fields, map[string]any{}, "f:metadata", "f:annotations",
				"f:"+fluxmeta.ReconcileRequestAnnotation)
		}
	}
	entries := make([]metav1.ManagedFieldsEntry, 0, len(byOwner))
	for owner, fields := range byOwner {
		raw, _ := json.Marshal(fields)
		entries = append(entries, metav1.ManagedFieldsEntry{
			Manager: owner.manager, Operation: owner.operation, APIVersion: "kustomize.toolkit.fluxcd.io/v1",
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(string(raw)),
		})
	}
	obj.SetManagedFields(entries)
}

// OwnSuspend sets a Kustomization's spec.suspend, owned by manager with operation, as a manifest
// applied by something else, or a `flux suspend`, leaves it.
func OwnSuspend(t testing.TB, obj *unstructured.Unstructured, suspend bool, manager string,
	operation metav1.ManagedFieldsOperationType) *unstructured.Unstructured {
	t.Helper()
	require.NoError(t, unstructured.SetNestedField(obj.Object, suspend, "spec", "suspend"))
	owners := owners(obj)
	owners[simFieldSuspend] = owner{manager: manager, operation: operation}
	setOwners(obj, owners)
	return obj
}

// Kustomization reads a Kustomization from the fake.
func (s *Sim) Kustomization(key types.NamespacedName) *unstructured.Unstructured {
	s.t.Helper()
	obj, err := s.Client.Resource(KustomizationGVR()).Namespace(key.Namespace).
		Get(context.Background(), key.Name, metav1.GetOptions{})
	require.NoError(s.t, err)
	return obj
}

// TickingClock starts at start and moves on a millisecond on every read, so every token taken from
// it is new, as `flux reconcile`'s are.
func TickingClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		read := now
		now = now.Add(time.Millisecond)
		return read
	}
}

// KustomizationGVR is Flux's Kustomization.
func KustomizationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
}

// GitRepositoryGVR is Flux's GitRepository.
func GitRepositoryGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
}

const (
	kindKustomization = "Kustomization"
	kindGitRepository = "GitRepository"
)

// gitRepositoryRef is a Kustomization's GitRepository, its namespace defaulted to its own.
func gitRepositoryRef(ks *unstructured.Unstructured) (types.NamespacedName, error) {
	kind, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "kind")
	name, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "name")
	namespace, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "namespace")
	if kind != kindGitRepository || name == "" {
		return types.NamespacedName{}, fmt.Errorf("%s/%s applies no GitRepository", ks.GetNamespace(), ks.GetName())
	}
	if namespace == "" {
		namespace = ks.GetNamespace()
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

// statusString reads a string under an object's status.
func statusString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, append([]string{"status"}, fields...)...)
	return value
}
