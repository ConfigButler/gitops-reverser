// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// This is the gate the whole loud-rejection pattern rests on, and it is a claim about the
// APISERVER rather than about our code — so it is pinned by execution rather than by reading.
//
// The pattern keeps a superseded field in the schema and narrows it so that re-applying a stored
// value FAILS (ClusterWatchRule.spec.rules[].scope: Namespaced set the precedent; GitTarget's
// allowedSourceNamespaces and GitProvider's push/commit.message now follow it). It only works if
// the controller can still write a status update onto an object whose STORED spec no longer
// validates: the one object that most needs to explain why it was refused is the object carrying
// the refused value. If the apiserver re-validated the whole object on a status-subresource
// update, that write would be rejected 422 and the refusal would be unreportable.
//
// The worry was that CRD Validation Ratcheting (beta and default-on from 1.30, GA in 1.33) was
// doing the work, and would therefore vanish on an older cluster or with the gate off. It is not:
// the status subresource does not re-validate spec at all. Measured on 1.31 with the gate
// explicitly on AND explicitly off, and on the version this module builds against — same answer
// every time. (On 1.33+ the gate cannot be turned off: kube-apiserver refuses to start on
// `CRDValidationRatcheting=false`, which is why this test does not try.)
//
// If this test ever fails, the fallback is in docs/design/gittarget-api-wave.md: widen the enum
// back and rely on the compile-path refusal plus a loud Stalled condition.
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

	// Widen the enum so a "stored" object carrying the superseded value can exist at all. This is
	// how an object written by an EARLIER release looks to the current schema.
	setScopeEnum(ctx, t, c, "Cluster", "Namespaced")
	stored := &configbutleraiv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "stored-namespaced-scope"},
		Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configbutleraiv1alpha3.NamespacedTargetReference{Name: "t", Namespace: "default"},
			Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
				Resources: []string{"configmaps"},
				Scope:     configbutleraiv1alpha3.ResourceScopeNamespaced,
			}},
		},
	}
	createEventually(ctx, t, c, stored)

	// Narrow it back to what ships. The object in etcd now no longer validates against its own CRD.
	setScopeEnum(ctx, t, c, "Cluster")
	requireCreateRejected(ctx, t, c)

	var refused configbutleraiv1alpha3.ClusterWatchRule
	if err := c.Get(ctx, client.ObjectKey{Name: stored.Name}, &refused); err != nil {
		t.Fatalf("get stored object: %v", err)
	}
	//nolint:staticcheck // reading the deprecated field is the point: the value must have survived.
	if got := refused.Spec.Rules[0].Scope; got != configbutleraiv1alpha3.ResourceScopeNamespaced {
		t.Fatalf("stored spec value did not survive the narrowing: scope = %q, want %q",
			got, configbutleraiv1alpha3.ResourceScopeNamespaced)
	}

	refused.Status.ObservedGeneration = refused.Generation
	refused.Status.Conditions = []metav1.Condition{{
		Type:               "Stalled",
		Status:             metav1.ConditionTrue,
		Reason:             "ClusterScopeOnly",
		Message:            "spec.rules[].scope: Namespaced is no longer supported",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: refused.Generation,
	}}
	if err := c.Status().Update(ctx, &refused); err != nil {
		t.Fatalf("status update on an object whose STORED spec no longer validates was rejected: %v\n"+
			"The loud-rejection pattern is unsafe here. Fall back to widening the enum and reporting "+
			"the refusal from the compile path (docs/design/gittarget-api-wave.md).", err)
	}
}

// setScopeEnum rewrites the served enum for ClusterWatchRule spec.rules[].scope.
func setScopeEnum(ctx context.Context, t *testing.T, c client.Client, values ...string) {
	t.Helper()
	var crd apiextv1.CustomResourceDefinition
	if err := c.Get(ctx, client.ObjectKey{Name: "clusterwatchrules.configbutler.ai"}, &crd); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	items := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["rules"].Items.Schema
	scope := items.Properties["scope"]
	scope.Enum = nil
	for _, v := range values {
		scope.Enum = append(scope.Enum, apiextv1.JSON{Raw: fmt.Appendf(nil, "%q", v)})
	}
	items.Properties["scope"] = scope
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
	t.Fatalf("create never succeeded under the widened schema: %v", err)
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
				TargetRef: configbutleraiv1alpha3.NamespacedTargetReference{Name: "t", Namespace: "default"},
				Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
					Resources: []string{"configmaps"},
					Scope:     configbutleraiv1alpha3.ResourceScopeNamespaced,
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
