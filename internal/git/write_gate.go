// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
)

// The write gates are the two reasons a GitTarget may not be written at all: spec.suspend, and a
// render-fidelity gate that has not established render-vs-live for it. They are rules about the
// TARGET, and every path that decides a CommitRequest's outcome asks targetWriteRefusal before it
// reports one, so a save on a target that refuses writes fails with the reason instead of resolving
// Ready=True with nothing written. That holds whichever path ends the request and whatever its
// whenNothingToCommit says:
//
//   - a request no window reached (expireWaitingCommitRequests, before the record it may make);
//   - the record itself (buildRequestRecordWrite);
//   - a window a request is attached to (finalizeOpenWindowWithReason before it builds the write,
//     and gateBuiltWindow after, right before the commit). A render-fidelity refusal seen at
//     either look stops the whole window; suspension fails only the request, and the write still
//     runs so the target's scan stays fresh.
//
// A WindowMismatch is not asked: that request was refused by another author's window, which is
// what it reports. Neither is a request whose write is already decided. The gates are read when the
// write is decided (the window closes), which is normally when it is committed, as
// ResolvedTargetMetadata.Suspend already defined for suspension. A write decided before a gate
// closed is still committed and pushed, even when an unreachable remote delays its commit: reading
// the gates later would strand it in the worker, to resurface out of order when the gate opens.
//
// They are deliberately not the checkout's state (baseTrusted, checkoutApplied). That says whether
// the worker can commit on its checkout; these say whether the target may be written at all, and
// no reset or replay changes the answer.

var (
	// errTargetSuspended is a request's failure on a suspended GitTarget, which writes nothing.
	errTargetSuspended = errors.New("the GitTarget is suspended")
	// errRenderFidelityClosed is a request's failure while the GitTarget's render-vs-live check has
	// not passed. The gate drops the target's writes on arrival, so the save cannot know what it
	// would have contained.
	errRenderFidelityClosed = errors.New(
		"the GitTarget's render fidelity is not established, so its writes are held back")
)

// targetWriteRefusal returns why a target may not be written, or nil when it may. suspended is
// the caller's reading of spec.suspend: the value captured with a planned write, or the live one
// for a request no write has planned.
func (w *BranchWorker) targetWriteRefusal(name, namespace string, suspended bool) error {
	if suspended {
		return errTargetSuspended
	}
	if !w.normalWritesAllowed(name, namespace) {
		return errRenderFidelityClosed
	}
	return nil
}

// requestWriteRefusal is targetWriteRefusal for a request no write has planned yet, reading
// spec.suspend from the live GitTarget. Only evidence refuses: a target that cannot be read is
// left to the path that writes, which fails on it, and a request that writes nothing (Resolve)
// keeps its answer.
func (w *BranchWorker) requestWriteRefusal(ctx context.Context, pcr *pendingCommitRequest) error {
	suspended := false
	if target, err := w.getGitTarget(ctx, pcr.gitTargetName, pcr.gitTargetNamespace); err == nil {
		suspended = target.Spec.Suspend
	}
	return w.targetWriteRefusal(pcr.gitTargetName, pcr.gitTargetNamespace, suspended)
}
