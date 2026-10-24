// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"

	gogit "github.com/go-git/go-git/v6"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the barrier an apply waits for (S2 in docs/design/applier-echoes.md), and what it says
// while it holds one. No applier is asked for anything, for any reason, until:
//
//   - the GitTargets of a worker that replaced another were told (WorkerManager.ReplacementPending):
//     until then their record of settled snapshots is the old worker's;
//   - every GitTarget the applier covers has had a snapshot of every collection it watches settled
//     and published (initialSnapshotsPublished). One that watches no collection holds its applier,
//     as a blocked watch does: nothing is mirrored into its folder yet;
//   - the folder of each of those GitTargets is on the branch. A GitTarget that names an applier
//     claim has its folder created by its resync even with no object, but one that named its claim
//     after its snapshots settled empty has none, and Argo CD retries a sync of a missing path for
//     minutes while Flux fails it. A fresh snapshot of it is asked for once, and its resync creates
//     the folder.
//
// The hold is approved; the silence was the defect. Every applier the barrier holds a reason back
// for is logged at Info when the hold begins, changes reason or lifts, and counted in the
// appliers_held gauge.

// barrierHold says why the barrier holds the applier back, leaving out its folders
// (missingFolders), or "" when it does not.
func (l *branchWorkerEventLoop) barrierHold(state *applierState) string {
	if l.w.replacementPending != nil && l.w.replacementPending() {
		return "this branch worker replaced one that wrote another repository, and the GitTargets on the " +
			"branch have not been re-established yet"
	}
	for _, target := range state.targets {
		if l.w.initialSnapshots != nil && !l.w.initialSnapshots(target.ref) {
			return fmt.Sprintf("GitTarget %s has not had a snapshot of every collection it watches settled; one "+
				"that watches no collection holds its applier until it watches one", target.ref)
		}
		if !l.initialSnapshotsPublished(target.ref) {
			return fmt.Sprintf("a snapshot of GitTarget %s is not published yet", target.ref)
		}
	}
	return ""
}

// awaitsFolder reports whether a GitTarget the applier covers is waiting for its folder
// (missingFolders).
func (l *branchWorkerEventLoop) awaitsFolder(state *applierState) bool {
	for _, target := range state.targets {
		if l.awaitingFolders[target.ref] {
			return true
		}
	}
	return false
}

// missingFolders says which folder of a GitTarget the applier covers the branch does not hold, or ""
// when it holds every one. A missing folder's GitTarget is asked for a fresh snapshot once, and waits
// for it (awaitingFolders); its snapshot is then used whatever is ahead (snapshotsWait), since the
// apply held for it would wait on the snapshot. A branch that cannot be read is no reason to hold.
func (l *branchWorkerEventLoop) missingFolders(state *applierState) string {
	var repo *gogit.Repository
	missing := ""
	for _, target := range state.targets {
		if target.folder == "" {
			continue
		}
		if repo == nil {
			opened, err := gogit.PlainOpen(l.w.repoPath())
			if err != nil {
				return ""
			}
			repo = opened
		}
		if headHasDir(repo, target.folder) {
			delete(l.awaitingFolders, target.ref)
			continue
		}
		if missing == "" {
			missing = fmt.Sprintf("the folder %s of GitTarget %s is not on the branch; a fresh snapshot of the "+
				"GitTarget, whose resync creates it, was asked for", target.folder, target.ref)
		}
		if l.awaitingFolders[target.ref] {
			continue
		}
		if l.awaitingFolders == nil {
			l.awaitingFolders = map[itypes.ResourceReference]bool{}
		}
		l.awaitingFolders[target.ref] = true
		if l.w.resyncRecheck != nil {
			l.w.resyncRecheck(target.ref)
		}
	}
	return missing
}

// forgetFolders drops the folders awaited for GitTargets no longer on the branch.
func (l *branchWorkerEventLoop) forgetFolders(listed map[itypes.ResourceReference]bool) {
	for ref := range l.awaitingFolders {
		if !listed[ref] {
			delete(l.awaitingFolders, ref)
		}
	}
}

// noteHeld records why the barrier holds the applier back now, "" for nothing, and logs a change.
func (l *branchWorkerEventLoop) noteHeld(state *applierState, held string) {
	if held == state.held {
		return
	}
	if held == "" {
		l.w.Log.Info("The barrier no longer holds the applier back", "branch", l.w.Branch,
			"applier", state.ref.String(), "was", state.held)
	} else {
		l.w.Log.Info("An apply is held back: no applier is asked for anything before the GitTargets it covers "+
			"are mirrored", "branch", l.w.Branch, "applier", state.ref.String(), "reason", held)
	}
	state.held = held
}

// publishHeld publishes how many of this branch's appliers the barrier holds a reason back for, for
// the appliers_held gauge.
func (l *branchWorkerEventLoop) publishHeld() {
	var held int64
	for _, state := range l.appliers {
		if state.held != "" {
			held++
		}
	}
	l.w.heldAppliers.Store(held)
}
