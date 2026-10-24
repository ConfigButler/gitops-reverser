// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
)

// The API server holds the applier claim kinds to their contract: each names one applier and its
// cluster, neither can be pointed elsewhere once created, and
// a GitTarget names an applier claim by a kind this release serves.
var _ = Describe("Applier claim schema", func() {
	ctx := context.Background()
	create := func(obj client.Object) error {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
		return err
	}
	flux := func(name string) *configbutleraiv1alpha3.FluxClaim {
		return &configbutleraiv1alpha3.FluxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: configbutleraiv1alpha3.FluxClaimSpec{
				KubeProviderRef:  meta.NamespacedObjectReference{Name: "hub"},
				KustomizationRef: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "apps"},
			},
		}
	}
	argo := func(name string) *configbutleraiv1alpha3.ArgoCDClaim {
		return &configbutleraiv1alpha3.ArgoCDClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: configbutleraiv1alpha3.ArgoCDClaimSpec{
				KubeProviderRef: meta.NamespacedObjectReference{Name: "hub"},
				ApplicationRef:  meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
			},
		}
	}

	It("requires names", func() {
		// A reference naming only a namespace names no KubeProvider; there is no default to fall
		// back to.
		noCluster := flux("flux-no-cluster")
		noCluster.Spec.KubeProviderRef = meta.NamespacedObjectReference{Namespace: "default"}
		Expect(create(noCluster)).To(MatchError(ContainSubstring("spec.kubeProviderRef.name must not be empty")))

		noApplication := argo("argo-no-app")
		noApplication.Spec.ApplicationRef.Name = ""
		Expect(create(noApplication)).To(MatchError(ContainSubstring("spec.applicationRef.name must not be empty")))

		noKustomization := flux("flux-no-ks")
		noKustomization.Spec.KustomizationRef.Name = ""
		Expect(create(noKustomization)).To(MatchError(ContainSubstring("spec.kustomizationRef.name must not be empty")))
	})

	// An applier claim no GitTarget names adopts nothing, so it carries no finalizer and deletes at once;
	// it still publishes its conditions, Ready because it does all it is asked to.
	It("publishes a new FluxClaim's conditions, and holds nothing while no GitTarget names it", func() {
		claim := flux("flux-first-pass")
		Expect(create(claim)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
			ready := findCondition(claim.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(applier.ReasonNotReferenced))
		}, 20*time.Second, 100*time.Millisecond).Should(Succeed())
		Expect(claim.Finalizers).NotTo(ContainElement(configbutleraiv1alpha3.FluxClaimFinalizer))
		inControl := findCondition(claim.Status.Conditions, ApplierClaimConditionInControl)
		Expect(inControl).NotTo(BeNil())
		Expect(inControl.Reason).To(Equal(applier.ReasonNotReferenced))
	})

	It("never points an applier claim at another applier or cluster", func() {
		// The suite's FluxClaim reconciler writes the applier claim too (its finalizer), so each update
		// is made from a fresh read, and one that loses that race is made again.
		update := func(obj client.Object, mutate func()) error {
			var err error
			Eventually(func() bool {
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
				mutate()
				err = k8sClient.Update(ctx, obj)
				return !apierrors.IsConflict(err)
			}).Should(BeTrue())
			return err
		}
		fluxClaim := flux("flux-immutable")
		Expect(create(fluxClaim)).To(Succeed())
		Expect(update(fluxClaim, func() { fluxClaim.Spec.KustomizationRef.Name = "other" })).
			To(MatchError(ContainSubstring("spec.kustomizationRef is immutable")))
		Expect(update(fluxClaim, func() { fluxClaim.Spec.KubeProviderRef.Name = "other" })).
			To(MatchError(ContainSubstring("spec.kubeProviderRef is immutable")))

		argoClaim := argo("argo-immutable")
		Expect(create(argoClaim)).To(Succeed())
		Expect(update(argoClaim, func() { argoClaim.Spec.ApplicationRef.Namespace = "elsewhere" })).
			To(MatchError(ContainSubstring("spec.applicationRef is immutable")))
	})

	It("lets a GitTarget name an applier claim of a kind it serves, and requires one for Reapply", func() {
		target := func(name string, ref *configbutleraiv1alpha3.ApplierClaimReference) *configbutleraiv1alpha3.GitTarget {
			return &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					KubeProviderRef: testKubeProviderRef(),
					GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("prov", "main")},
					Path:            "apps/" + name,
					ApplierClaimRef: ref,
				},
			}
		}
		Expect(create(target("names-flux", &configbutleraiv1alpha3.ApplierClaimReference{
			Kind: configbutleraiv1alpha3.ApplierClaimKindFlux, Name: "apps",
		}))).To(Succeed())
		Expect(create(target("names-argo", &configbutleraiv1alpha3.ApplierClaimReference{
			Kind: configbutleraiv1alpha3.ApplierClaimKindArgoCD, Name: "apps",
		}))).To(Succeed())
		Expect(create(target("names-webhook", &configbutleraiv1alpha3.ApplierClaimReference{
			Kind: "WebhookClaim", Name: "apps",
		}))).To(MatchError(ContainSubstring("Unsupported value")))

		reapply := target("reapply-alone", nil)
		reapply.Spec.OnRefusal = configbutleraiv1alpha3.RefusalActionReapply
		Expect(create(reapply)).To(MatchError(ContainSubstring("spec.onRefusal: Reapply needs spec.applierClaimRef")))
	})
})

// The applier claim reconciler runs under the manager: an applier claim created gets its trio, Ready while no
// GitTarget names it, and a GitTarget naming it re-runs the check, though nothing else changed on
// the applier claim. The suite reaches no applier cluster, so that check goes no further.
var _ = Describe("Applier claim reconcile", func() {
	ctx := context.Background()

	It("publishes the applier claim's verdict", func() {
		claim := &configbutleraiv1alpha3.ArgoCDClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "reconciled", Namespace: "default"},
			Spec: configbutleraiv1alpha3.ArgoCDClaimSpec{
				KubeProviderRef: testKubeProviderRef(),
				ApplicationRef:  meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, claim) })

		configuredReason := func(g Gomega) string {
			got := &configbutleraiv1alpha3.ArgoCDClaim{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), got)).To(Succeed())
			g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
			g.Expect(configured).NotTo(BeNil())
			return configured.Reason
		}
		Eventually(func(g Gomega) {
			g.Expect(configuredReason(g)).To(Equal(applier.ReasonNotReferenced))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "names-reconciled", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef: meta.NamespacedObjectReference{
					Name: gitBranchNameForTest("prov", "main"),
				},
				Path: "apps/reconciled",
				ApplierClaimRef: &configbutleraiv1alpha3.ApplierClaimReference{
					Kind: configbutleraiv1alpha3.ApplierClaimKindArgoCD, Name: "reconciled",
				},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		Eventually(func(g Gomega) {
			g.Expect(configuredReason(g)).NotTo(Equal(applier.ReasonNotReferenced))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
	})
})
