// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ageSaveApply moves a save's apply record back by by, as if it was asked and last recorded then.
func ageSaveApply(w *BranchWorker, id commitRequestID, by time.Duration) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	entry := w.crApplies[id]
	entry.askedAt = entry.askedAt.Add(-by)
	entry.updatedAt = entry.updatedAt.Add(-by)
	w.crApplies[id] = entry
}

// An apply that runs longer than the outcome TTL keeps its record while the controller still
// follows it, so another save resolving never turns it into a lost one; it is collected only past
// SaveApplyTimeout. A settled record is collected after the outcome TTL.
func TestSaveApply_AnUnsettledRecordOutlivesTheFollowDeadline(t *testing.T) {
	w := &BranchWorker{}
	running := commitRequestID{Namespace: "default", Name: "running", UID: "u1"}
	settled := commitRequestID{Namespace: "default", Name: "settled", UID: "u2"}
	w.recordSaveApply(running, SaveApply{Reason: SaveApplyApplying})
	w.recordSaveApply(settled, SaveApply{Done: true, Applied: true, Reason: SaveApplyApplied})
	collect := func() {
		w.recordCommitRequestOutcome(commitRequestID{Namespace: "default", Name: "other"}, FinalizeResult{})
	}

	ageSaveApply(w, running, commitRequestOutcomeTTL+time.Minute)
	ageSaveApply(w, settled, 2*commitRequestOutcomeTTL)
	collect()
	_, kept := w.LookupSaveApply(running.Namespace, running.Name, running.UID)
	assert.True(t, kept, "an unsettled apply is kept past the outcome TTL")
	_, kept = w.LookupSaveApply(settled.Namespace, settled.Name, settled.UID)
	assert.False(t, kept, "a settled apply is collected after the outcome TTL")

	w.recordSaveApply(running, SaveApply{Reason: SaveApplyRetrying}) // a retry keeps when it was asked
	ageSaveApply(w, running, SaveApplyTimeout)
	collect()
	_, kept = w.LookupSaveApply(running.Namespace, running.Name, running.UID)
	assert.False(t, kept, "past the follow deadline nothing reads it any more")
}
