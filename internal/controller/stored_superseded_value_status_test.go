// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// This is a claim about the APISERVER rather than about our code, so it is pinned by execution
// rather than by reading: a status update onto an object whose STORED spec no longer validates
// against its own CRD is ACCEPTED.
//
// It is the precondition for the "loud rejection" pattern — keep a superseded field in the schema
// and narrow it so re-applying a stored value FAILS — because that pattern only works if the one
// object that most needs to explain why it was refused can still carry the explanation. If the
// apiserver re-validated the whole object on a status-subresource update, that write would be
// rejected 422 and the refusal would be unreportable.
//
// No field in this API uses the pattern today: ClusterWatchRule.spec.rules[].scope was the last
// one and 0.43.0 removed it, so every superseded field is now deleted outright with
// docs/UPGRADING.md carrying the migration. The property is measured anyway, because it is what
// makes the pattern available the next time a field has to go, and the two strategies are priced
// against each other in docs/facts/crd-upgrade-strategies.md. The narrowing below is therefore
// synthetic: it edits the SERVED schema of a field that does exist, which is exactly the shape a
// real narrowing would take.
//
// The worry was that CRD Validation Ratcheting (beta and default-on from 1.30, GA in 1.33) was
// doing the work, and would therefore vanish on an older cluster or with the gate off. It is not:
// the status subresource does not re-validate spec at all. Measured on 1.31 with the gate
// explicitly on AND explicitly off, and on the version this module builds against — same answer
// every time. (On 1.33+ the gate cannot be turned off: kube-apiserver refuses to start on
// `CRDValidationRatcheting=false`, which is why this test does not try.)
func TestStoredSupersededValue_StatusUpdateIsAccepted(t *testing.T) {
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
	if err := configbutleraiv1alpha3.AddToScheme(sch); err != nil {
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

	// Store an object under the schema as it ships. This is how an object written by an EARLIER
	// release looks before the field it carries is narrowed under it.
	stored := &configbutleraiv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "stored-superseded-value"},
		Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: "t", Namespace: "default"},
			Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
				Resources: []string{"configmaps"},
			}},
		},
	}
	createEventually(ctx, t, c, stored)

	// Narrow the served schema so the stored value is no longer admissible. The object in etcd now
	// no longer validates against its own CRD.
	setResourcesEnum(ctx, t, c, "nodes")
	requireCreateRejected(ctx, t, c)

	var refused configbutleraiv1alpha3.ClusterWatchRule
	if err := c.Get(ctx, client.ObjectKey{Name: stored.Name}, &refused); err != nil {
		t.Fatalf("get stored object: %v", err)
	}
	if got := refused.Spec.Rules[0].Resources[0]; got != "configmaps" {
		t.Fatalf("stored spec value did not survive the narrowing: resources[0] = %q, want %q",
			got, "configmaps")
	}

	refused.Status.ObservedGeneration = refused.Generation
	refused.Status.Conditions = []metav1.Condition{{
		Type:               "Stalled",
		Status:             metav1.ConditionTrue,
		Reason:             "ResourceNotSupported",
		Message:            "spec.rules[].resources: configmaps is no longer supported",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: refused.Generation,
	}}
	if err := c.Status().Update(ctx, &refused); err != nil {
		t.Fatalf("status update on an object whose STORED spec no longer validates was rejected: %v\n"+
			"The loud-rejection pattern is unsafe, and a future field removal cannot rely on it "+
			"(docs/facts/crd-upgrade-strategies.md).", err)
	}
}

// setResourcesEnum narrows the served schema for ClusterWatchRule spec.rules[].resources to the
// given values, standing in for a real field narrowing.
func setResourcesEnum(ctx context.Context, t *testing.T, c client.Client, values ...string) {
	t.Helper()
	var crd apiextv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: "clusterwatchrules.configbutler.ai"}, &crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	items := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["rules"].Items.Schema
	resources := items.Properties["resources"]
	resources.Items.Schema.Enum = nil
	for _, v := range values {
		resources.Items.Schema.Enum = append(resources.Items.Schema.Enum, apiextv1.JSON{Raw: fmt.Appendf(nil, "%q", v)})
	}
	items.Properties["resources"] = resources
	if err := c.Update(ctx, &crd); err != nil {
		t.Fatalf("update CRD schema: %v", err)
	}
}

// createEventually retries a create until the apiserver has picked up the schema change. A CRD
// schema update is not visible to the CR handler synchronously, so a single attempt is a flake.
func createEventually(ctx context.Context, t *testing.T, c client.Client, obj client.Object) {
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
	t.Fatalf("create never succeeded under the shipped schema: %v", err)
}

// requireCreateRejected blocks until the NARROWED schema is the one being served, proven by a
// create that must fail. Waiting on the rejection rather than on a sleep is what makes the
// status-update assertion below meaningful: without it the test could pass against the old schema.
func requireCreateRejected(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		probe := &configbutleraiv1alpha3.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("narrowing-probe-%d", i)},
			Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
				GitTargetRef: meta.NamespacedObjectReference{Name: "t", Namespace: "default"},
				Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
					Resources: []string{"configmaps"},
				}},
			},
		}
		if err := c.Create(ctx, probe); err != nil {
			return
		}
		_ = c.Delete(ctx, probe)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the narrowed enum never took effect, so the status-update assertion would be vacuous")
}
