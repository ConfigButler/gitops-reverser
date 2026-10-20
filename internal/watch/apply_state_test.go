// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestSnapshotsDelivered_EveryCollectionMustBeStreaming: a GitTarget's initial snapshots are
// delivered once every collection it watches streams, which a stream reaches only once the worker
// took its snapshot. One still replaying, blocked, or not started yet holds the answer back.
func TestSnapshotsDelivered_EveryCollectionMustBeStreaming(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []types.CollectionKey{types.CollectionKeyFor(configmaps, ""), types.CollectionKeyFor(secrets, "")}

	assert.True(t, snapshotsDelivered(nil, nil), "a GitTarget with no collection has nothing to deliver")
	assert.False(t, snapshotsDelivered(expected, nil), "no stream started")
	for _, state := range []StreamState{StreamStateReplaying, StreamStateBlocked} {
		assert.False(t, snapshotsDelivered(expected, map[types.CollectionKey]targetStreamStatus{
			types.CollectionKeyFor(configmaps, ""): {state: StreamStateStreaming},
			types.CollectionKeyFor(secrets, ""):    {state: state},
		}), state)
	}
	assert.True(t, snapshotsDelivered(expected, map[types.CollectionKey]targetStreamStatus{
		types.CollectionKeyFor(configmaps, ""): {state: StreamStateStreaming},
		types.CollectionKeyFor(secrets, ""):    {state: StreamStateStreaming},
	}))
}
