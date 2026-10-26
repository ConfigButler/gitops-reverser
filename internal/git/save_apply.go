// SPDX-License-Identifier: Apache-2.0

package git

import (
	"time"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// A CommitRequest with catchUp: ImmediatelyAfterPush asks its GitTarget's applier to apply the tip
// as soon as its commit is pushed, and the controller reports how that went in its Applied
// condition. The save's own outcome is decided by the push alone; this is reported beside it and
// never changes it.
//
// The worker keeps, per applier, the saves waiting for an apply. The attempt that goes out carries
// them, so the one that succeeds settles them: any apply sent after a save's push applies a tip
// that holds the save's commit. An attempt that does not succeed gives them back, and the next
// carries them again. A save whose GitTarget names no applier, or that pushed no commit, or whose
// applier stops being named, is settled as not applied.

// Why a save's apply was settled, or why it still waits: the Applied condition's reasons.
const (
	SaveApplyApplying      = "Applying"
	SaveApplyRetrying      = "Retrying"
	SaveApplyApplied       = "Applied"
	SaveApplyNothingPushed = "NothingPushed"
	SaveApplyNoApplier     = "NoApplierClaim"
	SaveApplyApplierGone   = "ApplierGone"
)

// SaveApply is how the apply a CommitRequest asked for stands.
type SaveApply struct {
	// Done is whether it is settled: it changes no more.
	Done bool
	// Applied is whether the applier applied a commit that holds the save's.
	Applied bool
	// Reason is one of the SaveApply* reasons, and Message says more.
	Reason  string
	Message string
	// AppliedRevision is the revision the applier reported applying, once Applied.
	AppliedRevision string
}

type saveApplyEntry struct {
	apply     SaveApply
	updatedAt time.Time
}

// recordSaveApply stores where a save's apply stands, for the controller to read. The event loop is
// the only writer; the entries are collected with the CommitRequest outcomes.
func (w *BranchWorker) recordSaveApply(id commitRequestID, apply SaveApply) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if w.crApplies == nil {
		w.crApplies = map[commitRequestID]saveApplyEntry{}
	}
	w.crApplies[id] = saveApplyEntry{apply: apply, updatedAt: time.Now()}
}

// LookupSaveApply reports where the apply a CommitRequest asked for stands, and false while the
// worker knows nothing of it.
func (w *BranchWorker) LookupSaveApply(namespace, name, uid string) (SaveApply, bool) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	entry, ok := w.crApplies[commitRequestID{Namespace: namespace, Name: name, UID: uid}]
	return entry.apply, ok
}

// collectSaveApplies drops the entries older than the outcome TTL. The caller holds crOutcomesMu.
func (w *BranchWorker) collectSaveApplies(now time.Time) {
	for id, entry := range w.crApplies {
		if now.Sub(entry.updatedAt) > commitRequestOutcomeTTL {
			delete(w.crApplies, id)
		}
	}
}

// catchUpAfterSave is called as a CommitRequest that asked to catch up resolves. With a pushed
// commit, its GitTarget's applier gets a converge when the appliers are next listed, at the end of
// this wake (takeSavedCatchUps); with none, there is nothing to apply.
func (l *branchWorkerEventLoop) catchUpAfterSave(pcr *pendingCommitRequest, commit string) {
	if commit == "" {
		l.w.recordSaveApply(pcr.id, SaveApply{Done: true, Reason: SaveApplyNothingPushed,
			Message: "the request pushed no commit, so there is nothing to apply"})
		return
	}
	if l.savedCatchUps == nil {
		l.savedCatchUps = map[itypes.ResourceReference][]commitRequestID{}
	}
	ref := itypes.NewResourceReference(pcr.gitTargetName, pcr.gitTargetNamespace)
	l.savedCatchUps[ref] = append(l.savedCatchUps[ref], pcr.id)
	l.w.recordSaveApply(pcr.id, SaveApply{Reason: SaveApplyApplying,
		Message: "commit " + commit + " is pushed; its applier is asked to apply it"})
}

// takeSavedCatchUps leaves a converge pending for every applier a pushed CommitRequest asked to catch
// up, and records the request as waiting on it. It was asked for, so it is no lag converge: it does
// not yield to an edit (convergeHolds). A request whose GitTarget names no applier is settled.
func (l *branchWorkerEventLoop) takeSavedCatchUps() {
	for ref, ids := range l.savedCatchUps {
		state := l.applierNaming(ref)
		if state == nil {
			for _, id := range ids {
				l.w.recordSaveApply(id, SaveApply{Done: true, Reason: SaveApplyNoApplier,
					Message: "GitTarget " + ref.String() + " names no applier claim that may be asked"})
			}
			continue
		}
		state.converge, state.lag = true, false
		for _, id := range ids {
			state.waitSave(id)
		}
		l.w.Log.Info("A CommitRequest asked its applier to catch up with the pushed commit",
			"branch", l.w.Branch, "applier", state.ref.String(), "gitTarget", ref.String(), "requests", len(ids))
	}
	l.savedCatchUps = nil
}

// applierNaming is the applier a GitTarget names, as last listed, or nil.
func (l *branchWorkerEventLoop) applierNaming(ref itypes.ResourceReference) *applierState {
	for _, state := range l.appliers {
		for _, target := range state.targets {
			if target.ref == ref {
				return state
			}
		}
	}
	return nil
}

// waitSave records a CommitRequest as waiting for the applier's next apply that succeeds.
func (s *applierState) waitSave(id commitRequestID) {
	if s.waitingSaves == nil {
		s.waitingSaves = map[commitRequestID]bool{}
	}
	s.waitingSaves[id] = true
}

// settleSaves settles every save in saves as applied at revision.
func (l *branchWorkerEventLoop) settleSaves(saves map[commitRequestID]bool, revision string) {
	for id := range saves {
		l.w.recordSaveApply(id, SaveApply{Done: true, Applied: true, Reason: SaveApplyApplied,
			AppliedRevision: revision, Message: "the applier applied " + revision})
	}
}

// retrySaves records the saves an attempt that did not succeed gave back as waiting again, with why.
func (l *branchWorkerEventLoop) retrySaves(saves map[commitRequestID]bool, why string) {
	for id := range saves {
		l.w.recordSaveApply(id, SaveApply{Reason: SaveApplyRetrying,
			Message: "the apply did not complete, and is asked again: " + why})
	}
}

// dropSaves settles the saves of an applier no GitTarget names any more: nobody asks it for anything.
func (l *branchWorkerEventLoop) dropSaves(state *applierState) {
	saves := state.waitingSaves
	if state.inflight != nil {
		for id := range state.inflight.saves {
			if saves == nil {
				saves = map[commitRequestID]bool{}
			}
			saves[id] = true
		}
	}
	for id := range saves {
		l.w.recordSaveApply(id, SaveApply{Done: true, Reason: SaveApplyApplierGone,
			Message: "applier " + state.ref.String() + " is no longer named on this branch"})
	}
}
