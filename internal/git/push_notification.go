// SPDX-License-Identifier: Apache-2.0

package git

// EnqueuePushNotification queues a Git host's report that the branch was pushed to.
//
// The report is a hint and never a fact: after never decides whether the branch moved. It is queued
// as a refresh
// that reaches the remote whatever was proved recently, so the worker reads the advertisement itself
// and the observation it records decides whether the branch moved (applyInbox.observe). That one
// rule settles every awkward delivery without bookkeeping: a late delivery for an earlier push of
// this worker's finds the branch at the worker's latest push, a rollback onto an older one of its
// commits finds a tip it did not push last, and a duplicate costs one more advertisement.
//
// On a busy branch the refresh reads nothing (handleRefreshRequest): the worker closes its open
// window instead and publishes, and the compare-and-swap push discovers the move. The one thing after
// is used for is to NOT close a window: a delivery naming the tip the worker last observed is, almost
// always, the late report of the worker's own push, and closing a save's window on it would split
// that save across two commits. That direction is safe even for a wrong after: a window left open
// still closes on its timers, and its push finds any move by the compare-and-swap.
func (w *BranchWorker) EnqueuePushNotification(after string) {
	w.Log.V(1).Info("Git host reported a push; checking where the branch is", "branch", w.Branch, "after", after)
	w.EnqueueRefresh(&RefreshRequest{PushNotice: true, After: after})
}
