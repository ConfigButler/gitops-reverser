// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A GitTarget's destination — gitBranchRef and path — is immutable: changing where it materializes
// would orphan the old materialization, so the API server rejects the change (CEL transition rules)
// and a relocation is a delete + recreate. A GitBranch's spec is the repository branch it claims, so
// all of it is immutable the same way.
var _ = Describe("GitTarget Destination Immutability", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	// expectImmutable loops past any optimistic-lock conflict from a concurrent status write, so
	// the assertion lands on the real immutability rejection, not a transient 409.
	expectImmutable := func(
		ctx context.Context, key types.NamespacedName, obj client.Object, mutate func(), wantMsg string,
	) {
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			mutate()
			err := k8sClient.Update(ctx, obj)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(wantMsg))
		}, timeout, interval).Should(Succeed())
	}

	It("rejects changes to gitBranchRef and path but allows a no-op update", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "immutable-target", Namespace: "default"}

		gitTarget := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				GitBranchRef: &meta.NamespacedObjectReference{Name: gitBranchNameForTest("prov-a", "main")},
				Path:         "apps",
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

		current := &configbutleraiv1alpha3.GitTarget{}
		expectImmutable(ctx, key, current, func() { current.Spec.Path = "moved" }, "spec.path is immutable")
		expectImmutable(ctx, key, current, func() { current.Spec.GitBranchRef.Name = "other" },
			"spec.gitBranchRef is immutable")
		expectImmutable(ctx, key, current, func() { current.Spec.GitBranchRef.Namespace = "platform" },
			"spec.gitBranchRef is immutable")
	})

	It("rejects any change to a GitBranch's spec, including adding or removing parentBranch", func() {
		ctx := context.Background()
		withParent := types.NamespacedName{Name: "immutable-branch-parent", Namespace: "default"}
		withoutParent := types.NamespacedName{Name: "immutable-branch", Namespace: "default"}
		for _, tc := range []struct {
			key    types.NamespacedName
			parent string
		}{{withParent, "main"}, {withoutParent, ""}} {
			gitBranch := &configbutleraiv1alpha3.GitBranch{
				ObjectMeta: metav1.ObjectMeta{Name: tc.key.Name, Namespace: tc.key.Namespace},
				Spec: configbutleraiv1alpha3.GitBranchSpec{
					GitProviderRef: meta.LocalObjectReference{Name: "prov-a"},
					Branch:         "edits",
					ParentBranch:   tc.parent,
				},
			}
			Expect(k8sClient.Create(ctx, gitBranch)).Should(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, gitBranch) })
		}

		current := &configbutleraiv1alpha3.GitBranch{}
		expectImmutable(ctx, withParent, current, func() { current.Spec.GitProviderRef.Name = "prov-b" },
			"spec.gitProviderRef is immutable")
		expectImmutable(ctx, withParent, current, func() { current.Spec.Branch = "other" }, "spec.branch is immutable")
		expectImmutable(ctx, withParent, current, func() { current.Spec.ParentBranch = "release" },
			"spec.parentBranch is immutable")
		expectImmutable(ctx, withParent, current, func() { current.Spec.ParentBranch = "" },
			"spec.parentBranch is immutable")
		expectImmutable(ctx, withoutParent, current, func() { current.Spec.ParentBranch = "release" },
			"spec.parentBranch is immutable")

		empty := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitBranch",
			"metadata":   map[string]interface{}{"name": "empty-parent-branch", "namespace": "default"},
			"spec": map[string]interface{}{
				"gitProviderRef": map[string]interface{}{"name": "prov-a"},
				"branch":         "edits",
				"parentBranch":   "",
			},
		}}
		err := k8sClient.Create(ctx, empty)
		Expect(err).To(HaveOccurred(), "parentBranch can be omitted, never empty")
		Expect(err.Error()).To(ContainSubstring("spec.parentBranch"))
	})

	It("requires a non-empty path: rejects an omitted or empty path but allows an explicit \".\" root", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "root-policy-target", Namespace: "default"}

		base := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				GitBranchRef: &meta.NamespacedObjectReference{Name: gitBranchNameForTest("prov-a", "main")},
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
