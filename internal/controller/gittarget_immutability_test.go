// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A GitTarget's destination — providerRef, branch, path — is immutable: changing where
// it materializes would orphan the old materialization, so the API server rejects the
// change (CEL transition rules) and a relocation is a delete + recreate. This replaces
// the alternative of reconciling a destination move (which would need a generation-aware
// snapshot gate and worker rebinding); making it immutable removes that whole class of
// bug instead of handling it.
var _ = Describe("GitTarget Destination Immutability", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	It("rejects changes to providerRef, branch, and path but allows a no-op update", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "immutable-target", Namespace: "default"}

		gitTarget := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov-a"},
				Branch:      "main",
				Path:        "apps",
			},
		}
		Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, gitTarget) })

		// A no-op update (the controller keeps writing status; re-applying an unchanged
		// spec must still be allowed) succeeds.
		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha3.GitTarget{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		// Each destination field is immutable. Eventually loops past any optimistic-lock
		// conflict from a concurrent status write so the assertion lands on the real
		// immutability rejection, not a transient 409.
		expectImmutable := func(mutate func(*configbutleraiv1alpha3.GitTarget), wantMsg string) {
			Eventually(func(g Gomega) {
				current := &configbutleraiv1alpha3.GitTarget{}
				g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
				mutate(current)
				err := k8sClient.Update(ctx, current)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(wantMsg))
			}, timeout, interval).Should(Succeed())
		}

		expectImmutable(func(gt *configbutleraiv1alpha3.GitTarget) {
			gt.Spec.Path = "moved"
		}, "spec.path is immutable")
		expectImmutable(func(gt *configbutleraiv1alpha3.GitTarget) {
			gt.Spec.Branch = "develop"
		}, "spec.branch is immutable")
		expectImmutable(func(gt *configbutleraiv1alpha3.GitTarget) {
			gt.Spec.ProviderRef.Name = "prov-b"
		}, "spec.providerRef is immutable")
	})

	It("requires a non-empty path: rejects an omitted or empty path but allows an explicit \".\" root", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "root-policy-target", Namespace: "default"}

		base := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov-a"},
				Branch:      "main",
			},
		}

		// Omitting the path is rejected: with no default, a GitTarget can never silently
		// write to the repository root.
		omitted := base.DeepCopy()
		Expect(k8sClient.Create(ctx, omitted)).ShouldNot(Succeed())

		// An explicit empty string is rejected too: "" is too easy to leave blank by
		// accident to count as a deliberate root choice.
		empty := base.DeepCopy()
		empty.Spec.Path = ""
		Expect(k8sClient.Create(ctx, empty)).ShouldNot(Succeed())

		// "." is the deliberate, allowed way to target the repository root.
		root := base.DeepCopy()
		root.Spec.Path = "."
		Expect(k8sClient.Create(ctx, root)).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, root) })

		stored := &configbutleraiv1alpha3.GitTarget{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Spec.Path).To(Equal("."), "an explicit \".\" must be stored as the root path")
	})
})
