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

// targetRetentionState is one GitTarget's per-scope counts, valid for a single watch epoch.
type targetRetentionState struct {
	epoch    uint64
	scopes   map[targetWatchKey]int
	mode     v1alpha3.PruneMode
	observed time.Time
}

func (s targetRetentionState) total() int {
	sum := 0
	for _, retained := range s.scopes {
		sum += retained
	}
	return sum
}

// MarkTargetRetention records what one scope's resync retained.
//
// Scope lifecycle is handled by the EPOCH rather than by eviction, reusing the watch epoch
// RenderFidelityGate already defines: records carry the epoch they were produced under, a new
// epoch replaces the whole per-scope map, and a record from an older epoch is dropped. A scope
// that leaves the watch plan therefore takes its count with it at the next declaration, with no
// per-key deletion logic to get wrong — and a stale in-flight reply from a cancelled watch cannot
// resurrect a count for a scope this target no longer has.
//
// Zero is recorded as actively as any other number: it is the converged signal.
func (m *Manager) MarkTargetRetention(
	gitDest types.ResourceReference,
	key targetWatchKey,
	epoch uint64,
	mode v1alpha3.PruneMode,
	retained int,
) {
	m.targetRetentionMu.Lock()
	if m.targetRetention == nil {
		m.targetRetention = map[string]targetRetentionState{}
	}
	state, had := m.targetRetention[gitDest.Key()]
	if had && epoch < state.epoch {
		m.targetRetentionMu.Unlock()
		return
	}
	// Captured BEFORE the epoch reset below, so "changed" compares what an operator would see on
	// status, not what the internal map did. A new epoch that re-reports the same total is not a
	// change to them, and enqueueing for it would make every watch-set replacement reconcile twice.
	priorTotal, priorMode := state.total(), state.mode
	if !had || epoch > state.epoch {
		state = targetRetentionState{epoch: epoch, scopes: map[targetWatchKey]int{}}
	}
	state.scopes[key] = retained
	state.mode = mode.OrDefault()
	state.observed = time.Now()
	m.targetRetention[gitDest.Key()] = state
	changed := !had || state.total() != priorTotal || state.mode != priorMode
	m.targetRetentionMu.Unlock()

	// Prompt a status refresh on a CHANGE only. Without it the first appearance of a retention
	// would wait for the steady requeue (minutes), which is too long for a signal an operator
	// consults before flipping a target to `always`; with it on every report, a steadily retaining
	// target would enqueue on every resync of every scope forever.
	if changed {
		m.enqueueGitPathChange(gitDest)
	}
}

// RetentionForGitTarget returns the roll-up across the target's currently tracked scopes.
func (m *Manager) RetentionForGitTarget(gitDest types.ResourceReference) RetentionSummary {
	m.targetRetentionMu.Lock()
	defer m.targetRetentionMu.Unlock()
	state, had := m.targetRetention[gitDest.Key()]
	if !had {
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
	m.targetRetentionMu.Lock()
	defer m.targetRetentionMu.Unlock()
	delete(m.targetRetention, gitDest.Key())
}
