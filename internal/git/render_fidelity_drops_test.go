// SPDX-License-Identifier: Apache-2.0

package git

// The branch worker's side of the render-fidelity gate (render_fidelity_drops.go): a replay
// resync records its scope clean on the loop, before its caller hears of it, and a write the
// closed gate drops is counted and logged.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// replayFixture is a ledger target with a render-fidelity gate whose one scope, ConfigMaps in
// "default", was just started and has not reported.
type replayFixture struct {
	*ledgerFixture

	loop     *branchWorkerEventLoop
	gate     *RenderFidelityGate
	target   itypes.ResourceReference
	scope    ResyncScope
	revision uint64
}

func newReplayFixture(t *testing.T, slug string) *replayFixture {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")
	gate := NewRenderFidelityGate()
	f.worker.renderFidelityGate = gate
	target := itypes.NewResourceReference(ledgerTargetName, "default")
	scope := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "default")
	_, revisions := gate.Reconcile(target,
		[]itypes.CollectionKey{scope.Collection}, []itypes.CollectionKey{scope.Collection})
	require.False(t, gate.AllowsWrites(target), "precondition: the new scope has not reported")
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(loop.stopTimers)
	return &replayFixture{ledgerFixture: f, loop: loop, gate: gate, target: target, scope: scope,
		revision: revisions[scope.Collection]}
}

// replay is the replay resync the scope's stream sends once its initial list is complete.
func (f *replayFixture) replay() *ResyncRequest {
	return &ResyncRequest{
		Desired:                []manifestanalyzer.DesiredResource{desiredCMIn("keep", "default", "blue")},
		GitTargetName:          ledgerTargetName,
		GitTargetNamespace:     "default",
		Scope:                  &f.scope,
		RenderFidelityRevision: f.revision,
		Result:                 make(chan ResyncResult, 1),
	}
}

func (f *replayFixture) liveEvent(name string) {
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent(name, "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
}

func writesDropped(t *testing.T, reader *sdkmetric.ManualReader, kind, reason string) int64 {
	t.Helper()
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_git_writes_dropped_total",
		map[string]string{"kind": kind, "reason": reason})
	return got
}

// TestRenderFidelity_ALiveEventQueuedBehindAReplayResyncIsKept is the race the e2e spec for a
// rules[].sourceNamespace override lost its ConfigMap to. The stream queues its replay resync and
// then a live event behind it. The loop applies the resync, answers, and takes the event at once,
// while the caller that used to record the scope clean has not read the answer yet. The event must
// meet the gate the resync produced.
func TestRenderFidelity_ALiveEventQueuedBehindAReplayResyncIsKept(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newReplayFixture(t, "render-fidelity-replay-then-event")

	req := f.replay()
	f.loop.handleResyncRequest(req)
	f.loop.endWake(0)
	// Nothing has read req.Result: the watch layer's drain, on its own goroutine, is still behind.
	f.liveEvent("created-after-the-replay")

	assert.NotNil(t, f.loop.openWindow, "the event queued behind the replay is kept")
	assert.True(t, f.gate.AllowsWrites(f.target), "the loop recorded the scope before answering")
	assert.Zero(t, writesDropped(t, reader, writeDroppedEvent, writeDropRenderFidelityPending))
	require.NoError(t, (<-req.Result).Err)

	// The drain records the same result once it has the answer, and the gate takes it unchanged.
	status, applied := f.gate.RecordScopeClean(f.target, f.revision, f.scope.Collection)
	assert.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State)
}

// A replay resync held back by an unreachable remote is answered at once with the failure, and
// applied later with nobody waiting. Its caller saw only the failure, so it never recorded the
// scope, and the GitTarget stayed closed to live writes after the snapshot had landed. The loop
// records it when it applies.
func TestRenderFidelity_AReplayAppliedAfterAnOutageOpensTheGate(t *testing.T) {
	f := newReplayFixture(t, "render-fidelity-replay-after-outage")
	_, restoreSyncs := failSyncs(t)

	req := f.replay()
	f.loop.handleResyncRequest(req)
	f.loop.endWake(0)
	require.Error(t, (<-req.Result).Err, "the caller hears the outage, and records nothing")
	require.False(t, f.gate.AllowsWrites(f.target))

	restoreSyncs()
	fireRetry(f.loop)
	assert.Empty(t, f.loop.pendingWrites, "the retry applied the held resync")
	assert.True(t, f.gate.AllowsWrites(f.target), "applying it recorded the scope clean")
}

// A stream the plan has replaced still reports under the revision it started with. Its replay
// must not open the gate for the scope's new stream.
func TestRenderFidelity_AReplayFromARetiredStreamDoesNotOpenTheGate(t *testing.T) {
	f := newReplayFixture(t, "render-fidelity-retired-replay")
	stale := f.replay()
	_, revisions := f.gate.Reconcile(f.target,
		[]itypes.CollectionKey{f.scope.Collection}, []itypes.CollectionKey{f.scope.Collection})
	require.NotEqual(t, f.revision, revisions[f.scope.Collection], "precondition: the scope restarted")

	f.loop.handleResyncRequest(stale)
	f.loop.endWake(0)
	require.NoError(t, (<-stale.Result).Err)
	assert.False(t, f.gate.AllowsWrites(f.target), "only the current stream's replay opens the gate")
}

// TestRenderFidelity_ADroppedWriteIsCountedAndLoggedOncePerGateStatus: a write the closed gate
// drops is gone for good, so every drop is counted, and the first under each gate status is
// logged at Info with what the gate waits for. A burst of edits against one closed gate logs once.
func TestRenderFidelity_ADroppedWriteIsCountedAndLoggedOncePerGateStatus(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newReplayFixture(t, "render-fidelity-drops")
	logged := captureInfo(f.worker)

	f.liveEvent("first")
	f.liveEvent("second")
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("snapshot", "alice", ledgerTargetName)},
		CommitMode: CommitModeAtomic,
	}})

	assert.Nil(t, f.loop.openWindow, "a pending scope closes the GitTarget")
	assert.Equal(t, int64(2), writesDropped(t, reader, writeDroppedEvent, writeDropRenderFidelityPending))
	assert.Equal(t, int64(1), writesDropped(t, reader, writeDroppedAtomic, writeDropRenderFidelityPending))
	drops := logged.matching("Dropped a write")
	require.Len(t, drops, 1, "one Info line for the burst")
	assert.Contains(t, drops[0], "Waiting for 1 of 1", "it says which scopes the gate waits for")
	assert.Contains(t, drops[0], "configmaps/first")

	_, applied := f.gate.RecordScopeDivergence(f.target, f.revision, f.scope.Collection,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.True(t, applied)
	f.liveEvent("third")
	assert.Equal(t, int64(1), writesDropped(t, reader, writeDroppedEvent, writeDropRenderFidelityDiverged))
	assert.Len(t, logged.matching("Dropped a write"), 2, "a new gate status logs again")
}

// infoLines collects the worker's Info-level log lines.
type infoLines struct {
	mu    sync.Mutex
	lines []string
}

func captureInfo(w *BranchWorker) *infoLines {
	captured := &infoLines{}
	w.Log = funcr.New(func(prefix, args string) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		captured.lines = append(captured.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 0})
	return captured
}

func (c *infoLines) matching(substring string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.lines {
		if strings.Contains(line, substring) {
			out = append(out, line)
		}
	}
	return out
}
