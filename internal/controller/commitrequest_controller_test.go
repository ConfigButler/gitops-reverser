// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

var _ = Describe("CommitRequest controller", func() {
	const namespace = "default"

	// The suite registers the reconciler in attributed-author mode (a non-nil AuthorLookup
	// that never resolves) with a Finalizer that never resolves, so these specs cover
	// the in-progress stamp and the terminal short-circuit only; the full
	// attribute → attach → terminal flow and the configured-author path are covered by
	// the unit tests in commitrequest_controller_unit_test.go.
	It("stamps a freshly created CommitRequest with in-progress conditions", func() {
		commitRequest := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{
					Name: "team-a-config",
				},
				Message: "increase checkout API memory",
			},
		}
		Expect(k8sClient.Create(ctx, commitRequest)).To(Succeed())
		key := client.ObjectKeyFromObject(commitRequest)

		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha3.CommitRequest
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
		commitRequest := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "save-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{
					Name: "team-a-config",
				},
			},
		}
		Expect(k8sClient.Create(ctx, commitRequest)).To(Succeed())
		key := client.ObjectKeyFromObject(commitRequest)

		// Wait for the initial in-progress stamp.
		Eventually(func(g Gomega) {
			var fetched configbutleraiv1alpha3.CommitRequest
			g.Expect(k8sClient.Get(ctx, key, &fetched)).To(Succeed())
			ready := apimeta.FindStatusCondition(fetched.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		// Simulate a finalize having recorded the terminal outcome (Ready=True).
		var fetched configbutleraiv1alpha3.CommitRequest
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
			var checked configbutleraiv1alpha3.CommitRequest
			g.Expect(k8sClient.Get(ctx, key, &checked)).To(Succeed())
			ready := apimeta.FindStatusCondition(checked.Status.Conditions, ConditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(checked.Status.SHA).To(Equal("abc123"))
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})

// The whitespace-only rejection is a CEL rule on spec.message. A rule that fails to compile is
// rejected only when the CRD is installed, and a rule that compiles but matches nothing simply
// never rejects anything, so it is pinned against a real API server rather than only through the
// controller's Go validator.
var _ = Describe("CommitRequest message schema", func() {
	const namespace = "default"

	newRequest := func(message string) *configbutleraiv1alpha3.CommitRequest {
		return &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "message-schema-",
				Namespace:    namespace,
			},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "team-a-config"},
				Message:      message,
			},
		}
	}

	DescribeTable("rejects a message that carries no non-whitespace character",
		func(message string) {
			err := k8sClient.Create(ctx, newRequest(message))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("non-whitespace"))
		},
		Entry("a single space", " "),
		Entry("spaces and newlines", "  \n \n  "),
		Entry("a no-break space", "\u00a0"),
		Entry("an ideographic space", "\u3000"),
		Entry("a next line character", "\u0085"),
		Entry("an en quad", "\u2000"),
		Entry("a narrow no-break space", "\u202f"),
	)

	DescribeTable("accepts a message carrying real text",
		func(message string) {
			Expect(k8sClient.Create(ctx, newRequest(message))).To(Succeed())
		},
		Entry("a plain subject", "fix(api): correct the service port"),
		Entry("a subject and body", "fix(api): correct the port\n\nRoute traffic to the container port."),
		Entry("surrounding whitespace around real text", "  spaced  "),
		Entry("template-like text stays literal", "{{.Author}} changed the port"),
		Entry("non-ASCII text", "fix: träge Antwortzeiten"),
	)
})
