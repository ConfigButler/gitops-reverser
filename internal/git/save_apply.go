// SPDX-License-Identifier: Apache-2.0

package git

import (
	"maps"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// A CommitRequest with catchUp: AfterPush asks its GitTarget's applier to apply the tip as soon as its
// commit is pushed, and the controller reports how that went in its Applied condition. The save's own
// outcome is decided by the push alone; this is reported beside it and never changes it.
//
// The worker keeps, per applier, the saves waiting for an apply, each with the commit it pushed. The
// attempt that goes out carries them, and the one that succeeds settles each save whose commit the
// applied revision holds (settleSaves). An attempt that does not succeed gives them back, and the next
// carries them again. A save that pushed no commit, whose GitTarget names no applier, or whose applier
// stops being named is settled as applied: no apply is needed, and the cluster, where the saved objects
// came from, already holds them. Only a save whose commit the branch lost is settled as not applied.
//
// A save's apply is followed for SaveApplyTimeout at most: the controller then stops following it
// and says so in Applied. The worker keeps a record that is not settled at least that long, so it
// never ages out while the controller still reads it.

// Why a save's apply was settled, or why it still waits: the Applied condition's reasons.
const (
	SaveApplyApplying       = "Applying"
	SaveApplyRetrying       = "Retrying"
	SaveApplyApplied        = "Applied"
	SaveApplyNothingToApply = "NothingToApply"
	SaveApplyNoApplier      = "NoApplierClaim"
	SaveApplyApplierGone    = "ApplierGone"
	SaveApplyCommitDropped  = "CommitDropped"
)

// SaveApplyTimeout is how long, from Ready, the controller follows the apply a CommitRequest asked
// for. A save is not kept waiting on an applier that does not catch up: past it, Applied settles as
// timed out, though the commit stays pushed and the applier may still apply it.
const SaveApplyTimeout = 10 * time.Minute

// SaveApply is how the apply a CommitRequest asked for stands.
type SaveApply struct {
	// Done is whether it is settled: it changes no more.
	Done bool
	// Applied is whether the applier applied a commit that holds the save's, or no applier applies
	// the branch.
	Applied bool
	// Reason is one of the SaveApply* reasons, and Message says more.
	Reason  string
	Message string
	// AppliedRevision is the revision the applier reported applying, once Applied.
	AppliedRevision string
}

type saveApplyEntry struct {
	apply SaveApply
	// askedAt is when the save asked for its apply, and updatedAt when apply was last recorded.
	askedAt   time.Time
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
	now := time.Now()
	askedAt := now
	if entry, ok := w.crApplies[id]; ok {
		askedAt = entry.askedAt
	}
	w.crApplies[id] = saveApplyEntry{apply: apply, askedAt: askedAt, updatedAt: now}
}

// LookupSaveApply reports where the apply a CommitRequest asked for stands, and false while the
// worker knows nothing of it.
func (w *BranchWorker) LookupSaveApply(namespace, name, uid string) (SaveApply, bool) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	entry, ok := w.crApplies[commitRequestID{Namespace: namespace, Name: name, UID: uid}]
	return entry.apply, ok
}

// collectSaveApplies drops a settled entry once it is older than the outcome TTL. One not settled is
// kept the outcome TTL past SaveApplyTimeout, counted from when it was asked: the controller reads
// Ready within the outcome TTL of that, and stops following at SaveApplyTimeout from Ready. The
// caller holds crOutcomesMu.
func (w *BranchWorker) collectSaveApplies(now time.Time) {
	for id, entry := range w.crApplies {
		expired := now.Sub(entry.updatedAt) > commitRequestOutcomeTTL
		if !entry.apply.Done {
			expired = now.Sub(entry.askedAt) > commitRequestOutcomeTTL+SaveApplyTimeout
		}
		if expired {
			delete(w.crApplies, id)
		}
	}
}

// catchUpAfterSave is called as a CommitRequest that asked to catch up resolves. With a pushed
// commit, its GitTarget's applier gets a converge when the appliers are next listed, at the end of
// this wake (takeSavedCatchUps); with none, there is nothing to apply.
func (l *branchWorkerEventLoop) catchUpAfterSave(pcr *pendingCommitRequest, commit string) {
	if commit == "" {
		l.w.recordSaveApply(pcr.id, SaveApply{Done: true, Applied: true, Reason: SaveApplyNothingToApply,
			Message: "the request pushed no commit: what it saved was in Git already, and the cluster holds it"})
		return
	}
	if l.savedCatchUps == nil {
		l.savedCatchUps = map[itypes.ResourceReference]map[commitRequestID]string{}
	}
	ref := itypes.NewResourceReference(pcr.gitTargetName, pcr.gitTargetNamespace)
	if l.savedCatchUps[ref] == nil {
		l.savedCatchUps[ref] = map[commitRequestID]string{}
	}
	l.savedCatchUps[ref][pcr.id] = commit
	l.w.recordSaveApply(pcr.id, SaveApply{Reason: SaveApplyApplying,
		Message: "commit " + commit + " is pushed; its applier is asked to apply it"})
}

// takeSavedCatchUps leaves a converge pending for every applier a pushed CommitRequest asked to catch
// up, and records the request as waiting on it. It was asked for, so it is no lag converge: it does
// not yield to an edit (convergeHolds). A request whose GitTarget names no applier is settled.
func (l *branchWorkerEventLoop) takeSavedCatchUps() {
	for ref, saves := range l.savedCatchUps {
		state := l.applierNaming(ref)
		if state == nil {
			for id := range saves {
				l.w.recordSaveApply(id, SaveApply{Done: true, Applied: true, Reason: SaveApplyNoApplier,
					Message: "GitTarget " + ref.String() + " names no applier claim, so nothing applies the " +
						"branch; the cluster already holds the saved objects"})
			}
			continue
		}
		state.converge, state.lag = true, false
		for id, commit := range saves {
			state.waitSave(id, commit)
		}
		l.w.Log.Info("A CommitRequest asked its applier to catch up with the pushed commit",
			"branch", l.w.Branch, "applier", state.ref.String(), "gitTarget", ref.String(), "requests", len(saves))
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

// waitSave records a CommitRequest, which pushed commit, as waiting for the applier's next apply that
// succeeds.
func (s *applierState) waitSave(id commitRequestID, commit string) {
	if s.waitingSaves == nil {
		s.waitingSaves = map[commitRequestID]string{}
	}
	s.waitingSaves[id] = commit
}

// settleSaves settles the saves an apply of revision answered, by what revision holds:
//
//   - a save whose commit revision names or descends from is applied;
//   - one whose commit the branch no longer holds either is not, and never will be: the branch's
//     history was rewritten after the push;
//   - one whose commit the branch holds and revision does not waits for the converge left pending;
//   - one the checkout cannot tell about, because it lacks a commit, waits as well, and the remote is
//     looked at: revision is a commit pushed elsewhere, and its fetch brings the apply that settles it.
func (l *branchWorkerEventLoop) settleSaves(state *applierState, saves map[commitRequestID]string, revision string) {
	if len(saves) == 0 {
		return
	}
	repo, err := gogit.PlainOpen(l.w.repoPath())
	if err != nil {
		repo = nil
	}
	applied, tip := revisionCommit(revision), l.w.inbox.lastTip()
	look := false
	for id, commit := range saves {
		held, known := commitHolds(repo, applied, commit)
		if held {
			l.w.recordSaveApply(id, SaveApply{Done: true, Applied: true, Reason: SaveApplyApplied,
				AppliedRevision: revision, Message: "the applier applied " + revision})
			continue
		}
		onBranch, branchKnown := commitHolds(repo, tip, commit)
		switch {
		case known && branchKnown && !onBranch:
			l.w.recordSaveApply(id, SaveApply{Done: true, Reason: SaveApplyCommitDropped, Message: "the applier " +
				"applied " + revision + ", and the branch no longer holds commit " + commit + ": its history was " +
				"rewritten after the push"})
			continue
		case known:
			state.converge, state.lag = true, false
			l.w.recordSaveApply(id, SaveApply{
				Reason:  SaveApplyRetrying,
				Message: "the applier applied " + revision + ", which does not hold commit " + commit + "; asking again",
			})
		default:
			look = look || applied != ""
			l.w.recordSaveApply(id, SaveApply{Reason: SaveApplyRetrying, Message: "the applier applied " + revision +
				", and whether it holds commit " + commit + " is not known here yet; looking at the remote"})
		}
		state.waitSave(id, commit)
	}
	if look {
		l.w.EnqueuePushNotification(applied)
	}
}

// commitHolds reports whether descendant is commit or descends from it, and whether the checkout could
// tell: it holds both commits.
func commitHolds(repo *gogit.Repository, descendant, commit string) (bool, bool) {
	if descendant == "" || commit == "" {
		return false, false
	}
	if strings.EqualFold(descendant, commit) {
		return true, true
	}
	if repo == nil {
		return false, false
	}
	descendantID, ok := plumbing.FromHex(descendant)
	if !ok {
		return false, false
	}
	commitID, ok := plumbing.FromHex(commit)
	if !ok {
		return false, false
	}
	later, err := repo.CommitObject(descendantID)
	if err != nil {
		return false, false
	}
	earlier, err := repo.CommitObject(commitID)
	if err != nil {
		return false, false
	}
	held, err := earlier.IsAncestor(later)
	if err != nil {
		return false, false
	}
	return held, true
}

// retrySaves records the saves an attempt that did not succeed gave back as waiting again, with why.
func (l *branchWorkerEventLoop) retrySaves(saves map[commitRequestID]string, why string) {
	for id := range saves {
		l.w.recordSaveApply(id, SaveApply{Reason: SaveApplyRetrying,
			Message: "the apply did not complete, and is asked again: " + why})
	}
}

// dropSaves settles the saves of an applier no GitTarget names any more as applied: nothing applies the
// branch for them now, and the cluster already holds the saved objects.
func (l *branchWorkerEventLoop) dropSaves(state *applierState) {
	saves := maps.Clone(state.waitingSaves)
	if state.inflight != nil {
		if saves == nil {
			saves = map[commitRequestID]string{}
		}
		maps.Copy(saves, state.inflight.saves)
	}
	for id := range saves {
		l.w.recordSaveApply(id, SaveApply{Done: true, Applied: true, Reason: SaveApplyApplierGone,
			Message: "applier " + state.ref.String() + " is no longer named on this branch, so nothing applies " +
				"it; the cluster already holds the saved objects"})
	}
}
