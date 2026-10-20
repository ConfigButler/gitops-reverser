// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git"
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

// TestOwnerIsReadBeforeSanitize: the applier that owns an object is stamped on its live event and on
// its snapshot entry from the raw object, before sanitize strips the labels and annotations it is read
// from (docs/design/applier-echoes.md, F4).
func TestOwnerIsReadBeforeSanitize(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "cfg", "namespace": "team-a",
			"labels": map[string]any{
				"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			},
		},
	}}
	want := git.AppliedBy{Kind: "Kustomization", Namespace: "flux-system", Name: "infra"}

	event := targetWatchGitEvent(configmaps, u, string(types.OperationUpdate))
	assert.Equal(t, want, event.AppliedBy)

	var snapshot replaySnapshot
	snapshot.add(configmaps, u)
	require.Len(t, snapshot.desired, 1)
	assert.Equal(t, want, snapshot.owners[snapshot.desired[0].Resource])
}
