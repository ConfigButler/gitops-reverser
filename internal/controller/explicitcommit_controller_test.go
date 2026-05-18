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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("ExplicitCommit controller", func() {
	const namespace = "default"

	It("stamps a freshly created ExplicitCommit as WaitingForAuditEvent", func() {
		explicitCommit := &configbutleraiv1alpha1.ExplicitCommit{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha1.ExplicitCommitSpec{
				GitTargetRef: configbutleraiv1alpha1.ExplicitCommitGitTargetReference{
					Name: "team-a-config",
				},
				Message: "increase checkout API memory",
			},
		}
		Expect(k8sClient.Create(ctx, explicitCommit)).To(Succeed())
		key := client.ObjectKeyFromObject(explicitCommit)

		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha1.ExplicitCommit
			g.Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
			g.Expect(fetched.Status.Phase).To(Equal(
				configbutleraiv1alpha1.ExplicitCommitPhaseWaitingForAuditEvent))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("does not overwrite a terminal phase written by the audit consumer", func() {
		explicitCommit := &configbutleraiv1alpha1.ExplicitCommit{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha1.ExplicitCommitSpec{
				GitTargetRef: configbutleraiv1alpha1.ExplicitCommitGitTargetReference{
					Name: "team-a-config",
				},
			},
		}
		Expect(k8sClient.Create(ctx, explicitCommit)).To(Succeed())
		key := client.ObjectKeyFromObject(explicitCommit)

		// Wait for the initial stamp.
		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha1.ExplicitCommit
			g.Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
			g.Expect(fetched.Status.Phase).To(Equal(
				configbutleraiv1alpha1.ExplicitCommitPhaseWaitingForAuditEvent))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		// Simulate the audit consumer writing the terminal phase.
		var fetched configbutleraiv1alpha1.ExplicitCommit
		Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
		fetched.Status.Phase = configbutleraiv1alpha1.ExplicitCommitPhaseCommitted
		fetched.Status.Branch = "main"
		fetched.Status.SHA = "abc123"
		Expect(k8sClient.Status().Update(ctx, &fetched)).To(Succeed())

		// The controller must leave the terminal phase intact.
		Consistently(func(g Gomega) {
			var checked configbutleraiv1alpha1.ExplicitCommit
			g.Expect(k8sClient.Get(ctx, key, &checked)).To(Succeed())
			g.Expect(checked.Status.Phase).To(Equal(
				configbutleraiv1alpha1.ExplicitCommitPhaseCommitted))
			g.Expect(checked.Status.SHA).To(Equal("abc123"))
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
