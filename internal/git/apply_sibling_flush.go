// SPDX-License-Identifier: Apache-2.0

package git

// Flush, then apply (docs/design/applier-echoes.md, S1), across the workers writing one branch. Two
// GitProviders naming one repository give its branch two workers, and their GitTargets can name one
// trigger. An apply one of them sends writes the commit it was sent for over every object the applier
// owns, so a sibling's edit not published by then is reverted on the cluster, and the sibling, which
// sent nothing and has no apply window, mirrors the revert as an edit of its own.
//
// So the flush covers the siblings: when an apply comes due, the worker asks each sibling to flush,
// and each closes its open window, takes a mark of its own, and publishes up to it at once. The
// worker sends once every sibling it asked is done, for the tip their publications produced, which
// they shared with it (shareObservation). As in S1, a sibling's writes decided after its mark do not
// hold the send; an edit a sibling makes while the apply runs is not protected, as before.

// askFlush asks every other worker on w's repository and branch to flush, and returns the request's
// generation, zero when w has no sibling. A sibling asked a later generation has flushed for this
// one too, because its mark is taken after this one was asked.
func (s *branchSiblings) askFlush(w *BranchWorker) uint64 {
	key, ok := siblingKey(w)
	if s == nil || !ok {
		return 0
	}
	s.mu.Lock()
	var others []*BranchWorker
	for sibling := range s.byKey[key] {
		if sibling != w {
			others = append(others, sibling)
		}
	}
	if len(others) == 0 {
		s.mu.Unlock()
		return 0
	}
	s.flushGen++
	gen := s.flushGen
	for _, sibling := range others {
		sibling.flushAsked.Store(max(sibling.flushAsked.Load(), gen))
	}
	s.mu.Unlock()
	for _, sibling := range others {
		sibling.wakeApplier()
	}
	return gen
}

// othersFlushed reports whether every other worker on w's repository and branch that was asked for
// gen has flushed for it. A worker that joined after the request was never asked, and a worker that
// stopped is not waited for: what it held is published or gone with it.
func (s *branchSiblings) othersFlushed(w *BranchWorker, gen uint64) bool {
	key, ok := siblingKey(w)
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sibling := range s.byKey[key] {
		if sibling != w && sibling.flushAsked.Load() >= gen && sibling.flushDone.Load() < gen {
			return false
		}
	}
	return true
}

// takeSiblingFlush starts a flush a sibling asked for: the open window is closed, and the mark is the
// last write decided so far. It runs at the end of every wake, before the wake's writes are advanced,
// so they are published at once (publishesAtOnce). A save, a parent recovery or a dirty checkout
// hold it, as they hold the worker's own flush; the asking sibling waits for them.
func (l *branchWorkerEventLoop) takeSiblingFlush() {
	asked := l.w.flushAsked.Load()
	if asked <= l.siblingFlushGen || l.writeBusy() {
		return
	}
	l.finalizeOpenWindowWithReason(windowFinalizeReasonApplyDue)
	l.siblingFlushGen, l.siblingFlushMark = asked, l.decisions
}

// reportSiblingFlush says the sibling flush taken is done once every write up to its mark is
// published, and wakes the siblings, so the one waiting sends.
func (l *branchWorkerEventLoop) reportSiblingFlush() {
	if l.siblingFlushGen <= l.w.flushDone.Load() || l.pendingUpTo(l.siblingFlushMark) {
		return
	}
	l.w.flushDone.Store(l.siblingFlushGen)
	if l.w.siblings != nil {
		l.w.siblings.wakeOthers(l.w)
	}
}

// siblingsFlushed reports whether the siblings asked when this worker's flush began have flushed.
// Their publications moved the branch, and each one's observation of its push reached this worker's
// inbox (shareObservation) before it said it was done, so the tip the attempt is sent for holds them.
func (l *branchWorkerEventLoop) siblingsFlushed() bool {
	return l.siblingAsk == 0 || l.w.siblings.othersFlushed(l.w, l.siblingAsk)
}

// shareObservation tells every other worker on w's repository and branch what w observed of the
// branch, as their own inboxes classify it: w's push moved the tip with content that is live already,
// as a worker's own push does, and asks nothing; what w fetched, nobody on the branch wrote. Without
// it a worker learns of a sibling's push only from a fetch of its own, which takes it for somebody
// else's, and sends an attempt for a tip the sibling has moved.
func (s *branchSiblings) shareObservation(w *BranchWorker, observed RemoteObservation) {
	key, ok := siblingKey(w)
	if s == nil || !ok {
		return
	}
	s.mu.Lock()
	others := make([]*BranchWorker, 0, len(s.byKey[key]))
	for sibling := range s.byKey[key] {
		if sibling != w {
			others = append(others, sibling)
		}
	}
	s.mu.Unlock()
	for _, sibling := range others {
		sibling.inbox.observe(observed)
	}
}

// publishesAtOnce reports whether a write the pending writes hold is waited for by a flush, this
// worker's or a sibling's: it is pushed at once, cooldown or not.
func (l *branchWorkerEventLoop) publishesAtOnce() bool {
	return (l.flushing && l.pendingUpTo(l.flushMark)) ||
		(l.siblingFlushGen > l.w.flushDone.Load() && l.pendingUpTo(l.siblingFlushMark))
}
