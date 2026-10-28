// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// The tests here pin docs/design/concurrent-author-windows.md: a window per author while their
// footprints are proven not to overlap, and serial everywhere else.

// newAdmissionFixture is an echo fixture whose folder the worker has bootstrapped with a commit of its
// own, so writes there can be proven, and whose windows stay open for an hour unless something closes
// them. Pushes are held.
func newAdmissionFixture(t *testing.T, slug string, files map[string]string) *echoFixture {
	t.Helper()
	f := newEchoFixtureWith(t, slug, configv1alpha3.PruneOnEvent,
		map[string]string{"x": "x1", "z": "z1", "q": "q1"}, files)
	holdPushes(f.loop)
	f.writeWindow(echoEvent("q", "q2")) // the zero window commits at once, with the bootstrap files
	require.Empty(t, f.loop.windows.all())
	f.loop.defaultWindow = commitWindowDefaults{idleTimeout: time.Hour, maxDuration: time.Hour}
	return f
}

func (f *echoFixture) writeBy(author, name, value string) {
	event := echoEvent(name, value)
	event.UserInfo.Username = author
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}}})
}

func authorsOf(windows []*openWindow) []string {
	var authors []string
	for _, w := range windows {
		authors = append(authors, w.Author)
	}
	return authors
}

func TestWindowAdmission_AuthorsOfDisjointFilesKeepAWindowEach(t *testing.T) {
	f := newAdmissionFixture(t, "admission-disjoint", nil)

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("alice", "x", "x3")

	open := f.loop.windows.all()
	require.Equal(t, []string{"alice", "bob"}, authorsOf(open), "oldest first")
	require.NotNil(t, open[0].footprint)
	assert.Equal(t, []string{echoFile("x")}, open[0].footprint.paths)
	assert.Equal(t, []string{echoFile("z")}, open[1].footprint.paths)
	assert.Less(t, open[0].seq, open[1].seq)

	// Bob's window closes first; commits land in close order.
	open[1].timers.maxAt = time.Now().Add(-time.Millisecond)
	f.loop.closeOrArmWindow()
	f.loop.endWake(0)
	require.Equal(t, []string{"alice"}, authorsOf(f.loop.windows.all()))
	f.loop.finalizeOpenWindowWithReason(windowFinalizeReasonShutdown)
	f.loop.endWake(0)
	f.loop.pushPending()

	assert.Equal(
		t,
		[]string{"bob", "alice"},
		splitLines(gitIn(t, f.repoDir, "log", "--reverse", "--format=%an", "-2", "main")),
	)
	assert.Equal(t, echoDocument("x", "x3"), f.onMain(echoFile("x")))
	assert.Equal(t, echoDocument("z", "z2"), f.onMain(echoFile("z")))
}

func TestWindowAdmission_AWriteToAFileAnotherWindowHoldsClosesThatWindowFirst(t *testing.T) {
	f := newAdmissionFixture(t, "admission-conflict", nil)
	reader := captureInfo(f.worker)

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("carol", "x", "x3")

	assert.Equal(t, []string{"bob", "carol"}, authorsOf(f.loop.windows.all()))
	require.Len(t, f.loop.pendingWrites, 2, "the bootstrap write, and alice's window")
	assert.Equal(t, "alice", f.loop.pendingWrites[1].Author(), "decided before carol's write opened a window")
	assert.Len(
		t,
		reader.matching(`"msg"="Finalizing open commit window" "reason"="path-conflict" "windowAuthor"="alice"`),
		1,
	)
}

func TestWindowAdmission_AWriteThatCannotBeProvenStaysSerial(t *testing.T) {
	shared := echoDocument("s1", "a") + "---\n" + echoDocument("s2", "b")
	f := newAdmissionFixture(t, "admission-unproven",
		map[string]string{echoTarget + "/default/configmaps/shared.yaml": shared})

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "s1", "c")

	open := f.loop.windows.all()
	require.Equal(t, []string{"bob"}, authorsOf(open), "a document in a shared file closes every other window")
	require.NotNil(t, open[0].footprint)
	assert.False(t, open[0].footprint.known())
	assert.Equal(t, "alice", f.loop.pendingWrites[len(f.loop.pendingWrites)-1].Author())

	// And an unknown window closes before a write is admitted beside it.
	f.writeBy("carol", "z", "z2")
	assert.Equal(t, []string{"carol"}, authorsOf(f.loop.windows.all()))
}

func TestWindowAdmission_AWorkerThatFellBackHoldsOneWindow(t *testing.T) {
	f := newAdmissionFixture(t, "admission-single", nil)
	f.loop.singleWindow = true

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")

	assert.Equal(t, []string{"bob"}, authorsOf(f.loop.windows.all()))
}

func TestWindowAdmission_ClosingOneWindowArmsTheTimerForTheOthers(t *testing.T) {
	f := newAdmissionFixture(t, "admission-rearm", nil)
	clock := useTestClock(f.worker)

	f.writeBy("alice", "x", "x2")
	clock.advance(time.Minute)
	f.writeBy("bob", "z", "z2")
	open := f.loop.windows.all()
	require.Len(t, open, 2)
	aliceAt, _ := open[0].closeAt()
	bobAt, _ := open[1].closeAt()
	require.Equal(t, aliceAt, f.loop.commitTimerAt, "armed for the earliest deadline")

	f.loop.finalizeWindow(open[0], windowFinalizeReasonAttachNext)

	require.NotNil(t, f.loop.commitTimer, "bob's window still has a deadline to keep")
	assert.Equal(t, bobAt, f.loop.commitTimerAt)
}

func TestWindowAdmission_ACommitOutsideItsFootprintIsRefusedAndTheWorkerFallsBack(t *testing.T) {
	f := newAdmissionFixture(t, "admission-violation", nil)
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	head := gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD")

	pendingWrite, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{echoEvent("x", "x9")})
	require.NoError(t, err)
	rev, ok := f.loop.currentRevision(echoTarget, "default")
	require.True(t, ok)
	// A proof that names the wrong file, as a footprint bug would.
	pendingWrite.footprint = &footprint{paths: []string{echoFile("z")}, revision: rev}
	id := commitRequestID{Namespace: "default", Name: crName, UID: "uid-" + crName}
	pendingWrite.CommitRequest = &id

	err = f.worker.commitPendingWrites([]PendingWrite{*pendingWrite})
	var violation *footprintViolationError
	require.ErrorAs(t, err, &violation)
	assert.Equal(t, []string{echoFile("x")}, violation.outside)
	require.ErrorIs(t, err, errWriteFailed, "terminal for the write")
	assert.Equal(t, head, gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD"), "nothing was committed")
	assert.Empty(t, gitIn(t, f.worker.repoPath(), "status", "--porcelain"), "and the worktree was undone")

	f.loop.pendingWrites = []PendingWrite{*pendingWrite}
	f.loop.settleFailed(0, err)

	assert.True(t, f.loop.singleWindow, "one window at a time for the rest of the worker's life")
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(echoTarget, "default")}, asked,
		"a resync brings Git back to the cluster")
	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), echoFile("x"), "the save says which path broke the footprint")
}

// TestWindowAdmission_AFallbackLeavesOneWindowOpen pins the fallback closing the windows the wrong
// rules admitted side by side: every one but the newest, oldest first, with footprint_fallback.
func TestWindowAdmission_AFallbackLeavesOneWindowOpen(t *testing.T) {
	f := newAdmissionFixture(t, "admission-fallback-closes", nil)
	reader := captureInfo(f.worker)
	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("carol", "q", "q3")
	require.Len(t, f.loop.windows.all(), 3)
	decided := len(f.loop.pendingWrites)

	refused, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{echoEvent("w", "w1")})
	require.NoError(t, err)
	f.loop.pendingWrites = append(f.loop.pendingWrites, *refused)
	violation := &footprintViolationError{outside: []string{echoFile("w")}, footprint: footprint{}}
	f.loop.settleFailed(len(f.loop.pendingWrites)-1, fmt.Errorf("%w: %w", errWriteFailed, violation))

	require.True(t, f.loop.singleWindow)
	assert.Equal(t, []string{"carol"}, authorsOf(f.loop.windows.all()), "at most one window stays open")
	require.Len(t, f.loop.pendingWrites, decided+2, "alice's and bob's windows are decided")
	assert.Equal(t, "alice", f.loop.pendingWrites[decided].Author(), "oldest first")
	assert.Equal(t, "bob", f.loop.pendingWrites[decided+1].Author())
	assert.Len(t, reader.matching(`"msg"="Finalizing open commit window" "reason"="footprint-fallback"`), 2)
}

func TestFootprint_OverlapUnionAndOutside(t *testing.T) {
	a := footprint{paths: []string{"a", "b"}}
	b := footprint{paths: []string{"c"}}
	unknown := footprint{unknown: "why"}

	assert.False(t, a.overlaps(b))
	assert.True(t, a.overlaps(footprint{paths: []string{"b"}}))
	assert.True(t, a.overlaps(unknown), "unknown overlaps everything")
	assert.True(t, unknown.overlaps(footprint{}))
	assert.Equal(t, []string{"a", "b", "c"}, a.union(b).paths)
	assert.False(t, a.union(unknown).known())
	assert.Equal(t, []string{"d"}, a.outside([]string{"a", "d"}))
	assert.Equal(t, "[a b]", a.String())
	assert.Equal(t, "unknown: why", unknown.String())
	assert.Equal(t, "unproven", footprintString(nil))
}
