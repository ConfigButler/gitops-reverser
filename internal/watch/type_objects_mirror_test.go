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

package watch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// recordingObjectMirror captures the calls mirrorTypeObjects / clearTypeObjects make.
type recordingObjectMirror struct {
	replacedItems   map[string]string
	replacedRV      string
	replacedKey     string
	replacedVersion string
	replaceCount    int
	deletedKey      string
}

func (r *recordingObjectMirror) ReplaceTypeObjects(
	_ context.Context, group, version, resource string, items map[string]string, rv string,
) error {
	r.replacedKey = group + "/" + resource
	r.replacedVersion = version
	r.replacedItems = items
	r.replacedRV = rv
	r.replaceCount++
	return nil
}

func (r *recordingObjectMirror) DeleteTypeObjects(_ context.Context, group, resource string) error {
	r.deletedKey = group + "/" + resource
	return nil
}

// recordingTrimmer captures the audit-log trim calls the re-anchor path makes (R1).
type recordingTrimmer struct {
	trimmedKey string
	trimmedRV  string
	trimCount  int
	err        error
}

func (r *recordingTrimmer) TrimTypeAuditLog(_ context.Context, group, resource, minRV string) error {
	r.trimmedKey = group + "/" + resource
	r.trimmedRV = minRV
	r.trimCount++
	return r.err
}

// bookmarkObject builds the terminal object a streaming-list initial stream ends with: it carries
// only the snapshot resourceVersion and the k8s.io/initial-events-end annotation marking the end
// of the initial events.
func bookmarkObject(rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetResourceVersion(rv)
	u.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	return u
}

// seamBookmarkRV is the resourceVersion streamSeam pins its synthetic checkpoint to — distinct
// from the seeded objects' rvs so a test asserting the checkpoint rv proves it came from the
// bookmark, not the last object.
const seamBookmarkRV = "777"

// streamSeam returns a watchCheckpointObjects seam that emits objs as ADDED then an
// initial-events-end bookmark (at seamBookmarkRV) — the streaming-list shape the dynamic fake
// client does not synthesize. It drives the WATCH-first checkpoint fill deterministically in tests.
func streamSeam(objs ...*unstructured.Unstructured) func(
	context.Context, schema.GroupVersionResource, metav1.ListOptions,
) (watch.Interface, error) {
	return func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (watch.Interface, error) {
		fw := watch.NewFakeWithChanSize(len(objs)+1, false)
		for _, o := range objs {
			fw.Add(o)
		}
		fw.Action(watch.Bookmark, bookmarkObject(seamBookmarkRV))
		return fw, nil
	}
}

// rejectWatchSeam returns a seam that rejects streaming-list at open, as a non-conformant
// (e.g. aggregated) backend would — the trigger that forces the LIST fallback.
func rejectWatchSeam() func(
	context.Context, schema.GroupVersionResource, metav1.ListOptions,
) (watch.Interface, error) {
	return func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (watch.Interface, error) {
		return nil, errors.New("streaming-list not supported")
	}
}

func TestMirrorTypeObjects_WatchFirstFoldsAndPinsBookmarkRV(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		streamedCM("default", "a", "10"),
		streamedCM("kube-system", "b", "11"),
	)
	mirror := &recordingObjectMirror{}

	// Capture the options the production path sends so we prove streaming-list is requested, then
	// fold the seeded objects as ADDED and end with the initial-events-end bookmark at rv 777.
	var gotOpts metav1.ListOptions
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}
	m.watchCheckpointObjects = func(
		_ context.Context, _ schema.GroupVersionResource, opts metav1.ListOptions,
	) (watch.Interface, error) {
		gotOpts = opts
		return streamSeam(streamedCM("default", "a", "10"), streamedCM("kube-system", "b", "11"))(
			context.Background(), configMapGVR, opts)
	}

	rv, err := m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	require.NoError(t, err)

	// The WATCH-first request carries the streaming-list options.
	require.NotNil(t, gotOpts.SendInitialEvents)
	assert.True(t, *gotOpts.SendInitialEvents, "the watch opts in sendInitialEvents")
	assert.Equal(t, metav1.ResourceVersionMatchNotOlderThan, gotOpts.ResourceVersionMatch)
	assert.True(t, gotOpts.AllowWatchBookmarks, "the watch allows the initial-events-end bookmark")

	assert.Equal(t, "/configmaps", mirror.replacedKey, "core group + resource identify the type")
	require.Len(t, mirror.replacedItems, 2)
	assert.Contains(t, mirror.replacedItems, "default/a")
	assert.Contains(t, mirror.replacedItems, "kube-system/b")

	// Regression: the checkpoint is pinned to the bookmark rv, not the last object's rv (11).
	assert.Equal(t, seamBookmarkRV, mirror.replacedRV, "the bookmark rv pins the checkpoint")
	assert.Equal(t, seamBookmarkRV, rv)

	// Each item is an envelope: identity + rv lifted out beside the sanitized body.
	var env objectEnvelope
	require.NoError(t, json.Unmarshal([]byte(mirror.replacedItems["default/a"]), &env))
	assert.Equal(t, "configmaps", env.Resource)
	assert.Equal(t, "v1", env.APIVersion)
	assert.Equal(t, "ConfigMap", env.Kind)
	assert.Equal(t, "default", env.Namespace)
	assert.Equal(t, "a", env.Name)
	assert.Equal(t, "10", env.ResourceVersion, "rv is lifted out of the body (sanitize strips it)")
	assert.Contains(t, string(env.Object), "ConfigMap", "the sanitized object rides along under object")
}

// TestMirrorTypeObjects_FallsBackToListOnWatchReject proves the non-conformant path: a watch that
// rejects streaming-list falls back to the consistent LIST, pinning the checkpoint to the
// collection rv (not a bookmark) and still loading every object.
func TestMirrorTypeObjects_FallsBackToListOnWatchReject(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		streamedCM("default", "a", "10"),
		streamedCM("kube-system", "b", "11"),
	)
	mirror := &recordingObjectMirror{}
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror,
		watchCheckpointObjects: rejectWatchSeam(),
	}

	rv, err := m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	require.NoError(t, err, "a non-conformant watch must fall back to LIST, not fail the sync")

	require.Len(t, mirror.replacedItems, 2, "the LIST fallback loads the full set")
	assert.Contains(t, mirror.replacedItems, "default/a")
	assert.Contains(t, mirror.replacedItems, "kube-system/b")
	assert.NotEmpty(t, rv, "the collection rv pins the fallback checkpoint")
	assert.Equal(t, rv, mirror.replacedRV)
}

// TestMirrorTypeObjects_PartialWatchWithFailedListDoesNotReplace proves the partial-watch guard:
// a watch that closes before the initial-events-end bookmark is untrustworthy, and when the LIST
// fallback also fails the sync errors with NO checkpoint pinned — never a partial one.
func TestMirrorTypeObjects_PartialWatchWithFailedListDoesNotReplace(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	dc.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list boom")
	})
	mirror := &recordingObjectMirror{}
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror,
		watchCheckpointObjects: func(
			context.Context, schema.GroupVersionResource, metav1.ListOptions,
		) (watch.Interface, error) {
			// Emit one ADDED then close WITHOUT the initial-events-end bookmark: a partial set.
			fw := watch.NewFakeWithChanSize(1, false)
			fw.Add(streamedCM("default", "a", "10"))
			fw.Stop()
			return fw, nil
		},
	}

	rv, err := m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	require.Error(t, err, "a partial watch that cannot fall back to LIST must fail the sync")
	assert.Empty(t, rv)
	assert.Zero(t, mirror.replaceCount, "no checkpoint is pinned from a partial watch")
}

// TestMirrorTypeObjects_WatchErrorEventFallsBackToList proves a watch.Error mid-stream is treated
// as untrustworthy and falls back to LIST rather than pinning the objects seen so far.
func TestMirrorTypeObjects_WatchErrorEventFallsBackToList(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror,
		watchCheckpointObjects: func(
			context.Context, schema.GroupVersionResource, metav1.ListOptions,
		) (watch.Interface, error) {
			fw := watch.NewFakeWithChanSize(2, false)
			fw.Add(streamedCM("default", "a", "10"))
			fw.Error(&unstructured.Unstructured{Object: map[string]interface{}{"message": "boom"}})
			return fw, nil
		},
	}

	rv, err := m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	require.NoError(t, err, "a watch error must fall back to the LIST path")
	require.Len(t, mirror.replacedItems, 1, "the fallback LIST loads the set")
	assert.NotEmpty(t, rv)
}

// hangingWatchSeam returns a seam whose watch opens but never emits — neither objects nor the
// initial-events-end bookmark — so the read loop is governed entirely by the context (parent
// cancel) or the stream deadline (timeout fallback).
func hangingWatchSeam() func(
	context.Context, schema.GroupVersionResource, metav1.ListOptions,
) (watch.Interface, error) {
	return func(context.Context, schema.GroupVersionResource, metav1.ListOptions) (watch.Interface, error) {
		return watch.NewFake(), nil
	}
}

// TestStreamTypeObjects_TimeoutFallsBackToList proves the wait-forever guard: a watch that opens
// but never emits the initial-events-end bookmark hits the stream deadline and falls back to the
// consistent LIST rather than hanging or pinning a partial checkpoint.
func TestStreamTypeObjects_TimeoutFallsBackToList(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror,
		watchCheckpointObjects:          hangingWatchSeam(),
		streamCheckpointTimeoutOverride: 20 * time.Millisecond,
	}

	rv, err := m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	require.NoError(t, err, "a watch that never bookmarks must fall back to LIST")
	require.Len(t, mirror.replacedItems, 1, "the fallback LIST loads the set")
	assert.NotEmpty(t, rv)
}

// TestMirrorTypeObjects_ParentCancelEndsSyncWithoutFallback proves shutdown handling: a cancelled
// parent context ends the checkpoint sync with the context error — it does NOT fall back to a LIST
// and pins no checkpoint.
func TestMirrorTypeObjects_ParentCancelEndsSyncWithoutFallback(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	mirror := &recordingObjectMirror{}
	m := &Manager{
		Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror,
		watchCheckpointObjects: hangingWatchSeam(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown before the fill runs

	rv, err := m.mirrorTypeObjects(ctx, logr.Discard(), configMapGVR)
	require.ErrorIs(t, err, context.Canceled, "a cancelled parent ends the sync")
	assert.Empty(t, rv)
	assert.Zero(t, mirror.replaceCount, "a shutdown pins no checkpoint and does not fall back to LIST")
}

// TestOpenCheckpointWatch_DefaultBuildsFromDynamicClient covers the production branch (no seam):
// the watch is opened from the dynamic client itself.
func TestOpenCheckpointWatch_DefaultBuildsFromDynamicClient(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	m := &Manager{Log: logr.Discard(), dynamicClient: dc} // no watchCheckpointObjects seam

	w, err := m.openCheckpointWatch(context.Background(), dc, configMapGVR, metav1.ListOptions{})
	require.NoError(t, err)
	require.NotNil(t, w)
	w.Stop()
}

// TestFoldStreamEvent covers the per-event accumulator: ADDED/MODIFIED fold, DELETED removes, an
// interim bookmark keeps reading, the terminal bookmark finishes with its rv, and untrustworthy
// payloads (a non-object event or a watch.Error) abandon the watch for the LIST fallback.
func TestFoldStreamEvent(t *testing.T) {
	cmA := streamedCM("default", "a", "10")

	t.Run("added folds the object", func(t *testing.T) {
		items := map[string]string{}
		rv, done, err := foldStreamEvent(
			items,
			configMapGVR,
			watch.Event{Type: watch.Added, Object: cmA},
			logr.Discard(),
		)
		require.NoError(t, err)
		assert.False(t, done)
		assert.Empty(t, rv)
		assert.Contains(t, items, "default/a")
	})

	t.Run("modified folds the object", func(t *testing.T) {
		items := map[string]string{}
		_, done, err := foldStreamEvent(
			items,
			configMapGVR,
			watch.Event{Type: watch.Modified, Object: cmA},
			logr.Discard(),
		)
		require.NoError(t, err)
		assert.False(t, done)
		assert.Contains(t, items, "default/a")
	})

	t.Run("deleted removes the object", func(t *testing.T) {
		items := map[string]string{"default/a": "x"}
		_, done, err := foldStreamEvent(
			items,
			configMapGVR,
			watch.Event{Type: watch.Deleted, Object: cmA},
			logr.Discard(),
		)
		require.NoError(t, err)
		assert.False(t, done)
		assert.NotContains(t, items, "default/a")
	})

	t.Run("interim bookmark keeps reading", func(t *testing.T) {
		interim := &unstructured.Unstructured{Object: map[string]interface{}{}}
		interim.SetResourceVersion("55")
		_, done, err := foldStreamEvent(map[string]string{}, configMapGVR,
			watch.Event{Type: watch.Bookmark, Object: interim}, logr.Discard())
		require.NoError(t, err)
		assert.False(t, done, "a bookmark without the end annotation is not the end of the initial set")
	})

	t.Run("terminal bookmark finishes with its rv", func(t *testing.T) {
		rv, done, err := foldStreamEvent(map[string]string{}, configMapGVR,
			watch.Event{Type: watch.Bookmark, Object: bookmarkObject("999")}, logr.Discard())
		require.NoError(t, err)
		assert.True(t, done)
		assert.Equal(t, "999", rv)
	})

	t.Run("non-object payload abandons the watch", func(t *testing.T) {
		_, _, err := foldStreamEvent(map[string]string{}, configMapGVR,
			watch.Event{Type: watch.Added, Object: &metav1.Status{Message: "boom"}}, logr.Discard())
		require.ErrorIs(t, err, errStreamFallback)
	})

	t.Run("error event abandons the watch", func(t *testing.T) {
		_, _, err := foldStreamEvent(map[string]string{}, configMapGVR,
			watch.Event{Type: watch.Error, Object: &metav1.Status{Message: "expired"}}, logr.Discard())
		require.ErrorIs(t, err, errStreamFallback)
	})

	t.Run("unrecognized event type abandons the watch", func(t *testing.T) {
		_, _, err := foldStreamEvent(map[string]string{}, configMapGVR,
			watch.Event{Type: watch.EventType("WEIRD"), Object: cmA}, logr.Discard())
		require.ErrorIs(t, err, errStreamFallback)
	})
}

func TestMirrorTypeObjects_NilMirrorIsNoOp(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	m := &Manager{Log: logr.Discard(), dynamicClient: dc} // ObjectMirror nil

	assert.NotPanics(t, func() {
		m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	})
}

func TestClearTypeObjects_Deletes(t *testing.T) {
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), ObjectMirror: mirror}

	m.clearTypeObjects(context.Background(), logr.Discard(), configMapGVR)

	assert.Equal(t, "/configmaps", mirror.deletedKey)
}
