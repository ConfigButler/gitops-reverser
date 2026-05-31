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

// enqueueRequest must publish a non-zero depth as soon as the item lands on the
// queue, closing the race where a poller reads a stale drained value.
func TestEnqueueRequest_RecordsDepth(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.enqueueRequest(&WriteRequest{
		Events:     []Event{{}},
		CommitMode: CommitModePerEvent,
	})

	depth, ok := telemetry.CollectInt64Sum(reader, branchWorkerQueueDepthMetric, queueDepthLabels())
	require.True(t, ok, "expected a branch_worker_queue_depth sample after enqueue")
	assert.Equal(t, int64(1), depth)
}
