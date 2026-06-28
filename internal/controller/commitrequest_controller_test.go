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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

var _ = Describe("CommitRequest controller", func() {
	const namespace = "default"

	// The suite registers the reconciler in attributed mode (a non-nil AuthorLookup
	// that never resolves) with a Finalizer that never resolves, so these specs cover
	// the in-progress stamp and the terminal short-circuit only; the full
	// attribute → attach → terminal flow and the committer-only path are covered by
	// the unit tests in commitrequest_controller_unit_test.go.
	It("stamps a freshly created CommitRequest with in-progress conditions", func() {
		commitRequest := &configbutleraiv1alpha2.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha2.CommitRequestSpec{
				TargetRef: configbutleraiv1alpha2.LocalTargetReference{
					Name: "team-a-config",
				},
				Message: "increase checkout API memory",
			},
		}
		Expect(k8sClient.Create(ctx, commitRequest)).To(Succeed())
		key := client.ObjectKeyFromObject(commitRequest)

		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha2.CommitRequest
			g.Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
			reconciling := apimeta.FindStatusCondition(fetched.Status.Conditions, ConditionTypeReconciling)
			g.Expect(reconciling).NotTo(BeNil())
			g.Expect(reconciling.Status).To(Equal(metav1.ConditionTrue))
			ready := apimeta.FindStatusCondition(fetched.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("does not overwrite a terminal outcome that is already recorded", func() {
		commitRequest := &configbutleraiv1alpha2.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha2.CommitRequestSpec{
				TargetRef: configbutleraiv1alpha2.LocalTargetReference{
					Name: "team-a-config",
				},
			},
		}
		Expect(k8sClient.Create(ctx, commitRequest)).To(Succeed())
		key := client.ObjectKeyFromObject(commitRequest)

		// Wait for the initial in-progress stamp.
		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha2.CommitRequest
			g.Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
			ready := apimeta.FindStatusCondition(fetched.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		// Simulate a finalize having recorded the terminal outcome (Ready=True).
		var fetched configbutleraiv1alpha2.CommitRequest
		Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
		apimeta.SetStatusCondition(&fetched.Status.Conditions, metav1.Condition{
			Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: crReasonCommitted, Message: "committed",
		})
		apimeta.SetStatusCondition(&fetched.Status.Conditions, metav1.Condition{
			Type: ConditionTypeReconciling, Status: metav1.ConditionFalse, Reason: crReasonCommitted, Message: "done",
		})
		fetched.Status.Branch = "main"
		fetched.Status.SHA = "abc123"
		Expect(k8sClient.Status().Update(ctx, &fetched)).To(Succeed())

		// The controller must leave the terminal outcome intact.
		Consistently(func(g Gomega) {
			var checked configbutleraiv1alpha2.CommitRequest
			g.Expect(k8sClient.Get(ctx, key, &checked)).To(Succeed())
			ready := apimeta.FindStatusCondition(checked.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(checked.Status.SHA).To(Equal("abc123"))
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
