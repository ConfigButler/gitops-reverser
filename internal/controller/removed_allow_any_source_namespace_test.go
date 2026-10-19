// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// removedFieldMessage is what the spec-level CEL rule says of allowAnySourceNamespace: true.
const removedFieldMessage = "allowAnySourceNamespace was removed; set allowCrossNamespace.sources"

// TestRemovedAllowAnySourceNamespace_TrueIsRefusedOnEverySpecChange pins how the removal of
// ClusterProvider.spec.allowAnySourceNamespace stays loud, against the generated CRD on a real API
// server. true is refused on create and on any spec change, also for a provider that stored true
// before the rule existed: the rule sits on spec, so CRD validation ratcheting does not wave the
// stored value through once anything in spec moves. false, which the old default stored on every
// provider, stays accepted, and so does removing the field.
func TestRemovedAllowAnySourceNamespace_TrueIsRefusedOnEverySpecChange(t *testing.T) {
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
	for _, add := range []func(*runtime.Scheme) error{
		scheme.AddToScheme, configbutleraiv1alpha3.AddToScheme, apiextv1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	provider := func(name string, removed *bool) *configbutleraiv1alpha3.ClusterProvider {
		return &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AccessFrom:                     &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
				RemovedAllowAnySourceNamespace: removed,
			},
		}
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), removedFieldMessage) {
			t.Fatalf("%s: want the removal refused by name, got %v", what, err)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: want it accepted, got %v", what, err)
		}
	}
	addNamespace := func(p *configbutleraiv1alpha3.ClusterProvider) {
		p.Spec.AccessFrom.Names = append(p.Spec.AccessFrom.Names, fmt.Sprintf("team-%d", len(p.Spec.AccessFrom.Names)))
	}

	refused("create with true", c.Create(ctx, provider("created-true", ptr.To(true))))

	kept := provider("kept-false", ptr.To(false))
	createEventually(ctx, t, c, kept)
	addNamespace(kept)
	accepted("a spec change keeping false", c.Update(ctx, kept))
	kept.Spec.RemovedAllowAnySourceNamespace = ptr.To(true)
	refused("an update setting true", c.Update(ctx, kept))
	if err := c.Get(ctx, client.ObjectKeyFromObject(kept), kept); err != nil {
		t.Fatal(err)
	}
	kept.Spec.RemovedAllowAnySourceNamespace = nil
	accepted("removing the field", c.Update(ctx, kept))

	// A provider that stored true before the rule existed, as one from the release that read it.
	setRemovedFieldRule(ctx, t, c, false)
	stored := provider("stored-true", ptr.To(true))
	createEventually(ctx, t, c, stored)
	setRemovedFieldRule(ctx, t, c, true)
	requireRemovedFieldRefused(ctx, t, c)

	if err := c.Get(ctx, client.ObjectKeyFromObject(stored), stored); err != nil {
		t.Fatal(err)
	}
	stored.Labels = map[string]string{"edited": "metadata-only"}
	accepted("a metadata-only edit of a stored true, which CEL does not see", c.Update(ctx, stored))
	addNamespace(stored)
	refused("a spec change on a stored true", c.Update(ctx, stored))
	stored.Spec.RemovedAllowAnySourceNamespace = nil
	accepted("the same spec change, removing the field", c.Update(ctx, stored))
}

// setRemovedFieldRule takes the spec-level rule refusing allowAnySourceNamespace: true out of the
// served ClusterProvider schema, or puts it back, standing in for the CRD of the release before it.
func setRemovedFieldRule(ctx context.Context, t *testing.T, c client.Client, present bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var crd apiextv1.CustomResourceDefinition
		if err := c.Get(ctx, client.ObjectKey{Name: "clusterproviders.configbutler.ai"}, &crd); err != nil {
			t.Fatalf("get CRD: %v", err)
		}
		spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		var rules apiextv1.ValidationRules
		for _, rule := range spec.XValidations {
			if !strings.Contains(rule.Rule, "allowAnySourceNamespace") {
				rules = append(rules, rule)
			}
		}
		if present {
			rules = append(rules, apiextv1.ValidationRule{
				Rule:    "!has(self.allowAnySourceNamespace) || !self.allowAnySourceNamespace",
				Message: removedFieldMessage,
			})
		}
		spec.XValidations = rules
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
		err := c.Update(ctx, &crd)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("update CRD schema: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireRemovedFieldRefused blocks until the restored rule is the one being served, proven by a
// create that must fail: without it, the assertions after could pass against the schema without
// the rule.
func requireRemovedFieldRefused(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		probe := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("removed-field-probe-%d", i)},
			Spec:       configbutleraiv1alpha3.ClusterProviderSpec{RemovedAllowAnySourceNamespace: ptr.To(true)},
		}
		if err := c.Create(ctx, probe); err != nil {
			return
		}
		_ = c.Delete(ctx, probe)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the restored rule never took effect, so the assertions after would be vacuous")
}
