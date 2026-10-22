// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sync"
	"time"
)

// EnqueuePushNotification queues a Git host's report that the branch was pushed to.
//
// The report is a hint and never a fact: after never decides whether the branch moved. It is queued
// as a refresh that reaches the remote whatever was proved recently, so the worker reads the
// advertisement itself and the observation it records decides whether the branch moved
// (applyInbox.observe). That one rule settles every awkward delivery without bookkeeping: a late
// delivery for an earlier push of this worker's finds the branch at the worker's latest push, a
// rollback onto an older one of its commits finds a tip it did not push last, and a duplicate costs
// at most one more advertisement.
//
// At most one notice waits on the queue. A notice reported while one is waiting is folded into it,
// because the look the waiting one makes reads the remote after both pushes: a run of deliveries,
// from a busy repository or from whoever holds a receiver token, costs this worker one
// advertisement at a time. The waiting notice is taken off the fold when it is dispatched, before
// the remote is read, so a push reported during that read queues a look of its own.
//
// On a busy branch the refresh reads nothing (handleRefreshRequest): the worker closes its open
// window instead and publishes, and the compare-and-swap push discovers the move. The one thing after
// is used for is to NOT close a window: a delivery naming the tip the worker last observed is, almost
// always, the late report of the worker's own push, and closing a save's window on it would split
// that save across two commits. That direction is safe even for a wrong after: a window left open
// still closes on its timers, and its push finds any move by the compare-and-swap.
func (w *BranchWorker) EnqueuePushNotification(after string) {
	w.lastPushNoticeAt.Store(time.Now().UnixNano())
	if !w.pushNotices.offer(after) {
		w.Log.V(1).Info("Git host reported a push; a look is already queued", "branch", w.Branch, "after", after)
		return
	}
	w.Log.V(1).Info("Git host reported a push; checking where the branch is", "branch", w.Branch, "after", after)
	if !w.EnqueueRefresh(&RefreshRequest{PushNotice: true, After: after}) {
		// Nothing is waiting after all, so the next report must be free to queue.
		w.pushNotices.take()
	}
}

// LastPushNotice is when a Git host last reported a push to this branch, zero when none has since
// this worker started.
func (w *BranchWorker) LastPushNotice() time.Time {
	at := w.lastPushNoticeAt.Load()
	if at == 0 {
		return time.Time{}
	}
	return time.Unix(0, at)
}

// pendingPushNotice is the one push notice a worker may have waiting on its queue.
type pendingPushNotice struct {
	mu     sync.Mutex
	queued bool
	// after is what the waiting notice names: the commit every folded report agreed on, or empty
	// once two disagreed. Empty is the safe side: an open window treats it as a push it has not seen
	// (noteBusyPushNotice), as it would have treated at least one of the reports folded into it.
	after string
}

// offer records a report naming after, and reports whether the caller has to queue a notice for it:
// false when one is already waiting, which then stands for this report too.
func (p *pendingPushNotice) offer(after string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued {
		if p.after != after {
			p.after = ""
		}
		return false
	}
	p.queued, p.after = true, after
	return true
}

// take empties the slot and returns what the waiting notice names, and whether one was waiting.
func (p *pendingPushNotice) take() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	after, queued := p.after, p.queued
	p.queued, p.after = false, ""
	return after, queued
}
