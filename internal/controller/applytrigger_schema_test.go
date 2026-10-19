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

// The API server holds the trigger kinds to their contract: each names one applier and its
// cluster, neither can be pointed elsewhere once created, quietPeriod is bounded and defaulted, and
// a GitTarget names a trigger by a kind this release serves.
var _ = Describe("Apply trigger schema", func() {
	ctx := context.Background()
	create := func(obj client.Object) error {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
		return err
	}
	flux := func(name string) *configbutleraiv1alpha3.FluxTrigger {
		return &configbutleraiv1alpha3.FluxTrigger{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: configbutleraiv1alpha3.FluxTriggerSpec{
				ClusterProviderRef: &meta.LocalObjectReference{Name: "hub"},
				KustomizationRef:   meta.NamespacedObjectReference{Namespace: "flux-system", Name: "apps"},
			},
		}
	}
	argo := func(name string) *configbutleraiv1alpha3.ArgoCDTrigger {
		return &configbutleraiv1alpha3.ArgoCDTrigger{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: configbutleraiv1alpha3.ArgoCDTriggerSpec{
				ClusterProviderRef: &meta.LocalObjectReference{Name: "hub"},
				ApplicationRef:     meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
			},
		}
	}

	It("defaults a FluxTrigger's quiet period, and bounds it", func() {
		trigger := flux("flux-default")
		Expect(create(trigger)).To(Succeed())
		Expect(trigger.Spec.QuietPeriod).NotTo(BeNil())
		Expect(trigger.Spec.QuietPeriod.Duration).To(Equal(configbutleraiv1alpha3.DefaultApplierQuietPeriod))

		long := flux("flux-long")
		long.Spec.QuietPeriod = &metav1.Duration{Duration: 2 * time.Hour}
		Expect(create(long)).To(MatchError(ContainSubstring("spec.quietPeriod must be a Go duration of at most 1h")))
	})

	It("defaults the cluster to the default ClusterProvider, as a GitTarget does, and requires names", func() {
		omitted := argo("argo-default-cluster")
		omitted.Spec.ClusterProviderRef = nil
		Expect(create(omitted)).To(Succeed())
		Expect(omitted.Spec.ClusterProviderRef).To(Equal(
			&meta.LocalObjectReference{Name: configbutleraiv1alpha3.DefaultClusterProviderName}))

		noCluster := flux("flux-no-cluster")
		noCluster.Spec.ClusterProviderRef.Name = ""
		Expect(create(noCluster)).To(MatchError(ContainSubstring("spec.clusterProviderRef.name must not be empty")))

		noApplication := argo("argo-no-app")
		noApplication.Spec.ApplicationRef.Name = ""
		Expect(create(noApplication)).To(MatchError(ContainSubstring("spec.applicationRef.name must not be empty")))

		noKustomization := flux("flux-no-ks")
		noKustomization.Spec.KustomizationRef.Name = ""
		Expect(create(noKustomization)).To(MatchError(ContainSubstring("spec.kustomizationRef.name must not be empty")))
	})

	// A trigger no GitTarget names adopts nothing, so it carries no finalizer and deletes at once;
	// it still publishes its conditions, Ready because it does all it is asked to.
	It("publishes a new FluxTrigger's conditions, and holds nothing while no GitTarget names it", func() {
		trigger := flux("flux-first-pass")
		Expect(create(trigger)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(trigger), trigger)).To(Succeed())
			ready := findCondition(trigger.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal(applier.ReasonNotReferenced))
		}, 20*time.Second, 100*time.Millisecond).Should(Succeed())
		Expect(trigger.Finalizers).NotTo(ContainElement(configbutleraiv1alpha3.FluxTriggerFinalizer))
		held := findCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld)
		Expect(held).NotTo(BeNil())
		Expect(held.Reason).To(Equal(applier.ReasonNotReferenced))
	})

	It("never points a trigger at another applier or cluster", func() {
		// The suite's FluxTrigger reconciler writes the trigger too (its finalizer), so each update
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
		fluxTrigger := flux("flux-immutable")
		Expect(create(fluxTrigger)).To(Succeed())
		Expect(update(fluxTrigger, func() { fluxTrigger.Spec.KustomizationRef.Name = "other" })).
			To(MatchError(ContainSubstring("spec.kustomizationRef is immutable")))
		Expect(update(fluxTrigger, func() { fluxTrigger.Spec.ClusterProviderRef.Name = "other" })).
			To(MatchError(ContainSubstring("spec.clusterProviderRef is immutable")))
		Expect(update(fluxTrigger, func() {
			fluxTrigger.Spec.QuietPeriod = &metav1.Duration{Duration: time.Minute}
		})).To(Succeed(), "the quiet period is not what it holds")

		argoTrigger := argo("argo-immutable")
		Expect(create(argoTrigger)).To(Succeed())
		Expect(update(argoTrigger, func() { argoTrigger.Spec.ApplicationRef.Namespace = "elsewhere" })).
			To(MatchError(ContainSubstring("spec.applicationRef is immutable")))
	})

	It("lets a GitTarget name a trigger of a kind it serves, and requires one for Reapply", func() {
		target := func(name string, ref *configbutleraiv1alpha3.ApplyTriggerReference) *configbutleraiv1alpha3.GitTarget {
			return &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitProviderRef:  meta.LocalObjectReference{Name: "prov"},
					Branch:          "main",
					Path:            "apps/" + name,
					ApplyTriggerRef: ref,
				},
			}
		}
		Expect(create(target("names-flux", &configbutleraiv1alpha3.ApplyTriggerReference{
			Kind: configbutleraiv1alpha3.ApplyTriggerKindFlux, Name: "apps",
		}))).To(Succeed())
		Expect(create(target("names-argo", &configbutleraiv1alpha3.ApplyTriggerReference{
			Kind: configbutleraiv1alpha3.ApplyTriggerKindArgoCD, Name: "apps",
		}))).To(Succeed())
		Expect(create(target("names-webhook", &configbutleraiv1alpha3.ApplyTriggerReference{
			Kind: "WebhookTrigger", Name: "apps",
		}))).To(MatchError(ContainSubstring("Unsupported value")))

		reapply := target("reapply-alone", nil)
		reapply.Spec.OnRefusal = configbutleraiv1alpha3.RefusalActionReapply
		Expect(create(reapply)).To(MatchError(ContainSubstring("spec.onRefusal: Reapply needs spec.applyTriggerRef")))
	})
})

// The trigger reconciler runs under the manager: a trigger created gets its trio, Ready while no
// GitTarget names it, and a GitTarget naming it re-runs the check, though nothing else changed on
// the trigger. The suite reaches no applier cluster, so that check goes no further.
var _ = Describe("Apply trigger reconcile", func() {
	ctx := context.Background()

	It("publishes the trigger's verdict", func() {
		trigger := &configbutleraiv1alpha3.ArgoCDTrigger{
			ObjectMeta: metav1.ObjectMeta{Name: "reconciled", Namespace: "default"},
			Spec: configbutleraiv1alpha3.ArgoCDTriggerSpec{
				ClusterProviderRef: &meta.LocalObjectReference{Name: configbutleraiv1alpha3.DefaultClusterProviderName},
				ApplicationRef:     meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"},
			},
		}
		Expect(k8sClient.Create(ctx, trigger)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, trigger) })

		configuredReason := func(g Gomega) string {
			got := &configbutleraiv1alpha3.ArgoCDTrigger{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(trigger), got)).To(Succeed())
			g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			configured := findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured)
			g.Expect(configured).NotTo(BeNil())
			return configured.Reason
		}
		Eventually(func(g Gomega) {
			g.Expect(configuredReason(g)).To(Equal(applier.ReasonNotReferenced))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "names-reconciled", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				GitProviderRef: meta.LocalObjectReference{Name: "prov"}, Branch: "main", Path: "apps/reconciled",
				ApplyTriggerRef: &configbutleraiv1alpha3.ApplyTriggerReference{
					Kind: configbutleraiv1alpha3.ApplyTriggerKindArgoCD, Name: "reconciled",
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
