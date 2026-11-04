// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPushCooldown_TwoSavesInARowArePushedAtOnce pins gitops-api ask #41: a push a CommitRequest waits
// on skips the cooldown, so a save that follows a push by a second still resolves at once.
func TestPushCooldown_TwoSavesInARowArePushedAtOnce(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()
	loop.lastPushAt = time.Now() // a push has just been made: the cooldown runs

	for _, name := range []string{"save-1", "save-2"} {
		writeTo(loop, "edit-of-"+name)
		req := namedAttachReq(name, 0) // commits the open window at the attach
		req.Message = "feat: " + name
		serviceAttach(loop, req)

		res, ok := worker.LookupCommitRequestOutcome("default", name, "uid-"+name)
		require.True(t, ok, "%s is pushed and resolved inside the cooldown", name)
		require.NoError(t, res.Err)
		assert.Equal(t, FinalizeCommitted, res.Outcome)
		assert.Empty(t, loop.pendingWrites, "%s's write is published", name)
		assert.Nil(t, loop.pushTimer, "no push waits for the cooldown")
	}
}

// TestPushCooldown_PlainWritesStillWaitOutTheCooldown pins what the cooldown protects: commits that no
// save waits on, however fast they come, share one push once it ends.
func TestPushCooldown_PlainWritesStillWaitOutTheCooldown(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()
	loop.defaultWindow.maxDuration = 0 // every write is a commit of its own
	loop.lastPushAt = time.Now()

	writeTo(loop, "first")
	writeTo(loop, "second")
	loop.endWake(0)

	assert.Len(t, loop.pendingWrites, 2, "both commits wait for the cooldown")
	assert.NotNil(t, loop.pushTimer, "one push is scheduled for when it ends")
}
