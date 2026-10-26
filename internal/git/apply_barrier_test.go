// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// fakeBarrier is the watch side of a barrier with one stream per GitTarget: it keeps each stream's
// deliver for the test to call, or fails.
type fakeBarrier struct {
	mu       sync.Mutex
	delivers []func()
	err      error
}

func (f *fakeBarrier) take(_ context.Context, _ itypes.ResourceReference, deliver func()) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivers = append(f.delivers, deliver)
	return 1, nil
}

func (f *fakeBarrier) deliverAll() {
	f.mu.Lock()
	delivers := f.delivers
	f.delivers = nil
	f.mu.Unlock()
	for _, deliver := range delivers {
		deliver()
	}
}

// dispatchQueued hands every item on the worker's queue to the loop, as run() would.
func dispatchQueued(loop *branchWorkerEventLoop) {
	for {
		select {
		case item := <-loop.w.eventQueue:
			loop.dispatch(item)
			loop.w.releaseInflightItem()
		default:
			return
		}
	}
}

// completedWithBarrier is alpha's applier with an attempt that completed, its window waiting for a
// barrier whose streams have registered and not yet delivered.
func completedWithBarrier(t *testing.T, barrier *fakeBarrier) (*branchWorkerEventLoop, *applierState) {
	t.Helper()
	loop, req := sentAttempt(t)
	loop.w.applyBarrier = barrier.take
	report(req, ApplySucceeded)
	loop.serviceAppliers()
	state := loop.appliers[applierRef("alpha")]
	require.NotNil(t, state.window)
	require.Eventually(t, func() bool {
		dispatchQueued(loop)
		return state.window.barrier == nil || state.window.barrier.expected >= 0
	}, 5*time.Second, 10*time.Millisecond, "the barrier's registration reaches the loop")
	return loop, state
}

func barriers(t *testing.T, reader *sdkmetric.ManualReader, outcome string) int64 {
	t.Helper()
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_apply_window_barriers_total",
		map[string]string{"outcome": outcome})
	return got
}

// A window whose operation completed stays open past its grace while the watch has not delivered
// every write made before the apply completed, so the applier's write is still judged inside it; once
// every stream delivered, it closes.
func TestApplyBarrier_HoldsTheWindowUntilEveryStreamDelivered(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	barrier := &fakeBarrier{}
	loop, state := completedWithBarrier(t, barrier)
	require.NotNil(t, state.window.barrier, "the window waits for the watch")
	assert.Equal(t, 1, state.window.barrier.expected)

	state.window.closesAt = time.Now().Add(-time.Millisecond)
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "an API edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"past its grace, the window still judges the applier's write: withheld over the edit")

	barrier.deliverAll()
	dispatchQueued(loop)
	assert.Nil(t, state.window.barrier, "every stream delivered")
	assert.Equal(t, int64(1), barriers(t, reader, barrierMet))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"with its grace over and the barrier met, the window is closed")
}

// A window waits for the watch at most applyWindowBarrierLimit. Past it, it closes, and the miss is
// counted.
func TestApplyBarrier_TheWindowClosesAtTheLimit(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, state := completedWithBarrier(t, &fakeBarrier{})
	require.NotNil(t, state.window.barrier)

	past := time.Now().Add(-time.Millisecond)
	state.window.closesAt, state.window.barrier.until = past, past
	loop.reportApplyWindows(time.Now())
	assert.Nil(t, state.window.barrier)
	assert.Equal(t, int64(1), barriers(t, reader, barrierTimedOut))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "the window is closed")
}

// A barrier that cannot be taken leaves the window its grace alone.
func TestApplyBarrier_AFailedBarrierLeavesTheGrace(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, state := completedWithBarrier(t, &fakeBarrier{err: errors.New("the source cannot be listed")})

	assert.Nil(t, state.window.barrier)
	assert.Equal(t, int64(1), barriers(t, reader, barrierFailed))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "within the grace")
	state.window.closesAt = time.Now().Add(-time.Millisecond)
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "after it")
}

// An answer for a barrier the window no longer waits for changes nothing.
func TestApplyBarrier_AnAnswerForAnotherBarrierIsDropped(t *testing.T) {
	barrier := &fakeBarrier{}
	loop, state := completedWithBarrier(t, barrier)
	wait := state.window.barrier
	require.NotNil(t, wait)

	loop.handleApplyBarrier(&ApplyBarrier{Applier: state.ref, Token: wait.token + 1, Passed: true})
	assert.Same(t, wait, state.window.barrier)
	assert.Zero(t, wait.passed)
}
