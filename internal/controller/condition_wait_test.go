// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// conditionWaitTimeout bounds every condition wait in this suite. Gomega's built-in default is 1s,
// which is not a wait for an ASYNC controller at all: the reconciler has to be triggered, run, and
// write a status subresource before the first condition even exists, and under a loaded envtest
// that regularly takes longer. A 1s ceiling made these waits pass or fail on machine speed —
// exactly the flake this suite kept seeing. Match the 10s the other specs already use.
const (
	conditionWaitTimeout = 10 * time.Second
	conditionWaitPolling = 100 * time.Millisecond
)

// eventuallyConditionStatusReason blocks until the object at key publishes a condition of condType
// that has settled on BOTH the wanted status and the wanted reason, then returns it for any further
// assertions (message). getConditions extracts the freshly-fetched object's Status.Conditions — our
// CRD status structs don't share a conditions accessor, so the caller supplies a closure over its
// typed object.
//
// This is the unit-test analog of the e2e verifyResourceCondition helper: it removes the
// create→async-reconcile race that bites specs which read a *dependency's* published status (e.g. a
// WatchRule mirroring its referenced GitTarget's Ready condition). A single synchronous Reconcile
// would otherwise observe an as-yet-unpopulated status.
//
// The REASON is part of the wait on purpose. Waiting on status alone and then asserting the reason
// separately asserts a value that is still moving: a controller can publish the target status with
// a transient reason (a dependency it has not observed yet) and settle on the final reason a moment
// later. Folding it in makes the poll the assertion.
func eventuallyConditionStatusReason(
	ctx context.Context,
	key types.NamespacedName,
	obj client.Object,
	getConditions func() []metav1.Condition,
	condType string,
	wantStatus metav1.ConditionStatus,
	wantReason string,
) metav1.Condition {
	GinkgoHelper()
	var matched metav1.Condition
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		cond := meta.FindStatusCondition(getConditions(), condType)
		g.Expect(cond).NotTo(BeNil(), "condition %q not published yet on %s", condType, key)
		g.Expect(cond.Status).To(Equal(wantStatus))
		g.Expect(cond.Reason).To(Equal(wantReason),
			"condition %q on %s is %s but has not settled on reason %q yet",
			condType, key, wantStatus, wantReason)
		matched = *cond
	}).WithTimeout(conditionWaitTimeout).WithPolling(conditionWaitPolling).Should(Succeed())
	return matched
}
