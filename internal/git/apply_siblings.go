// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// A branch worker is keyed by its GitProvider, so two GitProviders naming one repository give one
// branch two workers. Their appliers can apply one GitRepository, and a source group has one
// operation at a time (apply_groups.go), so the workers claim the group before they send.
// branchSiblings is what lets each see the other's claims.

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

// remove forgets a worker that is stopping, and wakes the others: the groups it claimed are released.
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

// wakeOthers wakes every other worker on w's repository and branch, so one that held back an
// attempt for a group w had out looks again.
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

// claimGroup records that w sends an attempt for an applier of group, and reports false, recording
// nothing, when another worker on its repository and branch has one out for that group. Checking
// and recording under one lock is what keeps two workers from sending for one group at once.
func (s *branchSiblings) claimGroup(w *BranchWorker, group string) bool {
	key, ok := siblingKey(w)
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groupOutLocked(key, w, group) {
		return false
	}
	if w.groupsOut == nil {
		w.groupsOut = map[string]struct{}{}
	}
	w.groupsOut[group] = struct{}{}
	return true
}

// groupOutElsewhere reports whether another worker on w's repository and branch has an attempt out
// for an applier of group.
func (s *branchSiblings) groupOutElsewhere(w *BranchWorker, group string) bool {
	key, ok := siblingKey(w)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupOutLocked(key, w, group)
}

func (s *branchSiblings) groupOutLocked(key string, w *BranchWorker, group string) bool {
	for sibling := range s.byKey[key] {
		if _, out := sibling.groupsOut[group]; out && sibling != w {
			return true
		}
	}
	return false
}

// publishGroupsOut replaces the groups w has attempts out for, and wakes the others when one of them
// is no longer out: they may have held an attempt back for it.
func (s *branchSiblings) publishGroupsOut(w *BranchWorker, groups map[string]struct{}) {
	if _, ok := siblingKey(w); !ok {
		return
	}
	s.mu.Lock()
	released := false
	for group := range w.groupsOut {
		if _, out := groups[group]; !out {
			released = true
		}
	}
	w.groupsOut = groups
	s.mu.Unlock()
	if released {
		s.wakeOthers(w)
	}
}

// claimGroup records that this worker sends an attempt for an applier of group, unless a sibling
// has one out for it (branchSiblings.claimGroup). An applier in no group claims nothing.
func (w *BranchWorker) claimGroup(group string) bool {
	return group == "" || w.siblings == nil || w.siblings.claimGroup(w, group)
}

// groupOutOnSibling reports whether a sibling has an attempt out for an applier of group.
func (w *BranchWorker) groupOutOnSibling(group string) bool {
	return group != "" && w.siblings != nil && w.siblings.groupOutElsewhere(w, group)
}

// releaseInflightItem takes one item off the in-flight count, which the queue-depth gauge reads.
func (w *BranchWorker) releaseInflightItem() {
	w.inflightItems.Add(-1)
}
