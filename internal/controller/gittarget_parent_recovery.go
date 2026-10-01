// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// GitTargetReasonRecoveringParentBranch is the Ready reason while the parent branch has been found
// again but the branch worker has not yet published the work it held back. It is progress, not a
// stall, and keeps the not-Ready requeue cadence so the controller acts on the worker's snapshot
// requests within one requeue.
const GitTargetReasonRecoveringParentBranch = "RecoveringParentBranch"

// snapshotRequestTracker remembers, per GitTarget, the last snapshot request of its branch worker
// that this reconciler acted on. A missing-parent episode that starts and ends between two
// reconciles still leaves the worker's sequence higher, so it is never lost; an unchanged sequence
// forces nothing. Its memory is empty after a restart, which is harmless: every stream starts with
// an initial list, which is itself the snapshot. Its zero value is usable.
type snapshotRequestTracker struct {
	mu    sync.Mutex
	acted map[string]uint64
}

// take reports whether seq is a request not acted on yet, and records it as acted on.
func (t *snapshotRequestTracker) take(ref types.ResourceReference, seq uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq == 0 || t.acted[ref.Key()] >= seq {
		return false
	}
	if t.acted == nil {
		t.acted = map[string]uint64{}
	}
	t.acted[ref.Key()] = seq
	return true
}

// forget drops a deleted object's record.
func (t *snapshotRequestTracker) forget(ref types.ResourceReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.acted, ref.Key())
}

// parentRecovery reads the branch worker's obligation for work a missing parent held back. The
// first result is whether it asks for a new snapshot of this target (true once per rise of its
// sequence), the second whether the parent is back while work is still owed. It reads memory only,
// and costs no connection.
func (r *GitTargetReconciler) parentRecovery(
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (bool, bool) {
	if r.WorkerManager == nil {
		return false, false
	}
	worker, ok := r.WorkerManager.GetWorkerForTarget(target.Spec.GitProviderRef.Name, providerNS, target.Spec.Branch)
	if !ok {
		return false, false
	}
	ref := types.NewResourceReference(target.Name, target.Namespace)
	open, found := worker.ParentRecovery()
	return r.snapshotRequests.take(ref, worker.SnapshotRequestSeq(ref)), open && found
}

// parentRecoveryReadiness is the progress gate for a target whose parent is back but whose
// held-back work is not published yet.
func parentRecoveryReadiness(rd *readiness, recovering bool) {
	rd.progressingIf(recovering, metav1.ConditionFalse, GitTargetReasonRecoveringParentBranch,
		"The parent branch is on the remote again; the branch worker is publishing the work it held back")
}
