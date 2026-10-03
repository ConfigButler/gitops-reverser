// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// GitTargetReasonRecoveringParentBranch is the Ready reason while the parent branch has been found
// again but the branch worker has not yet published the writes it kept while the parent was
// missing. It is progress, not a stall.
const GitTargetReasonRecoveringParentBranch = "RecoveringParentBranch"

// parentRecovering reports whether the branch worker has found the parent branch again and still
// owes the writes it kept while the parent was missing. It reads memory only, and costs no
// connection.
func (r *GitTargetReconciler) parentRecovering(
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) bool {
	if r.WorkerManager == nil {
		return false
	}
	worker, ok := r.WorkerManager.GetWorkerForTarget(target.Spec.GitProviderRef.Name, providerNS, target.Spec.Branch)
	if !ok {
		return false
	}
	open, found := worker.ParentRecovery()
	return open && found
}

// parentRecoveryReadiness is the progress gate for a target whose parent is back but whose
// held-back work is not published yet.
func parentRecoveryReadiness(rd *readiness, recovering bool) {
	rd.progressingIf(recovering, metav1.ConditionFalse, GitTargetReasonRecoveringParentBranch,
		"The parent branch is on the remote again; the branch worker is publishing the work it held back")
}
