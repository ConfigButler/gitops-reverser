// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
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
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
)

// The ownership of spec.suspend on a real API server, which internal/applier/fluxsim models. A
// forced apply of the value a field holds does not take the field: it shares it with the manager
// that set it. So a suspend that takes over a `flux suspend` (a merge patch, an Update entry of
// manager "flux") co-owns spec.suspend, and an empty apply alone leaves it set. Release has to take
// the update's share first, or a hand-back leaves the Kustomization suspended while the Released
// Event says it resumed.
var _ = Describe("Kustomization spec.suspend on a real API server", func() {
	It("is shared with a flux suspend, and Release still resumes it", func() {
		ctx := context.Background()
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
			CRDs: []*apiextensionsv1.CustomResourceDefinition{kustomizationCRD()},
		})
		Expect(err).NotTo(HaveOccurred())
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		const namespace = "suspend-ownership"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		kustomizations := dyn.Resource(fluxsim.KustomizationGVR()).Namespace(namespace)
		_, err = kustomizations.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"namespace": namespace, "name": "apps"},
			"spec":     map[string]any{"path": "./apps"},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ref := k8stypes.NamespacedName{Namespace: namespace, Name: "apps"}
		read := func() *unstructured.Unstructured {
			ks, err := kustomizations.Get(ctx, ref.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			return ks
		}

		By("flux suspend: a merge patch, an Update entry")
		_, err = kustomizations.Patch(ctx, ref.Name, k8stypes.MergePatchType, []byte(`{"spec":{"suspend":true}}`),
			metav1.PatchOptions{FieldManager: "flux"})
		Expect(err).NotTo(HaveOccurred())
		Expect(suspendEntries(read())).To(ConsistOf("flux/Update"))

		By("our forced apply of the same value shares the field")
		flux := applier.NewFlux(dyn)
		_, err = flux.Suspend(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(suspendEntries(read())).To(ConsistOf("flux/Update", applier.SuspendFieldManager+"/Apply"))

		By("Release takes the update's share, and resumes it")
		keeper, err := flux.Release(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		Expect(keeper).To(BeEmpty())
		released := read()
		_, found, _ := unstructured.NestedFieldNoCopy(released.Object, "spec", "suspend")
		Expect(found).To(BeFalse(), "spec.suspend is gone, so the Kustomization is resumed")
		Expect(suspendEntries(released)).To(BeEmpty())
		Expect(released.GetAnnotations()).NotTo(HaveKey(fluxmeta.ReconcileRequestAnnotation))
		for _, entry := range released.GetManagedFields() {
			Expect(entry.Manager).NotTo(Equal(applier.SuspendFieldManager), "nothing of GitOps Reverser's is left")
		}

		By("an update of the value the field holds takes nothing")
		_, err = flux.Suspend(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
		_, err = kustomizations.Patch(ctx, ref.Name, k8stypes.MergePatchType, []byte(`{"spec":{"suspend":true}}`),
			metav1.PatchOptions{FieldManager: "flux"})
		Expect(err).NotTo(HaveOccurred())
		Expect(suspendEntries(read())).To(ConsistOf(applier.SuspendFieldManager + "/Apply"))
	})
})

// suspendEntries lists the managedFields entries owning a Kustomization's spec.suspend, as
// "manager/operation".
func suspendEntries(ks *unstructured.Unstructured) []string {
	var entries []string
	for _, entry := range ks.GetManagedFields() {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		Expect(json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields)).To(Succeed())
		if _, found, _ := unstructured.NestedFieldNoCopy(fields, "f:spec", "f:suspend"); found {
			entries = append(entries, entry.Manager+"/"+string(entry.Operation))
		}
	}
	return entries
}

// kustomizationCRD is as much of Flux's Kustomization CRD as spec.suspend's ownership needs: a
// structural schema whose spec.suspend is a boolean, so the API server tracks it as a field.
func kustomizationCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	gvr := fluxsim.KustomizationGVR()
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: gvr.Resource, Singular: "kustomization", Kind: "Kustomization", ListKind: "KustomizationList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"suspend": {Type: "boolean"},
							"path":    {Type: "string"},
						}},
						"status": {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}
}
