// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// spec.applier names exactly one applier, of one kind, and quietPeriod (which times a CatchUp)
// belongs to a Kustomization alone: Argo CD is never asked to catch up. The API server enforces
// both, so a knob that would do nothing is never accepted.
var _ = Describe("GitTarget applier schema", func() {
	kustomization := &meta.NamespacedObjectReference{Namespace: "flux-system", Name: "apps"}
	application := &meta.NamespacedObjectReference{Namespace: "argocd", Name: "apps"}
	quiet := &metav1.Duration{Duration: 5 * time.Second}

	create := func(name string, applier *configbutleraiv1alpha3.GitTargetApplier) error {
		ctx := context.Background()
		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				GitProviderRef: meta.LocalObjectReference{Name: "prov"},
				Branch:         "main",
				Path:           "apps/" + name,
				Applier:        applier,
			},
		}
		err := k8sClient.Create(ctx, target)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		}
		return err
	}

	It("accepts a Kustomization, with or without a quiet period, and an Application without one", func() {
		Expect(create("applier-kustomization", &configbutleraiv1alpha3.GitTargetApplier{
			KustomizationRef: kustomization, QuietPeriod: quiet,
		})).To(Succeed())
		Expect(create("applier-kustomization-default", &configbutleraiv1alpha3.GitTargetApplier{
			KustomizationRef: kustomization,
		})).To(Succeed())
		Expect(create("applier-application", &configbutleraiv1alpha3.GitTargetApplier{
			ApplicationRef: application,
		})).To(Succeed())
	})

	It("rejects both kinds at once, and neither", func() {
		Expect(create("applier-both", &configbutleraiv1alpha3.GitTargetApplier{
			KustomizationRef: kustomization, ApplicationRef: application,
		})).To(MatchError(ContainSubstring("exactly one of kustomizationRef and applicationRef")))
		Expect(create("applier-neither", &configbutleraiv1alpha3.GitTargetApplier{})).
			To(MatchError(ContainSubstring("exactly one of kustomizationRef and applicationRef")))
	})

	It("rejects a quiet period on an Application", func() {
		Expect(create("applier-application-quiet", &configbutleraiv1alpha3.GitTargetApplier{
			ApplicationRef: application, QuietPeriod: quiet,
		})).To(MatchError(ContainSubstring("quietPeriod applies only to a kustomizationRef")))
	})
})
