// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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
		keeper, err := flux.Release(ctx, ref, false)
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

// The preconditions a resume carries, on a real API server. Flux.resume reads the Kustomization
// again just before it resumes it, and applies spec.suspend: false with that read's uid and
// resourceVersion, so the API server refuses the resume of a Kustomization that changed since it
// was checked. internal/applier/fluxsim models the answers; these pin them: a resourceVersion that
// moved is a Conflict, another object's uid is Invalid, a deleted object's uid is a Conflict rather
// than a create, and neither precondition lands in managedFields.
var _ = Describe("A Kustomization resume guarded by uid and resourceVersion, on a real API server", func() {
	const (
		repositoryURL = "https://example.com/apps.git"
		branch        = "main"
		commit        = "0123456789abcdef0123456789abcdef01234567"
	)
	var (
		ctx            context.Context
		dyn            dynamic.Interface
		kustomizations dynamic.ResourceInterface
		ref            k8stypes.NamespacedName
	)
	// setUp makes a namespace holding GitRepository apps, whose artifact holds commit, and
	// Kustomization apps, which applies it and is suspended by GitOps Reverser.
	setUp := func(namespace string) {
		ctx = context.Background()
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
			CRDs: []*apiextensionsv1.CustomResourceDefinition{kustomizationCRD(), gitRepositoryCRD()},
		})
		Expect(err).NotTo(HaveOccurred())
		dyn, err = dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		sources := dyn.Resource(fluxsim.GitRepositoryGVR()).Namespace(namespace)
		source, err := sources.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
			"metadata": map[string]any{"namespace": namespace, "name": "apps"},
			"spec":     map[string]any{"url": repositoryURL, "ref": map[string]any{"branch": branch}},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(unstructured.SetNestedField(source.Object, branch+"@sha1:"+commit,
			"status", "artifact", "revision")).To(Succeed())
		_, err = sources.UpdateStatus(ctx, source, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		kustomizations = dyn.Resource(fluxsim.KustomizationGVR()).Namespace(namespace)
		_, err = kustomizations.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"namespace": namespace, "name": "apps"},
			"spec": map[string]any{
				"path":      "./apps",
				"sourceRef": map[string]any{"kind": "GitRepository", "name": "apps"},
			},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ref = k8stypes.NamespacedName{Namespace: namespace, Name: "apps"}
		_, err = applier.NewFlux(dyn).Suspend(ctx, ref)
		Expect(err).NotTo(HaveOccurred())
	}
	// request asks Flux.Request for an apply of commit, with inControl run where the applier claim's
	// InControl is checked: after the resume's read of the Kustomization, before its write.
	request := func(inControl func(context.Context) error) (applier.Started, error) {
		return applier.NewFlux(dyn).Request(ctx, ref, applier.Ask{
			Want:      applier.Binding{RepositoryURL: repositoryURL, Branch: branch},
			Commit:    commit,
			Reason:    configbutleraiv1alpha3.ApplyReasonRepair,
			Token:     "guarded-resume",
			InControl: inControl,
		})
	}
	read := func() *unstructured.Unstructured {
		ks, err := kustomizations.Get(ctx, ref.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ks
	}
	suspendedNow := func() bool {
		value, _, _ := unstructured.NestedBool(read().Object, "spec", "suspend")
		return value
	}

	It("resumes the Kustomization it read, and keeps uid and resourceVersion out of managedFields", func() {
		setUp("guarded-resume")
		started, err := request(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(started.Outcome).To(Equal(applier.OutcomeResumed))
		Expect(started.Unconfirmed).NotTo(HaveOccurred())
		Expect(suspendedNow()).To(BeFalse())

		var ours []metav1.ManagedFieldsEntry
		for _, entry := range read().GetManagedFields() {
			if entry.Manager == applier.SuspendFieldManager {
				ours = append(ours, entry)
			}
		}
		Expect(ours).To(HaveLen(1))
		var fields map[string]any
		Expect(json.Unmarshal(ours[0].FieldsV1.GetRawBytes(), &fields)).To(Succeed())
		Expect(fields).To(HaveKey("f:spec"), "the resume owns spec.suspend")
		metadata, _ := fields["f:metadata"].(map[string]any)
		Expect(metadata).NotTo(HaveKey("f:uid"), "a precondition is no field the resume owns")
		Expect(metadata).NotTo(HaveKey("f:resourceVersion"), "a precondition is no field the resume owns")
	})

	It("does not resume a Kustomization written to after the resume's read", func() {
		setUp("guarded-resume-written")
		writes := 0
		_, err := request(func(ctx context.Context) error {
			writes++
			_, err := kustomizations.Patch(ctx, ref.Name, k8stypes.MergePatchType,
				fmt.Appendf(nil, `{"metadata":{"labels":{"write":"%d"}}}`, writes), metav1.PatchOptions{})
			return err
		})
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "the API server refuses the stale resourceVersion: %v", err)
		Expect(writes).To(BeNumerically(">", 1), "a Conflict reads the Kustomization again")
		Expect(suspendedNow()).To(BeTrue(), "nothing was resumed")
	})

	It("does not re-create a Kustomization deleted after the resume's read", func() {
		setUp("guarded-resume-deleted")
		_, err := request(func(ctx context.Context) error {
			return kustomizations.Delete(ctx, ref.Name, metav1.DeleteOptions{})
		})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the read after the Conflict finds it gone: %v", err)
		_, err = kustomizations.Get(ctx, ref.Name, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the resume did not re-create it")
	})

	// The answers themselves, to the apply the resume sends. applier's hold builds that apply and is
	// not exported, so guardedResume sends the same body: apiVersion, kind, namespace and name, the
	// preconditions, the requestedAt token and spec.suspend: false, forced under SuspendFieldManager.
	It("answers a moved resourceVersion with Conflict, another uid with Invalid, a deleted object with Conflict",
		func() {
			setUp("guarded-resume-answers")
			guardedResume := func(uid k8stypes.UID, resourceVersion string) error {
				obj := &unstructured.Unstructured{}
				obj.SetAPIVersion(fluxsim.KustomizationGVR().GroupVersion().String())
				obj.SetKind("Kustomization")
				obj.SetNamespace(ref.Namespace)
				obj.SetName(ref.Name)
				obj.SetUID(uid)
				obj.SetResourceVersion(resourceVersion)
				obj.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: "guarded-resume"})
				Expect(unstructured.SetNestedField(obj.Object, false, "spec", "suspend")).To(Succeed())
				_, err := kustomizations.Apply(ctx, ref.Name, obj,
					metav1.ApplyOptions{FieldManager: applier.SuspendFieldManager, Force: true})
				return err
			}

			By("a resourceVersion that moved: Conflict")
			checked := read()
			_, err := kustomizations.Patch(ctx, ref.Name, k8stypes.MergePatchType,
				[]byte(`{"metadata":{"labels":{"write":"1"}}}`), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = guardedResume(checked.GetUID(), checked.GetResourceVersion())
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
			Expect(suspendedNow()).To(BeTrue())

			By("another object's uid, over the current resourceVersion: Invalid")
			err = guardedResume("00000000-0000-0000-0000-000000000000", read().GetResourceVersion())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(suspendedNow()).To(BeTrue())

			By("a deleted object's uid: Conflict, and nothing is created")
			checked = read()
			Expect(kustomizations.Delete(ctx, ref.Name, metav1.DeleteOptions{})).To(Succeed())
			err = guardedResume(checked.GetUID(), checked.GetResourceVersion())
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("uid mismatch"))
			_, err = kustomizations.Get(ctx, ref.Name, metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the resume did not re-create it")

			By("the resourceVersion alone, which is why the resume carries the uid: it re-creates the object")
			Expect(guardedResume("", checked.GetResourceVersion())).To(Succeed())
			Expect(read().GetUID()).NotTo(Equal(checked.GetUID()))
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

// kustomizationCRD is as much of Flux's Kustomization CRD as spec.suspend's ownership and a
// request need: a structural schema whose spec.suspend is a boolean, so the API server tracks it as
// a field, and which keeps spec.sourceRef.
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
							"suspend":   {Type: "boolean"},
							"path":      {Type: "string"},
							"sourceRef": {Type: "object", XPreserveUnknownFields: &preserve},
						}},
						"status": {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}
}

// gitRepositoryCRD is as much of Flux's GitRepository CRD as a request reads: spec and status kept
// as written, and a status subresource for the artifact.
func gitRepositoryCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	gvr := fluxsim.GitRepositoryGVR()
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: gvr.Resource, Singular: "gitrepository", Kind: "GitRepository", ListKind: "GitRepositoryList",
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
						"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
						"status": {Type: "object", XPreserveUnknownFields: &preserve},
					},
				}},
			}},
		},
	}
}
