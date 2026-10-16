// SPDX-License-Identifier: Apache-2.0

package git

import (
	"slices"
	"time"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ApplyRequest is one request, decided by a branch worker, that the appliers of some GitTargets on
// its branch apply a commit. The worker decides WHEN; delivering it is the ApplyRequester's job, off
// the worker's queue, so a slow applier never holds the branch.
type ApplyRequest struct {
	// Branch is the branch the request is about.
	Branch BranchKey
	// Reason is why the appliers are asked.
	Reason v1alpha3.ApplyReason
	// Commit is the commit to apply: the branch tip when the worker decided, which the worker has
	// pushed or proved by a fetch. Empty when the remote does not carry the branch.
	Commit string
	// Targets are the GitTargets whose appliers are asked. Several may name one applier; the
	// requester asks it once.
	Targets []itypes.ResourceReference
	// HandBacks is how many times a request for these targets has already been handed back
	// undelivered (ApplyHandBack). Zero for a request the worker decided fresh. The worker's
	// backoff grows with it, and the requester reports a failure only for a request at zero, so a
	// standing failure is reported once rather than on every retry.
	HandBacks int
}

// StrongerApplyReason is the reason a target owed two requests is owed. A CatchUp asks for the
// least: it stamps nothing when the artifact already holds the commit, where the others stamp the
// Kustomization, so it never replaces either. A ForeignCommit outranks a Refusal because it needs
// no consent. The empty reason ranks below all three.
func StrongerApplyReason(a, b v1alpha3.ApplyReason) v1alpha3.ApplyReason {
	weakestFirst := []v1alpha3.ApplyReason{
		"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit,
	}
	if slices.Index(weakestFirst, b) > slices.Index(weakestFirst, a) {
		return b
	}
	return a
}

// ApplyRequester delivers apply requests. RequestApply must not block: it is called on the branch
// worker's loop goroutine.
type ApplyRequester interface {
	RequestApply(req ApplyRequest)
}

// ApplyHandBack takes back a request the requester could not deliver, so the worker owes it again.
//
// The requester retries a failed request only briefly. A retry is the worker's decision replayed
// without the quiet check: the worker decided it on a quiet branch, and by the time a long retry
// lands, live edits may be waiting in an open window that the apply would revert. So the requester
// hands a request back once it has failed for about one quiet period, and the worker sends it
// again only when the branch is quiet once more. HandBackApply must not block.
type ApplyHandBack interface {
	HandBackApply(req ApplyRequest)
}

// handBackDelay is how long the worker waits before sending a request that has been handed back
// handBacks times: doubling from handBackInitialDelay, capped at handBackMaxDelay, so a permanent
// failure (a misconfigured Kustomization) is asked about every few minutes rather than in a loop.
func handBackDelay(handBacks int) time.Duration {
	delay := handBackInitialDelay
	for i := 1; i < handBacks && delay < handBackMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, handBackMaxDelay)
}

const (
	handBackInitialDelay = 15 * time.Second
	handBackMaxDelay     = 5 * time.Minute
)

// HandBackApply hands a request back to the worker for its branch. A branch with no worker has
// nothing to owe it to: a worker created later starts by owing every applier a catch-up.
func (m *WorkerManager) HandBackApply(req ApplyRequest) {
	worker, found := m.GetWorkerForTarget(req.Branch.RepoName, req.Branch.RepoNamespace, req.Branch.Branch)
	if !found {
		return
	}
	worker.handBackApply(req)
}

// handBackApply records a handed-back request in the debt and wakes the loop, which sends it
// again once it is due and the branch is quiet (serviceAppliers). The record is the debt, not the
// wake: a wake dropped on a full queue costs nothing, because the work already queued wakes the
// loop, and every wake ends by servicing the appliers.
func (w *BranchWorker) handBackApply(req ApplyRequest) {
	w.debt.handBack(req, time.Now())
	w.Log.Info("An apply request could not be delivered; owing it again",
		"branch", w.Branch, "reason", string(req.Reason), "gitTargets", len(req.Targets),
		"handBacks", req.HandBacks, "retryIn", handBackDelay(req.HandBacks))
	w.inflightItems.Add(1)
	w.pendingResyncsMu.Lock()
	defer w.pendingResyncsMu.Unlock()
	if w.stoppingLocked() {
		w.inflightItems.Add(-1)
		return
	}
	select {
	case w.eventQueue <- WorkItem{ApplyWake: true}:
	default:
		w.inflightItems.Add(-1)
	}
}
