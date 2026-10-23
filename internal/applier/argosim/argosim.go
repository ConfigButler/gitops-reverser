// SPDX-License-Identifier: Apache-2.0

/*
Package argosim is the API server's field ownership of an Argo CD Application's automated sync
switch, spec.syncPolicy.automated.enabled, for tests of code that holds it. The fake dynamic client
keeps no managedFields, and the ArgoCDClaim's hold is decided by them.
*/
package argosim

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ApplicationGVR is Argo CD's Application.
func ApplicationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
}

// switchPath is the path of the automated sync switch.
func switchPath() []string { return []string{"spec", "syncPolicy", "automated", "enabled"} }

// Serve makes client serve the patches of an Application as the API server does for the switch,
// as measured in envtest (internal/controller/argocdclaim_apiserver_test.go):
//
//   - A write that changes the switch's value takes it from every other owner.
//   - An update (a merge patch) that writes the value the switch holds takes nothing.
//   - A server-side apply of the value it holds shares it with its owners.
//   - A server-side apply that leaves out a switch its manager owned gives it up, and removes it
//     when nobody else owns it, with the syncPolicy maps a write of the switch created: a map
//     somebody else wrote stays, even empty.
//   - A merge patch that sets it to null removes it, as kubectl's client-side apply does for a field
//     its last applied manifest had.
//
// Every other field of a merge patch is merged as a merge patch merges it. A server-side apply
// carries only the switch here: nothing else applies an Application in these tests. A dry run
// (DryRun: All) answers with the Application the write would leave, and stores nothing.
func Serve(client *dynamicfake.FakeDynamicClient, opts ...Option) {
	srv := &server{client: client, created: map[string]map[int]bool{}}
	for _, opt := range opts {
		opt(&srv.config)
	}
	client.PrependReactor("patch", "applications", srv.patch)
}

// server serves the patches of Applications.
type server struct {
	client *dynamicfake.FakeDynamicClient
	config config
	// writes makes each read-modify-write one step, as the API server's resourceVersion check does.
	writes sync.Mutex
	// created is the depths of switchPath whose maps a write of the switch created, by Application.
	created map[string]map[int]bool
}

// patch serves one patch of an Application.
func (s *server) patch(action k8stesting.Action) (bool, runtime.Object, error) {
	patch, ok := action.(k8stesting.PatchActionImpl)
	if !ok {
		return false, nil, nil
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	current, err := s.client.Tracker().Get(ApplicationGVR(), patch.GetNamespace(), patch.GetName())
	if err != nil {
		return true, nil, err
	}
	app, ok := current.(*unstructured.Unstructured)
	if !ok {
		return true, nil, fmt.Errorf("an Application the tracker holds as %T", current)
	}
	app = app.DeepCopy()
	body, writer, err := s.write(app, patch)
	if err != nil {
		return true, nil, err
	}
	key := patch.GetNamespace() + "/" + patch.GetName()
	made := maps.Clone(s.created[key])
	if made == nil {
		made = map[int]bool{}
	}
	writeSwitch(app, body, writer, made)
	if len(patch.PatchOptions.DryRun) > 0 {
		return true, app, nil
	}
	s.created[key] = made
	if err := s.client.Tracker().Update(ApplicationGVR(), app, app.GetNamespace()); err != nil {
		return true, nil, err
	}
	return true, app, nil
}

// write reads a patch's writer, merges into app everything of a merge patch but the switch, and
// returns the body that leaves for the switch: the API server's typed patch, before ownership.
func (s *server) write(
	app *unstructured.Unstructured,
	patch k8stesting.PatchActionImpl,
) (map[string]any, owner, error) {
	var body map[string]any
	if err := json.Unmarshal(patch.Patch, &body); err != nil {
		return nil, owner{}, apierrors.NewBadRequest(err.Error())
	}
	manager := patch.PatchOptions.FieldManager
	if manager == "" {
		manager = "unnamed"
	}
	writer := owner{manager: manager, operation: metav1.ManagedFieldsOperationUpdate}
	if patch.PatchType == types.ApplyPatchType {
		writer.operation = metav1.ManagedFieldsOperationApply
	} else {
		value, present, _ := unstructured.NestedFieldNoCopy(body, switchPath()...)
		unstructured.RemoveNestedField(body, switchPath()...)
		mergePatch(app.Object, body)
		body = map[string]any{}
		if present {
			_ = unstructured.SetNestedField(body, value, switchPath()...)
		}
	}
	if !s.config.withoutSwitch {
		return body, writer, nil
	}
	if writer.operation == metav1.ManagedFieldsOperationApply && hasSwitch(body) {
		return nil, owner{}, apierrors.NewInternalError(fmt.Errorf("failed to create typed patch object "+
			"(%s/%s; argoproj.io/v1alpha1, Kind=Application): .spec.syncPolicy.automated.enabled: field not "+
			"declared in schema", patch.GetNamespace(), patch.GetName()))
	}
	if pruneSwitch(body) {
		parent := switchPath()[:len(switchPath())-1]
		if _, found, _ := unstructured.NestedMap(app.Object, parent...); !found {
			_ = unstructured.SetNestedField(app.Object, map[string]any{}, parent...)
		}
	}
	return body, writer, nil
}

// hasSwitch reports whether a write's body carries the switch.
func hasSwitch(body map[string]any) bool {
	_, present, _ := unstructured.NestedFieldNoCopy(body, switchPath()...)
	return present
}

// Option changes what Serve serves.
type Option func(*config)

type config struct {
	withoutSwitch bool
}

// WithoutSwitch serves an Argo CD before 3.1, whose Application has no
// spec.syncPolicy.automated.enabled in its schema. The API server refuses a server-side apply of it,
// with a 500 that says "field not declared in schema", and prunes it from a merge patch, keeping the
// maps around it: a merge patch of it to an Application without an automated block leaves
// automated: {}, which is automated sync on.
func WithoutSwitch() Option {
	return func(c *config) { c.withoutSwitch = true }
}

// pruneSwitch removes the switch from a write's body, as the API server prunes a field its schema
// does not know, and reports whether there was one. The maps around it stay, which Serve writes.
func pruneSwitch(body map[string]any) bool {
	if !hasSwitch(body) {
		return false
	}
	unstructured.RemoveNestedField(body, switchPath()...)
	return true
}

// owner is a managedFields entry's identity.
type owner struct {
	manager   string
	operation metav1.ManagedFieldsOperationType
}

// writeSwitch writes the switch of body, by writer, to app and its managedFields. made is the
// depths of switchPath whose maps a write of the switch created, which it keeps up to date.
func writeSwitch(app *unstructured.Unstructured, body map[string]any, writer owner, made map[int]bool) {
	owners := switchOwners(app)
	value, present, _ := unstructured.NestedFieldNoCopy(body, switchPath()...)
	switch {
	case present:
		owners = writeValue(app, owners, value, writer, made)
	case writer.operation == metav1.ManagedFieldsOperationApply && slices.Contains(owners, writer):
		owners = slices.DeleteFunc(owners, func(o owner) bool { return o == writer })
		if len(owners) == 0 {
			removeSwitch(app, made)
		}
	}
	setSwitchOwners(app, owners)
}

// writeValue writes value to the switch, by writer, and returns its owners after.
func writeValue(app *unstructured.Unstructured, owners []owner, value any, writer owner, made map[int]bool) []owner {
	old, held, _ := unstructured.NestedFieldNoCopy(app.Object, switchPath()...)
	unchanged := held && fmt.Sprint(old) == fmt.Sprint(value)
	switch {
	case value == nil:
		removeSwitch(app, made)
		return nil
	case unchanged && writer.operation == metav1.ManagedFieldsOperationUpdate:
		// An update owns only what it changed.
		return owners
	case unchanged:
		if !slices.Contains(owners, writer) {
			owners = append(owners, writer)
		}
		return owners
	}
	for depth := 2; depth < len(switchPath()); depth++ {
		if _, found, _ := unstructured.NestedFieldNoCopy(app.Object, switchPath()[:depth]...); !found {
			made[depth] = true
		}
	}
	_ = unstructured.SetNestedField(app.Object, value, switchPath()...)
	return []owner{writer}
}

// removeSwitch removes the switch, and the syncPolicy maps a write of it created that it leaves
// empty.
func removeSwitch(app *unstructured.Unstructured, made map[int]bool) {
	unstructured.RemoveNestedField(app.Object, switchPath()...)
	for depth := len(switchPath()) - 1; depth > 1; depth-- {
		parent, found, _ := unstructured.NestedMap(app.Object, switchPath()[:depth]...)
		if found && len(parent) == 0 && made[depth] {
			unstructured.RemoveNestedField(app.Object, switchPath()[:depth]...)
		}
		delete(made, depth)
	}
}

// switchOwners reads which managedFields entries own the switch.
func switchOwners(app *unstructured.Unstructured) []owner {
	var owners []owner
	for _, entry := range app.GetManagedFields() {
		if ownsSwitch(entry) {
			owners = append(owners, owner{manager: entry.Manager, operation: entry.Operation})
		}
	}
	return owners
}

// setSwitchOwners keeps the entries that own anything but the switch, and writes one entry per
// owner of the switch.
func setSwitchOwners(app *unstructured.Unstructured, owners []owner) {
	var entries []metav1.ManagedFieldsEntry
	for _, entry := range app.GetManagedFields() {
		if !ownsSwitch(entry) {
			entries = append(entries, entry)
		}
	}
	for _, owner := range owners {
		entries = append(entries, SwitchEntry(owner.manager, owner.operation))
	}
	app.SetManagedFields(entries)
}

// ownsSwitch reports whether a managedFields entry owns the switch.
func ownsSwitch(entry metav1.ManagedFieldsEntry) bool {
	if entry.FieldsV1 == nil {
		return false
	}
	var fields map[string]any
	if json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields) != nil {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(fields, "f:spec", "f:syncPolicy", "f:automated", "f:enabled")
	return found
}

// SwitchEntry is the managedFields entry of manager owning the switch, through a write of operation.
func SwitchEntry(manager string, operation metav1.ManagedFieldsOperationType) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: operation, APIVersion: "argoproj.io/v1alpha1", FieldsType: "FieldsV1",
		FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:syncPolicy":{"f:automated":{"f:enabled":{}}}}}`),
	}
}

// OwnSwitch sets an Application's switch to enabled, owned by manager through a write of operation,
// as a manifest applied by something else, or a click in Argo CD's UI, leaves it.
func OwnSwitch(t testing.TB, app *unstructured.Unstructured, enabled bool, manager string,
	operation metav1.ManagedFieldsOperationType) *unstructured.Unstructured {
	t.Helper()
	require.NoError(t, unstructured.SetNestedField(app.Object, enabled, switchPath()...))
	setSwitchOwners(app, []owner{{manager: manager, operation: operation}})
	return app
}

// mergePatch merges patch into obj as a JSON merge patch (RFC 7386) does.
func mergePatch(obj, patch map[string]any) {
	for key, value := range patch {
		if value == nil {
			delete(obj, key)
			continue
		}
		into, isMap := obj[key].(map[string]any)
		from, patchesMap := value.(map[string]any)
		if isMap && patchesMap {
			mergePatch(into, from)
			continue
		}
		if patchesMap {
			fresh := map[string]any{}
			mergePatch(fresh, from)
			value = fresh
		}
		obj[key] = value
	}
}
