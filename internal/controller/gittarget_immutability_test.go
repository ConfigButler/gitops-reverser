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

// A GitTarget's repository — providerRef — is immutable: pointing at a different
// repository is not a move, it is a different object, with nothing to migrate and nothing
// to observe. Its branch and path ARE mutable: changing either is a supported retarget,
// which the controller drives (see gittarget_retarget.go and
// docs/design/multi-tenant/gittarget-retarget.md). The snapshot gate is preserved by
// status.observedDestination rather than by freezing the spec.
var _ = Describe("GitTarget Destination Mutability", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	It("rejects a providerRef change, accepts branch and path changes, and allows a no-op update", func() {
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

		// Eventually loops past any optimistic-lock conflict from a concurrent status write
		// so each assertion lands on the real API-server verdict, not a transient 409.
		expectMutable := func(mutate func(*configbutleraiv1alpha3.GitTarget)) {
			Eventually(func(g Gomega) {
				current := &configbutleraiv1alpha3.GitTarget{}
				g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
				mutate(current)
				g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}, timeout, interval).Should(Succeed())
		}

		expectMutable(func(gt *configbutleraiv1alpha3.GitTarget) { gt.Spec.Path = "moved" })
		expectMutable(func(gt *configbutleraiv1alpha3.GitTarget) { gt.Spec.Branch = "develop" })

		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha3.GitTarget{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			current.Spec.ProviderRef.Name = "prov-b"
			err := k8sClient.Update(ctx, current)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.providerRef is immutable"))
			// The message must point at the supported move, not just refuse.
			g.Expect(err.Error()).To(ContainSubstring("spec.branch and spec.path are mutable"))
		}, timeout, interval).Should(Succeed())
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
