// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// branchPublication is the branch worker's publication report: whether it can publish to the Git
// remote, and if not, since when and why. Every GitTarget on the branch reads the same report,
// because they share the worker: a sibling's success cannot clear it, and only a landed push does.
// It reads memory only, and costs no connection.
func (r *GitTargetReconciler) branchPublication(
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) git.PublicationStatus {
	if r.WorkerManager == nil {
		return git.PublicationStatus{}
	}
	worker, ok := r.WorkerManager.GetWorkerForTarget(target.Spec.GitProviderRef.Name, providerNS, target.Spec.Branch)
	if !ok {
		return git.PublicationStatus{}
	}
	return worker.Publication()
}

// publicationCondition is the gate a publication report contributes: progress while a failed
// attempt waits for its retry, which needs nobody. A missing parent branch is left to the parent
// gate, which says which branch to create. The message changes only when the report does, so a
// retry that fails the same way again writes no status.
func publicationCondition(report git.PublicationStatus) conditionValue {
	if !report.Failing || report.ParentUnavailable {
		return conditionValue{Status: metav1.ConditionTrue, Reason: GitTargetReasonOK}
	}
	return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonProgressing, Message: report.Message()}
}
