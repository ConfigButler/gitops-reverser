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

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A GitProvider's repository URL is the destination identity its GitTargets materialize
// into, so it is immutable (delete + recreate to repoint). Everything else is
// operational and must stay mutable — especially allowedBranches: widening or narrowing
// the writable set is a routine change that must not force tearing down every GitTarget.
var _ = Describe("GitProvider URL Immutability", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	It("rejects a change to the URL but allows changing allowedBranches", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "immutable-provider", Namespace: "default"}

		provider := &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: configbutleraiv1alpha3.GitProviderSpec{
				URL:             "https://example.com/repo.git",
				AllowedBranches: []string{"main"},
			},
		}
		Expect(k8sClient.Create(ctx, provider)).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, provider) })

		// allowedBranches is operational and mutable: widening the set succeeds.
		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha3.GitProvider{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			current.Spec.AllowedBranches = []string{"main", "develop"}
			g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		// The URL is the destination identity and is immutable.
		Eventually(func(g Gomega) {
			current := &configbutleraiv1alpha3.GitProvider{}
			g.Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			current.Spec.URL = "https://example.com/other.git"
			err := k8sClient.Update(ctx, current)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.url is immutable"))
		}, timeout, interval).Should(Succeed())
	})
})
