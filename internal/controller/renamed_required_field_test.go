// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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

	// Stand in for the pre-rename release: gitProviderRef not yet required.
	setGitProviderRefRequired(ctx, t, c, false)
	legacy := target("stored-before-the-rename", false)
	createUntilServed(ctx, t, c, legacy)

	// Ship the rename: the field is required from here on.
	setGitProviderRefRequired(ctx, t, c, true)
	requireRefLessCreateRejected(ctx, t, c, target)

	// The migrating apply: one update that sets a required, immutable field for the first time.
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion("configbutler.ai/v1alpha3")
	stored.SetKind("GitTarget")
	if err := c.Get(ctx, client.ObjectKey{Name: legacy.GetName(), Namespace: "default"}, stored); err != nil {
		t.Fatalf("get stored object: %v", err)
	}
	if err := unstructured.SetNestedMap(
		stored.Object, map[string]any{"name": "platform"}, "spec", "gitProviderRef"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, stored); err != nil {
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

// setGitProviderRefRequired flips whether the SERVED GitTarget schema requires spec.gitProviderRef,
// standing in for the release boundary the rename crosses.
func setGitProviderRefRequired(ctx context.Context, t *testing.T, c client.Client, required bool) {
	t.Helper()
	var crd apiextv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: "gittargets.configbutler.ai"}, &crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	kept := make([]string, 0, len(spec.Required)+1)
	for _, r := range spec.Required {
		if r != "gitProviderRef" {
			kept = append(kept, r)
		}
	}
	if required {
		kept = append(kept, "gitProviderRef")
	}
	spec.Required = kept
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
