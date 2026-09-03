// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestRenamedRequiredField_StoredObjectCanAdoptIt is the measurement behind the guard on
// GitTarget's gitProviderRef immutability rule, and the reason renaming a required reference did
// not need a two-release migration.
//
// The hazard is real and this test reproduces it: a GitTarget stored before the rename serves NO
// value for the new name and no value for the old one either — a field outside the structural
// schema is not served, so the loss is immediate on the CRD upgrade rather than on the next write.
// The object is therefore missing a REQUIRED field, and the apply that fixes it is also the apply
// that sets an IMMUTABLE one. Written as a plain `self.x == oldSelf.x`, that rule rejects the
// migrating apply outright, because oldSelf has no such key: the user's only route would be
// delete-and-recreate.
//
// Guarding it as `!has(oldSelf.x) || self.x == oldSelf.x` opens a one-way door. It cannot loosen
// anything, because a required field can never be absent on an object created from this release
// on — which the last assertion here holds to.
//
// The schema surgery below stands in for the release: widen the served schema so an object can be
// stored without the field, then narrow it back to what ships.
func TestRenamedRequiredField_StoredObjectCanAdoptIt(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest control plane: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := apiextv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	target := func(name string, withRef bool) *unstructured.Unstructured {
		spec := map[string]any{"branch": "main", "path": "clusters/prod"}
		if withRef {
			spec["gitProviderRef"] = map[string]any{"name": "platform"}
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitTarget",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	// Stand in for the PRE-rename release: the schema serves `providerRef` and does not know
	// `gitProviderRef` at all. Storing under this schema is what makes the assertions below mean
	// something — an object created without the new field under the SHIPPED schema would prove only
	// that the guard works, not that the loss it exists for is real.
	renameGitProviderRefTo(ctx, t, c, "providerRef")
	legacy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "GitTarget",
		"metadata":   map[string]any{"name": "stored-before-the-rename", "namespace": "default"},
		"spec": map[string]any{
			"providerRef": map[string]any{"name": "platform"},
			"branch":      "main",
			"path":        "clusters/prod",
		},
	}}
	createUntilServed(ctx, t, c, legacy)

	// Ship the rename.
	renameGitProviderRefTo(ctx, t, c, "gitProviderRef")
	requireRefLessCreateRejected(ctx, t, c, target)

	// FIRST CLAIM: the old value is gone on READ, immediately, with no write in between. This is
	// what makes the rename a stall rather than a silent mis-configuration, and it is the premise
	// docs/UPGRADING.md's "re-apply every object" step rests on.
	afterRename := requirePrunedOfBothSpellings(ctx, t, c, legacy.GetName())

	// SECOND CLAIM: the migrating apply works. One update sets a required, immutable field for the
	// first time, which is the shape the guard exists for.
	if err := unstructured.SetNestedMap(
		afterRename.Object, map[string]any{"name": "platform"}, "spec", "gitProviderRef"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, afterRename); err != nil {
		t.Fatalf("a stored object could not adopt the renamed required field in one apply: %v\n"+
			"Without this, upgrading across the rename would force delete-and-recreate on every "+
			"GitTarget. See the guard on spec.gitProviderRef in api/v1alpha3/gittarget_types.go.", err)
	}

	// And the door is shut for everything created after the rename.
	fresh := target("created-after-the-rename", true)
	if err := c.Create(ctx, fresh); err != nil {
		t.Fatalf("create under the shipped schema: %v", err)
	}
	if err := unstructured.SetNestedMap(
		fresh.Object, map[string]any{"name": "somewhere-else"}, "spec", "gitProviderRef"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, fresh); err == nil {
		t.Fatal("gitProviderRef is no longer immutable: the migration guard has leaked into " +
			"objects that were created with the field")
	}
}

// renameGitProviderRefTo rewrites the SERVED GitTarget schema so the provider reference is spelled
// `to` — property, required list and all — standing in for the release boundary the rename crosses.
// Crossing it in the test the way the release crosses it is the whole point: a field the schema no
// longer describes is a field the apiserver no longer serves.
func renameGitProviderRefTo(ctx context.Context, t *testing.T, c client.Client, to string) {
	t.Helper()
	const (
		oldName = "providerRef"
		newName = "gitProviderRef"
	)
	from := newName
	if to == newName {
		from = oldName
	}

	var crd apiextv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: "gittargets.configbutler.ai"}, &crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if prop, ok := spec.Properties[from]; ok {
		spec.Properties[to] = prop
		delete(spec.Properties, from)
	}
	required := make([]string, 0, len(spec.Required))
	for _, r := range spec.Required {
		if r == from {
			r = to
		}
		required = append(required, r)
	}
	spec.Required = required
	// The rules move with the field. A CEL rule is compiled against the schema, so leaving one
	// naming a property that no longer exists makes the CRD itself invalid — which is a useful
	// thing to have learned: the immutability rule and the field it guards cannot drift apart.
	for i := range spec.XValidations {
		spec.XValidations[i].Rule = strings.ReplaceAll(spec.XValidations[i].Rule, from, to)
		spec.XValidations[i].Message = strings.ReplaceAll(spec.XValidations[i].Message, from, to)
	}
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	if err := c.Update(ctx, &crd); err != nil {
		t.Fatalf("update CRD schema: %v", err)
	}
}

// createUntilServed retries a create until the apiserver is serving the schema change. A CRD schema
// update is not visible to the CR handler synchronously, so a single attempt is a flake.
func createUntilServed(ctx context.Context, t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = c.Create(ctx, obj); err == nil {
			return
		}
		obj.SetResourceVersion("")
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("create never succeeded under the widened schema: %v", err)
}

// requireRefLessCreateRejected blocks until the NARROWED schema is the one being served, proven by
// a create that must fail. Waiting on the rejection rather than on a sleep is what stops the
// migration assertion passing vacuously against the widened schema.
func requireRefLessCreateRejected(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	target func(string, bool) *unstructured.Unstructured,
) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		probe := target(fmt.Sprintf("narrowing-probe-%d", i), false)
		if err := c.Create(ctx, probe); err != nil {
			return
		}
		_ = c.Delete(ctx, probe)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("gitProviderRef never became required, so the migration assertion would be vacuous")
}

// requirePrunedOfBothSpellings reads the stored object back and holds the premise of the migration:
// after the rename it serves NEITHER the old name (the schema no longer describes it) nor the new
// one (nothing has written it). It returns the object so the caller can attempt the migration.
func requirePrunedOfBothSpellings(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	name string,
) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("configbutler.ai/v1alpha3")
	obj.SetKind("GitTarget")
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, obj); err != nil {
		t.Fatalf("get stored object after the rename: %v", err)
	}
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec["providerRef"]; ok {
		t.Fatalf("the pruned field is still served, so this test is not reproducing the migration: %v", spec)
	}
	if _, ok := spec["gitProviderRef"]; ok {
		t.Fatalf("the stored object gained the new field without being written: %v", spec)
	}
	return obj
}
