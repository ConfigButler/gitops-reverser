// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// barrierList lists objects with the given resourceVersions, two to a page.
func barrierList(rvs ...string) func(
	context.Context, schema.GroupVersionResource, string, metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	return func(
		_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
	) (*unstructured.UnstructuredList, error) {
		start := 0
		if opts.Continue != "" {
			start = 2
		}
		list := &unstructured.UnstructuredList{}
		for i := start; i < len(rvs) && i < start+2; i++ {
			item := unstructured.Unstructured{}
			item.SetName("cm")
			item.SetResourceVersion(rvs[i])
			list.Items = append(list.Items, item)
		}
		if start == 0 && len(rvs) > 2 {
			list.SetContinue("next")
		}
		return list, nil
	}
}

// A stream answers a barrier once it has handled the newest write its collection held when the
// barrier was taken, an event or a bookmark, and not before.
func TestApplyBarrier_AStreamAnswersOnceItHandledTheNewestWrite(t *testing.T) {
	gitDest := types.NewResourceReference("target", "team-a")
	key := targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "team-a"}
	manager := &Manager{targetWatchList: barrierList("12", "40", "7")}
	manager.trackStreamProgress(gitDest, key)
	manager.advanceStreamProgress(gitDest, key, "30")

	var delivered atomic.Int32
	streams, err := manager.ApplyBarrier(context.Background(), gitDest, func() { delivered.Add(1) })
	require.NoError(t, err)
	assert.Equal(t, 1, streams)
	assert.Zero(t, delivered.Load(), "the stream has handled 30, and the newest write, on the second page, is 40")

	manager.advanceStreamProgress(gitDest, key, "39")
	assert.Zero(t, delivered.Load())
	manager.advanceStreamProgress(gitDest, key, "41")
	assert.Equal(t, int32(1), delivered.Load(), "a bookmark past the mark answers it")
	manager.advanceStreamProgress(gitDest, key, "50")
	assert.Equal(t, int32(1), delivered.Load(), "once")
}

// A stream that already handled the newest write answers at once, and a collection with no objects
// asks for nothing.
func TestApplyBarrier_AStreamThatIsCaughtUpAnswersAtOnce(t *testing.T) {
	gitDest := types.NewResourceReference("target", "team-a")
	key := targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "team-a"}
	for _, rvs := range [][]string{{"12", "40"}, {}} {
		manager := &Manager{targetWatchList: barrierList(rvs...)}
		manager.trackStreamProgress(gitDest, key)
		manager.advanceStreamProgress(gitDest, key, "40")

		var delivered atomic.Int32
		streams, err := manager.ApplyBarrier(context.Background(), gitDest, func() { delivered.Add(1) })
		require.NoError(t, err)
		assert.Equal(t, 1, streams)
		assert.Equal(t, int32(1), delivered.Load(), "%v", rvs)
	}
}

// A source whose resourceVersions are not numbers fails the barrier, and a GitTarget with no
// running stream has nobody to answer.
func TestApplyBarrier_NonNumericResourceVersionsFailAndNoStreamsAnswerNothing(t *testing.T) {
	gitDest := types.NewResourceReference("target", "team-a")
	key := targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "team-a"}
	manager := &Manager{targetWatchList: barrierList("abc")}

	streams, err := manager.ApplyBarrier(context.Background(), gitDest, func() {})
	require.NoError(t, err)
	assert.Zero(t, streams, "no stream runs")

	manager.trackStreamProgress(gitDest, key)
	_, err = manager.ApplyBarrier(context.Background(), gitDest, func() {})
	require.Error(t, err)
}

// A stream replaced by a newer one for the same collection keeps the newer one's progress when it
// stops.
func TestApplyBarrier_AStoppedStreamDoesNotRemoveItsReplacement(t *testing.T) {
	gitDest := types.NewResourceReference("target", "team-a")
	key := targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "team-a"}
	manager := &Manager{}
	old := manager.trackStreamProgress(gitDest, key)
	newer := manager.trackStreamProgress(gitDest, key)
	manager.untrackStreamProgress(gitDest, key, old)
	assert.Same(t, newer, manager.streamProgress[gitDest.Key()][key.Collection()])
	manager.untrackStreamProgress(gitDest, key, newer)
	assert.Empty(t, manager.streamProgress)
}
