/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const branchWorkerQueueDepthMetric = "gitopsreverser_branch_worker_queue_depth"

func newMetricsTestWorker() *BranchWorker {
	return &BranchWorker{
		GitProviderRef:       "test-provider",
		GitProviderNamespace: "test-ns",
		Branch:               "main",
		Log:                  logr.Discard(),
		ctx:                  context.Background(),
		contentWriter:        newContentWriter(itypes.SensitiveResourcePolicy{}),
		eventQueue:           make(chan WorkItem, branchWorkerQueueSize),
		branchBufferMaxBytes: DefaultBranchBufferMaxBytes,
	}
}

func queueDepthLabels() map[string]string {
	return map[string]string{
		"provider_namespace": "test-ns",
		"provider_name":      "test-provider",
		"branch":             "main",
	}
}

// recordQueueDepth must report 0 for a freshly drained worker: empty queue and
// no retained unpushed work.
func TestRecordQueueDepth_DrainedReportsZero(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.recordQueueDepth()

	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample")
	assert.Equal(t, int64(0), depth)
}

// recordQueueDepth must count both accepted-but-unhandled items (in flight) and
// the retained unpushed-work flag, so the gauge reflects work the channel length
// alone cannot see — including an item already dequeued and being processed.
func TestRecordQueueDepth_CountsInflightAndRetained(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	// Two items accepted but not yet fully handled, plus retained unpushed work.
	w.inflightItems.Store(2)
	w.hasUnpushedWork.Store(true)

	w.recordQueueDepth()

	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample")
	assert.Equal(t, int64(3), depth)
}

// recordQueueDepth must still report > 0 for an item that has been dequeued from
// the channel but is still being handled — the exact window where len(eventQueue)
// would read 0 and a drain gate could be falsely satisfied mid-commit.
func TestRecordQueueDepth_InflightItemDequeuedButUnhandled(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	// Simulate the loop having received the item (channel empty) while it is
	// still being processed: inflight accounts for it, retained flag not yet set.
	w.inflightItems.Store(1)
	w.hasUnpushedWork.Store(false)

	w.recordQueueDepth()

	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample")
	assert.Equal(t, int64(1), depth, "an in-flight item must keep depth > 0 even with an empty channel")
}

// enqueueRequest must account for the accepted item in inflightItems but must
// NOT publish the depth gauge itself: the gauge is last-writer-wins, so an
// enqueue goroutine that raced the loop's drain could latch a stale value and
// hang the restart drain gate. Publication is the loop's job; once the loop
// observes the inflight item it reports the non-zero depth.
func TestEnqueueRequest_AccountsInflightWithoutPublishing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.enqueueRequest(&WriteRequest{
		Events:     []Event{{}},
		CommitMode: CommitModePerEvent,
	})

	// The item is accounted for, but enqueue did not touch the gauge.
	assert.Equal(t, int64(1), w.inflightItems.Load())
	_, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	assert.False(t, ok, "enqueue must not publish the depth gauge; only the loop does")

	// The loop's publication (modelled here by a direct record) then reports the
	// inflight item as non-zero depth.
	w.recordQueueDepth()
	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample after the loop publishes")
	assert.Equal(t, int64(1), depth)
}

// On shutdown the loop must drain items still buffered on eventQueue so the depth
// gauge settles to 0. Each buffered item was counted into inflightItems at
// enqueue; left undrained, the exiting worker's final publish would latch a
// non-zero depth that never clears.
func TestHandleShutdown_DrainsBufferedItemsToZeroDepth(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	// Two write requests accepted onto the queue but never handled by the loop.
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})
	require.Equal(t, int64(2), w.inflightItems.Load())

	loop := newBranchWorkerEventLoop(w, time.Second)
	loop.handleShutdown()
	// run() publishes once more after handleShutdown; model that here.
	loop.syncQueueDepthMetric()

	assert.Equal(t, int64(0), w.inflightItems.Load(), "buffered items must be drained from the inflight count")
	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample after shutdown")
	assert.Equal(t, int64(0), depth, "a drained, exiting worker must publish depth 0")
}

// A CommitRequest attach still buffered at shutdown is fire-and-forget: it is
// simply drained from the inflight count (the controller re-sends on its next
// poll), so the exiting worker's depth gauge settles to 0.
func TestHandleShutdown_DrainsBufferedAttach(t *testing.T) {
	_, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.EnqueueAttach(&AttachCommitRequest{Namespace: "default", Name: "save", Author: "alice"})
	require.Equal(t, int64(1), w.inflightItems.Load())

	loop := newBranchWorkerEventLoop(w, time.Second)
	loop.handleShutdown()

	assert.Equal(t, int64(0), w.inflightItems.Load(),
		"a buffered attach must be drained from the inflight count on shutdown")
}
