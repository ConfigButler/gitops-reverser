// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// A branch worker is keyed by its GitProvider, so two GitProviders naming one repository give one
// branch two workers. A Kustomization applies the tip of that branch whichever of them wrote it, so
// the branch is quiet only when BOTH are: an apply one worker asks for while the other holds an open
// window would revert that window's edits before they reach Git. branchSiblings is what lets each
// see the other.

// branchSiblings groups the live workers by the repository and branch they write.
type branchSiblings struct {
	mu    sync.Mutex
	byKey map[string]map[*BranchWorker]struct{}
}

func newBranchSiblings() *branchSiblings {
	return &branchSiblings{byKey: map[string]map[*BranchWorker]struct{}{}}
}

// siblingKey is the repository, compared as repourl does, and the branch. A worker that knows no
// repository (the CLI, tests) has no siblings.
func siblingKey(w *BranchWorker) (string, bool) {
	if w.repo.URL == "" {
		return "", false
	}
	return repourl.Normalize(w.repo.URL) + "\x00" + w.Branch, true
}

// add registers a worker that is about to start. A nil registry (a WorkerManager built by hand in a
// test) registers nothing.
func (s *branchSiblings) add(w *BranchWorker) {
	key, ok := siblingKey(w)
	if s == nil || !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey[key] == nil {
		s.byKey[key] = map[*BranchWorker]struct{}{}
	}
	s.byKey[key][w] = struct{}{}
	w.siblings = s
}

// remove forgets a worker that is stopping, and wakes the others: whatever it held busy is gone.
func (s *branchSiblings) remove(w *BranchWorker) {
	key, ok := siblingKey(w)
	if s == nil || !ok {
		return
	}
	s.mu.Lock()
	delete(s.byKey[key], w)
	if len(s.byKey[key]) == 0 {
		delete(s.byKey, key)
	}
	s.mu.Unlock()
	s.wakeOthers(w)
}

// othersBusy reports whether another worker on w's repository and branch is busy writing, and
// whether one has work items queued or in hand: a queued watch event is an edit that is live and
// not yet decided, and the item a loop is handling may be committing and pushing before the wake
// that publishes it ends.
func (s *branchSiblings) othersBusy(w *BranchWorker) (bool, bool) {
	key, ok := siblingKey(w)
	if !ok {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writing, working := false, false
	for sibling := range s.byKey[key] {
		if sibling != w {
			writing = writing || sibling.busyWriting.Load()
			working = working || sibling.inflightItems.Load() > 0
		}
	}
	return writing, working
}

// wakeOthers wakes every other worker on w's repository and branch, so one that held back an
// attempt because w was busy looks again.
func (s *branchSiblings) wakeOthers(w *BranchWorker) {
	key, ok := siblingKey(w)
	if !ok {
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
		sibling.wakeApplier()
	}
}

// publishWriteBusy records whether this worker holds live writes not yet on the remote, and wakes
// its siblings when it stops: they may have held an attempt back for it.
func (w *BranchWorker) publishWriteBusy(busy bool) {
	if was := w.busyWriting.Swap(busy); was && !busy && w.siblings != nil {
		w.siblings.wakeOthers(w)
	}
}

// releaseInflightItem takes one item off the in-flight count, and wakes the siblings when it was
// the last: they may have held an attempt back for it.
func (w *BranchWorker) releaseInflightItem() {
	if w.inflightItems.Add(-1) == 0 && w.siblings != nil {
		w.siblings.wakeOthers(w)
	}
}

// siblingBusy reports whether another worker on this repository and branch is busy writing, and
// whether one has work items queued or in hand (othersBusy).
func (w *BranchWorker) siblingBusy() (bool, bool) {
	if w.siblings == nil {
		return false, false
	}
	return w.siblings.othersBusy(w)
}
