// SPDX-License-Identifier: Apache-2.0

package git

// CommitRequestChange names a CommitRequest whose standing on its branch worker changed: it was
// registered, attached, committed or resolved, or its apply moved. It carries only the request's
// identity: the controller reads the standing from the worker, as it always does, so a change that is
// dropped or arrives twice costs at most a step.
type CommitRequestChange struct {
	Namespace string
	Name      string
	UID       string
}

// commitRequestChangeBuffer bounds the changes a burst can queue. A full buffer drops the arriving
// change: the controller's fallback poll reads the request later.
const commitRequestChangeBuffer = 256

// CommitRequestChanges carries each change of a CommitRequest's standing on a branch worker, so the
// controller takes the request's next step at once instead of at its next poll. Nil for a manager
// not built by NewWorkerManager.
func (m *WorkerManager) CommitRequestChanges() <-chan CommitRequestChange {
	return m.commitRequestChanges
}

// notifyCommitRequestChange queues a change, and never blocks: the worker's loop calls it, holding
// the lock over its outcome tables.
func (m *WorkerManager) notifyCommitRequestChange(change CommitRequestChange) {
	if m.commitRequestChanges == nil {
		return
	}
	select {
	case m.commitRequestChanges <- change:
	default:
	}
}

// reportCommitRequestChange tells the manager that the request's standing changed.
func (w *BranchWorker) reportCommitRequestChange(id commitRequestID) {
	if w.commitRequestReporter != nil {
		w.commitRequestReporter(CommitRequestChange(id))
	}
}
