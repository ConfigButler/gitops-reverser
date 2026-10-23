// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/applier/argosim"
)

// The ownership of an Application's automated sync switch on a real API server, which
// internal/applier/argosim models. Holding it off and handing it back must leave the Application as
// its own spec says: a hand-back that left an empty automated block behind would switch automated
// sync ON for an Application that was manual, since Argo CD reads an automated block without
// enabled as on.
var _ = Describe("Application automated sync switch on a real API server", Ordered, func() {
	const namespace = "argocd-switch"
	ctx := context.Background()
	var applications dynamic.ResourceInterface
	var argo *applier.ArgoCD

	BeforeAll(func() {
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
			CRDs: []*apiextensionsv1.CustomResourceDefinition{applicationCRD()},
		})
		Expect(err).NotTo(HaveOccurred())
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		applications = dyn.Resource(argosim.ApplicationGVR()).Namespace(namespace)
		argo = applier.NewArgoCD(dyn)
	})

	create := func(name string, syncPolicy map[string]any) k8stypes.NamespacedName {
		spec := map[string]any{"source": map[string]any{"repoURL": "https://github.com/acme/platform"}}
		if syncPolicy != nil {
			spec["syncPolicy"] = syncPolicy
		}
		_, err := applications.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"namespace": namespace, "name": name},
			"spec":     spec,
		}}, metav1.CreateOptions{FieldManager: "kubectl-client-side-apply"})
		Expect(err).NotTo(HaveOccurred())
		return k8stypes.NamespacedName{Namespace: namespace, Name: name}
	}
	// syncPolicy is the Application's spec.syncPolicy, nil when it has none.
	syncPolicy := func(ref k8stypes.NamespacedName) map[string]any {
		app, err := applications.Get(ctx, ref.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		policy, _, err := unstructured.NestedMap(app.Object, "spec", "syncPolicy")
		Expect(err).NotTo(HaveOccurred())
		return policy
	}
	state := func(ref k8stypes.NamespacedName) applier.AutomatedSync {
		held, err := argo.AutomatedSync(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		return held
	}

	It("hands a manual Application back manual, with no syncPolicy left", func() {
		ref := create("manual", nil)
		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		Expect(syncPolicy(ref)).To(Equal(map[string]any{"automated": map[string]any{"enabled": false}}))
		Expect(state(ref)).To(Equal(applier.AutomatedSync{Held: true}))

		on, err := argo.ReleaseAutomatedSync(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeFalse())
		Expect(syncPolicy(ref)).To(BeNil(), "the maps the hold created go with it")
	})

	It("hands an Application with automated sync on back on, its other settings untouched", func() {
		ref := create("automated", map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}})
		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		Expect(syncPolicy(ref)).To(Equal(map[string]any{
			"automated": map[string]any{"enabled": false, "prune": true, "selfHeal": true},
		}))

		on, err := argo.ReleaseAutomatedSync(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeTrue())
		Expect(syncPolicy(ref)).To(Equal(map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}}))
	})

	It("shares a switch its owner set off, and leaves it off on hand-back", func() {
		ref := create("owner-off", map[string]any{"automated": map[string]any{"enabled": false, "prune": true}})
		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		Expect(state(ref)).To(Equal(applier.AutomatedSync{Held: true}))

		on, err := argo.ReleaseAutomatedSync(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeFalse(), "the owner's own choice stays")
		Expect(syncPolicy(ref)).To(Equal(map[string]any{"automated": map[string]any{"enabled": false, "prune": true}}))
	})

	It("loses the switch to a click in Argo CD's UI, and takes it back", func() {
		ref := create("clicked", nil)
		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		_, err := applications.Patch(
			ctx,
			ref.Name,
			k8stypes.MergePatchType,
			[]byte(
				`{"spec":{"syncPolicy":{"automated":{"enabled":true}}}}`,
			),
			metav1.PatchOptions{FieldManager: "argocd-server"},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(state(ref)).To(Equal(applier.AutomatedSync{On: true}), "a write of another value takes the switch")

		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		Expect(state(ref)).To(Equal(applier.AutomatedSync{Held: true}), "and a forced apply takes it back")
		on, err := argo.ReleaseAutomatedSync(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(on).To(BeFalse())
		Expect(syncPolicy(ref)).To(BeNil())
	})

	It("keeps the switch through a sync, an update of the whole Application", func() {
		ref := create("synced", nil)
		Expect(argo.SwitchOff(ctx, ref)).To(BeFalse(), "switched off")
		app, err := applications.Get(ctx, ref.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedField(app.Object, map[string]any{"sync": map[string]any{"revision": "abc"}},
			"operation")).To(Succeed())
		_, err = applications.Update(ctx, app, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(state(ref)).To(Equal(applier.AutomatedSync{Held: true}), "an update owns only what it changed")
	})
})

// applicationCRD is as much of Argo CD's Application CRD as the switch needs: syncPolicy.automated
// typed, as Argo CD's own schema types it, so server-side apply tracks the switch as a field of its
// own; everything else kept as written.
func applicationCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	gvr := argosim.ApplicationGVR()
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: gvr.Resource, Singular: "application", Kind: "Application", ListKind: "ApplicationList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"source": {Type: "object", XPreserveUnknownFields: &preserve},
							"syncPolicy": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"automated": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"enabled":  {Type: "boolean"},
									"prune":    {Type: "boolean"},
									"selfHeal": {Type: "boolean"},
								}},
							}},
						}},
						"operation": {Type: "object", XPreserveUnknownFields: &preserve},
						"status":    {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}
}
