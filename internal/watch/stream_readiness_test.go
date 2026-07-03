// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStreamSummaryForTypes_AggregatesByGVR(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []targetWatchKey{
		{GVR: configmaps, Namespace: "a"},
		{GVR: configmaps, Namespace: "b"},
		{GVR: secrets, Namespace: "a"},
	}
	states := map[targetWatchKey]targetStreamStatus{
		{GVR: configmaps, Namespace: "a"}: {state: StreamStateStreaming},
		{GVR: configmaps, Namespace: "b"}: {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		{GVR: secrets, Namespace: "a"}:    {state: StreamStateStreaming},
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
	expected := []targetWatchKey{{GVR: configmaps}, {GVR: secrets}}
	states := map[targetWatchKey]targetStreamStatus{
		{GVR: configmaps}: {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		{GVR: secrets}:    {state: StreamStateBlocked, reason: StreamReasonWatchError},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Blocked)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonWatchError, summary.Reason)
	assert.False(t, summary.StreamsRunning())
}
