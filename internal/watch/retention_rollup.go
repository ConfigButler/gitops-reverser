// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"time"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A suppressed sweep is invisible by construction — no plan action, no commit, no other stat —
// which is exactly right for the write path and leaves an operator unable to tell a CONVERGED
// mirror from one that is deliberately retaining. This roll-up is the answer: each scope's resync
// reports what its policy kept, the counts are summed per GitTarget, and the controller projects
// the sum onto status.retention.
//
// It is an observation, never a gate. Nothing here may fail a reconciliation or move a condition.

// RetentionSummary is the per-GitTarget roll-up the controller projects onto status.
type RetentionSummary struct {
	// Reported distinguishes "no resync has reported yet" from "a resync reported zero". Both are
	// legitimate states and they mean opposite things: the first is unknown, the second is the
	// converged signal, which is half the value of publishing this at all.
	Reported bool
	// Mode is the effective spec.prune.mode the most recent contributing resync ran under. It
	// travels WITH the count rather than being read from the spec at projection time, so the two
	// always describe the same observation — a target switched to `always` does not briefly
	// publish `always` beside a count that a retaining policy produced.
	Mode v1alpha3.PruneMode
	// RetainedDocuments is the sum over the target's currently tracked scopes.
	RetainedDocuments int
	// ObservedTime is when the most recent contributing resync reported.
	ObservedTime time.Time
}

// targetRetentionScope is one cell's count, stamped with the stream revision that produced it.
type targetRetentionScope struct {
	revision uint64
	retained int
	reported bool
	// reportedRevision is the revision of the report that produced `retained`. It separates a
	// stream RE-reporting under the revision it already reported (routine, every resync) from a
	// NEW incarnation measuring the cell afresh and arriving at the same number. Only the second
	// is interesting: it is the one an unchanged published count can be hiding
	// (docs/design/watch-plane-status-convergence-failures.md, §3.4).
	reportedRevision uint64
}

// targetRetentionState is one GitTarget's per-cell counts, covering exactly the cells its
// current watch plan selects.
type targetRetentionState struct {
	scopes   map[types.CellKey]targetRetentionScope
	mode     v1alpha3.PruneMode
	observed time.Time
}

func (s targetRetentionState) total() int {
	sum := 0
	for _, scope := range s.scopes {
		sum += scope.retained
	}
	return sum
}

func (s targetRetentionState) anyReported() bool {
	for _, scope := range s.scopes {
		if scope.reported {
			return true
		}
	}
	return false
}

// MarkTargetRetention records what one scope's resync retained.
//
// Scope lifecycle is handled by the watch plan rather than by eviction here:
// retainTargetRetentionScopes installs the selected cells and the revision each one's stream
// reports under, so a cell that leaves the plan takes its count with it, and a stale in-flight
// reply from a cancelled watch — which carries the retired stream's revision — cannot resurrect
// or overwrite a count. A report for a cell the plan does not hold is dropped for the same
// reason.
//
// Zero is recorded as actively as any other number: it is the converged signal.
func (m *Manager) MarkTargetRetention(
	gitDest types.ResourceReference,
	cell types.CellKey,
	revision uint64,
	mode v1alpha3.PruneMode,
	retained int,
) {
	// Prompt a status refresh on a CHANGE only. Without it the first appearance of a retention
	// would wait for the steady requeue (minutes), which is too long for a signal an operator
	// consults before flipping a target to `always`; with it on every report, a steadily retaining
	// target would enqueue on every resync of every scope forever.
	// A dropped report is recorded, not swallowed. Dropping is correct -- it is how a tail from a
	// replaced stream is kept out -- but the CONSEQUENCE is that the published count no longer
	// describes the mirror, and nothing re-measures it until the plan next restarts this cell,
	// which for a settled target is the steady requeue away. A roll-up that silently stops
	// advancing is the same class of invisible failure the storm was: it reads exactly like a
	// converged one.
	var dropped string
	var installed uint64
	var remeasured bool
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		state := s.retention[gitDest.Key()]
		scope, selected := state.scopes[cell]
		if !selected {
			dropped = "the cell is not in the current watch plan"
			return false
		}
		if revision != scope.revision {
			dropped, installed = "the reporting stream has been replaced", scope.revision
			return false
		}
		// Captured BEFORE the write below, so "changed" compares what an operator would see on
		// status, not what the internal map did. A re-report of the same total is not a change to
		// them, and enqueueing for it would make every watch-set replacement reconcile twice.
		priorTotal, priorMode, priorReported := state.total(), state.mode, state.anyReported()
		remeasured = scope.reported && scope.reportedRevision != revision
		scope.retained = retained
		scope.reported = true
		scope.reportedRevision = revision
		state.scopes[cell] = scope
		state.mode = mode.OrDefault()
		state.observed = time.Now()
		s.retention[gitDest.Key()] = state
		return !priorReported || state.total() != priorTotal || state.mode != priorMode
	})
	if dropped != "" {
		m.Log.WithName("retention").Info(
			"retention report dropped; the published count is now stale until this cell is replanned",
			"gitDest", gitDest.String(), "cell", cell.String(), "reason", dropped,
			"reportedRevision", revision, "installedRevision", installed, "retained", retained)
		return
	}
	// Accepted. Say so, because the alternative is the silence that hid Failure B: a report that
	// lands and changes nothing an operator would see is DISCARDED whole by mutateWatchPlane, so
	// "accepted and unchanged" and "never reported at all" look identical from outside. The
	// refusals above have been logged since c24844a1; the acceptances were not, and that asymmetry
	// is why B could be narrowed to this function and no further
	// (docs/design/watch-plane-status-convergence-failures.md, §3.4).
	//
	// Info is reserved for the one shape that is genuinely ambiguous: a cell RE-MEASURED under a
	// new revision that arrived at the number already published, so the mutation was discarded and
	// status did not move. A stream re-reporting under the revision it already reported is routine
	// -- it happens on every resync of every steady scope, and logging it at Info put ~100 lines
	// into a single e2e run, which is how a diagnostic becomes noise instead of evidence.
	log := m.Log.WithName("retention").WithValues(
		"gitDest", gitDest.String(), "cell", cell.String(),
		"revision", revision, "mode", string(mode.OrDefault()), "retained", retained)
	switch {
	case !changed && remeasured:
		log.Info("retention report accepted but published nothing; a fresh measurement of this " +
			"cell produced the count already published")
	case !changed:
		log.V(1).Info("retention report accepted; nothing an operator sees moved")
	default:
		log.V(1).Info("retention report accepted")
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// retainTargetRetentionScopes installs the cells the current watch plan selects, and the stream
// revision each one reports under. A cell that left the plan is dropped along with its count; a
// cell that stayed keeps the count it last reported, because its stream may not have moved.
func (m *Manager) retainTargetRetentionScopes(
	gitDest types.ResourceReference,
	revisions map[types.CellKey]uint64,
) {
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		state := s.retention[gitDest.Key()]
		next := make(map[types.CellKey]targetRetentionScope, len(revisions))
		for cell, revision := range revisions {
			scope := state.scopes[cell]
			// A restarted stream reports under a new revision. Its previous count stands until the
			// replacement reports, which is a truer answer than zeroing a scope nobody re-measured.
			scope.revision = revision
			next[cell] = scope
		}
		state.scopes = next
		s.retention[gitDest.Key()] = state
		return true
	})
}

// RetentionForGitTarget returns the roll-up across the target's currently tracked scopes.
func (m *Manager) RetentionForGitTarget(gitDest types.ResourceReference) RetentionSummary {
	state, had := m.watchPlane().retention[gitDest.Key()]
	if !had || !state.anyReported() {
		return RetentionSummary{}
	}
	return RetentionSummary{
		Reported:          true,
		Mode:              state.mode,
		RetainedDocuments: state.total(),
		ObservedTime:      state.observed,
	}
}

// forgetTargetRetention drops a deleted GitTarget's roll-up.
func (m *Manager) forgetTargetRetention(gitDest types.ResourceReference) {
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		if _, had := s.retention[gitDest.Key()]; !had {
			return false
		}
		delete(s.retention, gitDest.Key())
		return true
	})
}
