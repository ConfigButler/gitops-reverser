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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventuallyConditionStatus blocks until the object at key publishes a condition of condType
// with the wanted status, then returns the matched condition for any further assertions
// (reason, message). getConditions extracts the freshly-fetched object's Status.Conditions —
// our CRD status structs don't share a conditions accessor, so the caller supplies a closure
// over its typed object.
//
// This is the unit-test analog of the e2e verifyResourceCondition helper: it removes the
// create→async-reconcile race that bites specs which read a *dependency's* published status
// (e.g. a WatchRule mirroring its referenced GitTarget's Ready condition). A single synchronous
// Reconcile would otherwise observe an as-yet-unpopulated status.
func eventuallyConditionStatus(
	ctx context.Context,
	key types.NamespacedName,
	obj client.Object,
	getConditions func() []metav1.Condition,
	condType string,
	want metav1.ConditionStatus,
) metav1.Condition {
	GinkgoHelper()
	var matched metav1.Condition
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		cond := meta.FindStatusCondition(getConditions(), condType)
		g.Expect(cond).NotTo(BeNil(), "condition %q not published yet on %s", condType, key)
		g.Expect(cond.Status).To(Equal(want))
		matched = *cond
	}).Should(Succeed())
	return matched
}
