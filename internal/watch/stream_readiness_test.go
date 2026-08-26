// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestStreamSummaryForTypes_AggregatesByType(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []types.CellKey{
		types.CellKeyFor(configmaps, "a"),
		types.CellKeyFor(configmaps, "b"),
		types.CellKeyFor(secrets, "a"),
	}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(configmaps, "a"): {state: StreamStateStreaming},
		types.CellKeyFor(configmaps, "b"): {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		types.CellKeyFor(secrets, "a"):    {state: StreamStateStreaming},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Ready)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonReplaying, summary.Reason)
	assert.Equal(t, []string{"configmaps"}, summary.PendingSample)
}

func TestStreamSummaryForTypes_BlockedOutranksReplaying(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []types.CellKey{types.CellKeyFor(configmaps, ""), types.CellKeyFor(secrets, "")}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(configmaps, ""): {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		types.CellKeyFor(secrets, ""):    {state: StreamStateBlocked, reason: StreamReasonWatchError},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Blocked)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonWatchError, summary.Reason)
	assert.False(t, summary.StreamsRunning())
}

// The regression this keying exists to prevent. A rule can match two SERVED VERSIONS of one
// resource (a `*` apiVersions wildcard over, say, autoscaling/v1 and autoscaling/v2), while the
// declared set runs exactly one stream for that cell. Expecting a per-version stream meant
// expecting one that by construction never exists, and the rule reported permanently
// not-ready while its stream ran perfectly.
func TestStreamSummaryForTypes_TwoServedVersionsAreOneStream(t *testing.T) {
	v1 := schema.GroupVersionResource{Group: "autoscaling", Version: "v1", Resource: "horizontalpodautoscalers"}
	v2 := schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	expected := []types.CellKey{types.CellKeyFor(v1, "team-a"), types.CellKeyFor(v2, "team-a")}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(v2, "team-a"): {state: StreamStateStreaming},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 1, summary.Total, "one cell is one stream, whatever versions serve it")
	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning())
}
