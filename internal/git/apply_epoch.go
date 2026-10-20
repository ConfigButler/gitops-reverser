// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// A resync never decides from a stale picture (docs/design/applier-echoes.md, F3).
//
// A resync mirrors a snapshot of live state. While Git is ahead of the cluster, a commit pushed and
// not yet applied, a snapshot holds the old state of every object that commit changes, and one whose
// collection overlaps an apply holds a mix. Either, decided, reverts the push. So the watch side reads
// the worker's ApplyState when a snapshot's collection starts, and the snapshot is used only if Git
// was not ahead then, the apply epoch has not moved since, and Git is not ahead when it arrives.
// Otherwise it is discarded, and a fresh snapshot of the GitTarget is asked for once Git is no longer
// ahead (resyncRecheck re-anchors its streams). No snapshot is ever parked.
//
// Three exceptions keep that from holding anything forever:
//
//   - A branch with no trigger never waits: nothing applies its commits.
//   - A collection's initial snapshot (the first one applied for it, ResyncRequest.Initial) is
//     exempt. It is the startup baseline, and S2 asks no applier for anything before it is in Git,
//     so holding it for an apply would wait on itself.
//   - A GitTarget whose snapshots were held for snapshotHoldLimit uses the next one anyway: an
//     apply that keeps failing must not leave drift unmirrored for as long as it fails. The C2
//     policy, API wins, then applies to whatever the push changed.

// snapshotHoldLimit is how long F3 may hold off a GitTarget's resyncs, from the first snapshot it
// discarded: applierRetryMax, so an applier at full backoff gets at least one more attempt first.
const snapshotHoldLimit = applierRetryMax

// Stale snapshot outcomes, the outcome label of stale_snapshots_total.
const (
	staleSnapshotDiscarded = "discarded"
	staleSnapshotUsed      = "used"
)

// Why a snapshot was discarded, for the log.
const (
	snapshotAheadAtStart   = "collected while Git was ahead of the cluster"
	snapshotAcrossApply    = "collected across an apply"
	snapshotAheadOnArrival = "arrived while Git is ahead of the cluster"
	snapshotForeignFetch   = "its fetch found a commit the cluster does not run yet"
)

// ApplyState reports where this worker's appliers stand, for the watch side to read when a
// snapshot's collection starts. It is safe from any goroutine.
func (w *BranchWorker) ApplyState() ApplyState {
	ahead := w.appliersAhead.Load() || (w.hasAppliers.Load() && w.inbox.foreignPending())
	return ApplyState{Epoch: w.applyEpoch.Load(), GitAhead: ahead}
}

// raiseApplyEpoch marks a send or an end of an attempt: a snapshot collected across it is stale.
func (w *BranchWorker) raiseApplyEpoch() { w.applyEpoch.Add(1) }

// gitAhead reports whether Git may hold a commit the cluster does not run yet: an applier on the
// branch has a reason pending that applies the tip, or an attempt out. A converge counts once the
// applier may be asked for it; an adopt does not count, because it applies objects the cluster
// already runs. A branch with no applier is never ahead.
func (l *branchWorkerEventLoop) gitAhead() bool {
	if len(l.appliers) == 0 {
		return false
	}
	return l.appliersAhead() || l.w.inbox.foreignPending()
}

// appliersAhead is gitAhead without what the inbox holds.
func (l *branchWorkerEventLoop) appliersAhead() bool {
	if len(l.unsentRefusals) > 0 {
		return true
	}
	for _, state := range l.appliers {
		if state.inflight != nil || state.foreign || state.repair || (state.converge && l.pastBarrier(state)) {
			return true
		}
	}
	return false
}

// publishApplyState publishes the loop's half of ApplyState. It runs at the end of every wake.
func (l *branchWorkerEventLoop) publishApplyState() {
	l.w.hasAppliers.Store(len(l.appliers) > 0)
	l.w.appliersAhead.Store(len(l.appliers) > 0 && l.appliersAhead())
}

// handleResyncRequest decides one revision-pinned resync in order on the worker goroutine, unless its
// snapshot is stale (snapshotStale), in which case it is discarded and asked for again.
func (l *branchWorkerEventLoop) handleResyncRequest(req *ResyncRequest) {
	if reason := l.snapshotStale(req); reason != "" {
		l.discardSnapshot(req, reason)
		return
	}
	l.applyResync(req)
}

// snapshotStale says why a resync's snapshot must not be decided, or "" when it may be.
func (l *branchWorkerEventLoop) snapshotStale(req *ResyncRequest) string {
	if !l.snapshotsWait(req) {
		return ""
	}
	switch start := req.CollectionStart; {
	case start != nil && start.GitAhead:
		return snapshotAheadAtStart
	case start != nil && start.Epoch != l.w.applyEpoch.Load():
		return snapshotAcrossApply
	case l.gitAhead():
		return snapshotAheadOnArrival
	}
	return ""
}

// snapshotsWait reports whether F3 applies to a resync: the branch has an applier, the snapshot is
// not its collection's initial one, and the GitTarget's snapshots have not been held for
// snapshotHoldLimit already. A snapshot let through by the limit is logged and counted as used.
func (l *branchWorkerEventLoop) snapshotsWait(req *ResyncRequest) bool {
	if len(l.appliers) == 0 || req.Initial {
		return false
	}
	since, held := l.snapshotHeldSince[resyncTargetKey(req)]
	if !held || l.w.now().Sub(since) < snapshotHoldLimit {
		return true
	}
	if l.gitAhead() {
		l.w.Log.Info("Using a snapshot although Git is ahead of the cluster: its GitTarget's snapshots have "+
			"been held for the limit, and drift must not stay unmirrored for as long as an apply fails",
			"gitTarget", resyncTargetKey(req), "heldFor", l.w.now().Sub(since).Round(time.Second).String())
		l.recordStaleSnapshot(staleSnapshotUsed)
	}
	return false
}

// discardSnapshot answers a resync whose snapshot is stale, and asks for a fresh one of its GitTarget
// once Git is no longer ahead.
func (l *branchWorkerEventLoop) discardSnapshot(req *ResyncRequest, reason string) {
	key := resyncTargetKey(req)
	l.w.Log.Info("Discarding a resync snapshot; a fresh one is asked for once the apply is done",
		"gitTarget", key, "scope", req.Scope.String(), "reason", reason)
	req.reply(ResyncResult{Err: fmt.Errorf("%w: %s", ErrResyncDiscarded, reason)})
	l.recordStaleSnapshot(staleSnapshotDiscarded)
	if l.snapshotRerequests == nil {
		l.snapshotRerequests = map[string]itypes.ResourceReference{}
	}
	l.snapshotRerequests[key] = itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace)
	if l.snapshotHeldSince == nil {
		l.snapshotHeldSince = map[string]time.Time{}
	}
	if _, held := l.snapshotHeldSince[key]; !held {
		l.snapshotHeldSince[key] = l.w.now()
	}
}

// heldForTheLimit reports whether a GitTarget's snapshots have been held for snapshotHoldLimit: its
// next one is asked for, and used, whatever is ahead.
func (l *branchWorkerEventLoop) heldForTheLimit(key string) bool {
	since, held := l.snapshotHeldSince[key]
	return held && l.w.now().Sub(since) >= snapshotHoldLimit
}

// snapshotHoldDeadline is when the earliest held GitTarget reaches snapshotHoldLimit, zero when none
// waits for it: the applier timer wakes the loop then, to ask for its snapshot.
func (l *branchWorkerEventLoop) snapshotHoldDeadline() time.Time {
	var deadline time.Time
	for key := range l.snapshotRerequests {
		if since, held := l.snapshotHeldSince[key]; held {
			deadline = earliest(deadline, since.Add(snapshotHoldLimit))
		}
	}
	return deadline
}

// snapshotUsed records that a GitTarget's snapshot was decided: its hold is over.
func (l *branchWorkerEventLoop) snapshotUsed(req *ResyncRequest) {
	delete(l.snapshotHeldSince, resyncTargetKey(req))
}

// rerequestSnapshots asks for the snapshots discarded, once what made them stale is over. It runs
// at the end of every wake.
func (l *branchWorkerEventLoop) rerequestSnapshots() {
	if len(l.snapshotRerequests) == 0 {
		return
	}
	ahead := l.gitAhead()
	for key, target := range l.snapshotRerequests {
		if ahead && !l.heldForTheLimit(key) {
			continue
		}
		delete(l.snapshotRerequests, key)
		if l.w.resyncRecheck == nil {
			continue
		}
		l.w.Log.Info("Asking for a fresh snapshot of a GitTarget whose snapshot was discarded",
			"gitTarget", key)
		l.w.resyncRecheck(target)
	}
}

// initialSnapshotsPublished reports whether a GitTarget's initial snapshots are published: every
// collection it watches has a settled snapshot, applied, refused or failed (the watch side says so,
// initialSnapshots), and the worker holds no resync of it, queued or decided and not yet published.
func (l *branchWorkerEventLoop) initialSnapshotsPublished(ref itypes.ResourceReference) bool {
	if l.w.initialSnapshots != nil && !l.w.initialSnapshots(ref) {
		return false
	}
	key := ref.String()
	if l.w.queuedResyncsFor(key) {
		return false
	}
	for i := range l.pendingWrites {
		if write := &l.pendingWrites[i]; write.Kind == PendingWriteResync &&
			write.GitTargetNamespace+"/"+write.GitTargetName == key {
			return false
		}
	}
	return true
}

// recordStaleSnapshot counts one stale snapshot, by what was done with it.
func (l *branchWorkerEventLoop) recordStaleSnapshot(outcome string) {
	if telemetry.StaleSnapshotsTotal == nil {
		return
	}
	telemetry.StaleSnapshotsTotal.Add(context.WithoutCancel(l.w.ctx), 1,
		metric.WithAttributes(l.w.providerAttrs(attribute.String("outcome", outcome))...))
}
