// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

// An observable gauge must read its source AT COLLECTION, not at the moment the source was
// installed. That is the whole property: a value produced when the producer last got around to
// publishing is a value that freezes during the stall the gauge exists to report.
func TestObservableGauge_ReadsTheSourceAtCollectionTime(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	depth := int64(0)
	SetGaugeSource(GaugeGitQueueDepth, func() []GaugeSample {
		return []GaugeSample{{Value: depth, Attrs: []attribute.KeyValue{attribute.String("branch", "main")}}}
	})
	defer SetGaugeSource(GaugeGitQueueDepth, nil)

	value, ok := CollectInt64Sum(reader, "gitopsreverser_git_queue_depth", map[string]string{"branch": "main"})
	require.True(t, ok)
	require.Equal(t, int64(0), value)

	// Nothing republishes. The next scrape must still see the new value.
	depth = 7

	value, ok = CollectInt64Sum(reader, "gitopsreverser_git_queue_depth", map[string]string{"branch": "main"})
	require.True(t, ok)
	assert.Equal(t, int64(7), value)
}

// A gauge with no installed source publishes no series. That is the honest answer for a producer
// that is not running, and it keeps a cleared source from leaving a last value latched forever.
func TestObservableGauge_NoSourcePublishesNoSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	SetGaugeSource(GaugeGitQueueDepth, func() []GaugeSample {
		return []GaugeSample{{Value: 3}}
	})
	_, ok := CollectInt64Sum(reader, "gitopsreverser_git_queue_depth", nil)
	require.True(t, ok, "the source should be publishing while installed")

	SetGaugeSource(GaugeGitQueueDepth, nil)

	_, ok = CollectInt64Sum(reader, "gitopsreverser_git_queue_depth", nil)
	assert.False(t, ok, "a cleared source must stop publishing rather than latch its last value")
}

// One source may publish many series, which is what lets a single callback cover every branch
// worker without the exporter knowing anything about workers.
func TestObservableGauge_SourceMayPublishManySeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	SetGaugeSource(GaugeWatchPlanDirtyTargets, func() []GaugeSample {
		return []GaugeSample{
			{Value: 2, Attrs: []attribute.KeyValue{attribute.String("shard", "a")}},
			{Value: 5, Attrs: []attribute.KeyValue{attribute.String("shard", "b")}},
		}
	})
	defer SetGaugeSource(GaugeWatchPlanDirtyTargets, nil)

	value, ok := CollectInt64Sum(reader, "gitopsreverser_watch_plan_dirty_targets",
		map[string]string{"shard": "b"})
	require.True(t, ok)
	assert.Equal(t, int64(5), value)
}
