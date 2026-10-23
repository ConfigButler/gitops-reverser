// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"time"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Restart recovery (docs/design/apply-trigger.md, "Restart recovery"). The worker's attempts live
// in memory, and the applier claim's status.operation is their durable record: the worker writes it
// before an apply can start (ApplyRequest.Record). So a process that starts reads it back, before
// its first live event is mirrored:
//
//   - An operation that has not completed is followed to its end, by its recorded tokens, with its
//     apply window open, exactly as if this process had sent it: the apply may still be running, or
//     the Kustomization may have been left resumed, and the follow suspends it again.
//   - repairPending is restored.
//
// The applier claim's reconciler records an apply somebody else started (`flux resume`) the same way, as
// an operation with no request token. The worker follows such a record whenever it lists one, not
// only on start, so its apply window opens for an apply nobody checked.
//
// The first observation of the branch is still a baseline that leaves a CatchUp pending: a
// recovered operation decides nothing about which reasons are pending, and asks the applier for
// nothing.

// followRecordedOperations reads the applier claims' recorded status on every pass, and follows what it
// finds. Every pass, and not only while the branch has appliers: a GitTarget naming an applier claim for
// the first time is seen here, and gets its first apply (syncAppliers). The listing is cached. A
// listing that fails is tried again on the applier timer (listRetryAt), as takeReasons' is.
func (l *branchWorkerEventLoop) followRecordedOperations(now time.Time) {
	w := l.w
	if w.applyRequester == nil {
		return
	}
	targets, err := w.appliersOnBranch(w.ctx)
	if err != nil {
		w.Log.Error(err, "Cannot read the applier claims' recorded operations; retrying", "branch", w.Branch)
		l.listRetryAt = time.Now().Add(applierRetryInterval)
		l.resetApplierTimer(applierRetryInterval)
		return
	}
	l.listRetryAt = time.Time{}
	l.syncAppliers(targets)
	for _, ref := range l.sortedAppliers() {
		l.followRecordedFor(l.appliers[ref], now)
	}
}

// followRecordedFor follows what one applier's applier claim records: anything in flight, and the
// pending repair, the first time this process lists it; afterwards only an apply started elsewhere.
func (l *branchWorkerEventLoop) followRecordedFor(state *applierState, now time.Time) {
	recorded, repairPending := recordedStatus(state)
	inFlight := recorded != nil && !recorded.Phase.Completed() && state.inflight == nil
	if !state.recovered {
		state.recovered = true
		state.repair = state.repair || repairPending
		if inFlight {
			l.followRecorded(state, recorded, now)
		}
		return
	}
	if inFlight && recorded.Token == "" && recorded.SuspendToken != state.followedSuspend {
		l.followRecorded(state, recorded, now)
	}
}

// recordedStatus is the operation and repairPending recorded on an applier's applier claim, as listed with
// any GitTarget naming it: they all name the same applier claim.
func recordedStatus(state *applierState) (*v1alpha3.ApplyOperation, bool) {
	for _, target := range state.targets {
		return target.recorded, target.repairPending
	}
	return nil, false
}

// followRecorded hands the requester an attempt that follows a recorded operation instead of asking
// for one, and opens its applier's apply window from now: the watches of this process see nothing
// from before it. The attempt carries no pending reason, so whatever it ends with, nothing is given
// back.
func (l *branchWorkerEventLoop) followRecorded(state *applierState, recorded *v1alpha3.ApplyOperation, now time.Time) {
	w := l.w
	l.nextApplyID++
	w.raiseApplyEpoch()
	abandoned, abandon := context.WithCancel(w.ctx)
	attempt := &inflightAttempt{
		id: l.nextApplyID, abandon: abandon, recovered: true,
		previous: state.operation, previousID: state.operationID,
	}
	state.inflight, state.lastTurn = attempt, attempt.id
	state.followedSuspend = recorded.SuspendToken
	l.reportApplyWindow(state)
	state.openApplyWindow(attempt.id, recorded.Commit, now)
	state.operation, state.operationID = recorded.DeepCopy(), attempt.id

	targets := make([]ApplyTarget, 0, len(state.targets))
	for _, target := range state.targets {
		targets = append(targets, ApplyTarget{Ref: target.ref, UID: target.uid, Reason: recorded.Reason})
	}
	w.Log.Info("Following an apply recorded on the applier claim, with its apply window open",
		"branch", w.Branch, "applier", state.ref.String(), "phase", string(recorded.Phase),
		"reason", string(recorded.Reason), "commit", recorded.Commit, "startedElsewhere", recorded.Token == "")
	w.applyRequester.RequestApply(ApplyRequest{
		ID:        attempt.id,
		Branch:    w.branchKey(),
		Provider:  w.provider(),
		GitBranch: w.GitBranch,
		Applier:   state.ref,
		Reason:    recorded.Reason,
		Commit:    recorded.Commit,
		Token:     recorded.Token,
		Targets:   targets,
		Recovered: recorded.DeepCopy(),
		Report:    w.reportApplyResult,
		Abandoned: abandoned.Done(),
	})
}
