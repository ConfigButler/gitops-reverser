/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
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

		gitTarget := &configbutleraiv1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha1.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha1.GitProviderReference{Name: "prov-a", Kind: "GitProvider"},
				Branch:      "main",
				Path:        "apps",
			},
		}
		Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, gitTarget) })

		// A no-op update (the controller keeps writing status; re-applying an unchanged
		// spec must still be allowed) succeeds.
		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha1.GitTarget{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		// Each destination field is immutable. Eventually loops past any optimistic-lock
		// conflict from a concurrent status write so the assertion lands on the real
		// immutability rejection, not a transient 409.
		expectImmutable := func(mutate func(*configbutleraiv1alpha1.GitTarget), wantMsg string) {
			Eventually(func(g Gomega) {
				current := &configbutleraiv1alpha1.GitTarget{}
				g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
				mutate(current)
				err := k8sClient.Update(ctx, current)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(wantMsg))
			}, timeout, interval).Should(Succeed())
		}

		expectImmutable(func(gt *configbutleraiv1alpha1.GitTarget) {
			gt.Spec.Path = "moved"
		}, "spec.path is immutable")
		expectImmutable(func(gt *configbutleraiv1alpha1.GitTarget) {
			gt.Spec.Branch = "develop"
		}, "spec.branch is immutable")
		expectImmutable(func(gt *configbutleraiv1alpha1.GitTarget) {
			gt.Spec.ProviderRef.Name = "prov-b"
		}, "spec.providerRef is immutable")
	})

	It("treats an absent path and an explicit empty path as the same root destination", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "immutable-root-target", Namespace: "default"}

		// Create with path omitted: it must default to "" (the repo root), not stay absent.
		gitTarget := &configbutleraiv1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha1.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha1.GitProviderReference{Name: "prov-a", Kind: "GitProvider"},
				Branch:      "main",
				// Path omitted.
			},
		}
		Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, gitTarget) })

		stored := &configbutleraiv1alpha1.GitTarget{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Spec.Path).To(Equal(""), "an omitted path must default to the empty (root) path")

		// Re-applying with an explicit empty path is a no-op, not an immutable change.
		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha1.GitTarget{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			current.Spec.Path = ""
			g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}, timeout, interval).Should(Succeed())
	})
})
