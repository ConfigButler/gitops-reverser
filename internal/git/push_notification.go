// SPDX-License-Identifier: Apache-2.0

package git

// EnqueuePushNotification queues a Git host's report that the branch was pushed to.
//
// The report is a hint and never a fact: after is logged and nothing else. It is queued as a refresh
// that reaches the remote whatever was proved recently, so the worker reads the advertisement itself
// and the observation it records decides whether the branch moved (applyDebt.observe). That one
// rule settles every awkward delivery without bookkeeping: a late delivery for an earlier push of
// this worker's finds the branch at the worker's latest push, a rollback onto an older one of its
// commits finds a tip it did not push last, and a duplicate costs one more advertisement.
//
// On a busy branch the refresh reads nothing (handleRefreshRequest): the worker closes its open
// window instead and publishes, and the compare-and-swap push discovers the move.
func (w *BranchWorker) EnqueuePushNotification(after string) {
	w.Log.V(1).Info("Git host reported a push; checking where the branch is", "branch", w.Branch, "after", after)
	w.EnqueueRefresh(&RefreshRequest{PushNotice: true})
}
