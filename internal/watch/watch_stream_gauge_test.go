// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const openStreamsMetric = "gitopsreverser_watch_streams_open"

// The point of this gauge: one TYPE watched across several namespaces is several SESSIONS, and
// those sessions are what the source cluster's API server actually holds. watch_types cannot
// express that, which is why a cluster admin asking "what will this cost my apiserver" had no
// answer before.
func TestOpenWatchGauge_CountsSessionsNotTypes(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{}
	m.installOpenWatchGaugeSource()
	defer m.clearOpenWatchGaugeSource()

	gitDest := types.NewResourceReference("my-target", "my-ns")
	// One type, three namespaces: three sessions.
	releases := []func(){m.trackOpenWatch(gitDest), m.trackOpenWatch(gitDest), m.trackOpenWatch(gitDest)}

	value, ok := telemetry.CollectInt64Sum(reader, openStreamsMetric, map[string]string{
		"gittarget_namespace": "my-ns", "gittarget_name": "my-target",
	})
	require.True(t, ok, "an open session must be visible to a scrape")
	assert.Equal(t, int64(3), value)

	releases[0]()

	value, ok = telemetry.CollectInt64Sum(reader, openStreamsMetric, nil)
	require.True(t, ok)
	assert.Equal(t, int64(2), value, "closing one session must decrement, not clear")
}

// A source cluster this operator no longer watches must stop publishing rather than report a
// standing zero, which would read as "connected and idle" instead of "not connected".
func TestOpenWatchGauge_LastSessionRemovesTheSeries(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{}
	m.installOpenWatchGaugeSource()
	defer m.clearOpenWatchGaugeSource()

	release := m.trackOpenWatch(types.NewResourceReference("my-target", "my-ns"))
	_, ok := telemetry.CollectInt64Sum(reader, openStreamsMetric, nil)
	require.True(t, ok)

	release()

	_, ok = telemetry.CollectInt64Sum(reader, openStreamsMetric, nil)
	assert.False(t, ok, "no open sessions must publish no series")
}

// The release is idempotent. Each session's release runs from a defer beside the watch's own Stop,
// and a double release would drive the count below the sessions actually open.
func TestOpenWatchGauge_ReleaseIsIdempotent(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{}
	m.installOpenWatchGaugeSource()
	defer m.clearOpenWatchGaugeSource()

	gitDest := types.NewResourceReference("my-target", "my-ns")
	keep := m.trackOpenWatch(gitDest)
	defer keep()
	release := m.trackOpenWatch(gitDest)

	release()
	release()

	value, ok := telemetry.CollectInt64Sum(reader, openStreamsMetric, nil)
	require.True(t, ok)
	assert.Equal(t, int64(1), value, "a repeated release must not consume another session's count")
}

// The series is per source cluster, which is the axis the question is asked on.
func TestOpenWatchGauge_SeparatesSourceClusters(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{}
	m.installOpenWatchGaugeSource()
	defer m.clearOpenWatchGaugeSource()

	defer m.trackOpenWatch(types.NewResourceReference("target-a", "ns-a"))()
	defer m.trackOpenWatch(types.NewResourceReference("target-b", "ns-b"))()

	value, ok := telemetry.CollectInt64Sum(reader, openStreamsMetric, map[string]string{
		"gittarget_name": "target-a",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}
