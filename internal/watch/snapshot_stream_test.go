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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var configMapGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

// streamedCM is an unstructured ConfigMap an initial-events stream would replay.
func streamedCM(namespace, name, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"data":       map[string]interface{}{"k": "v"},
	}}
	u.SetResourceVersion(rv)
	return u
}

// initialEventsEndBookmark is the synthetic bookmark the API server sends to mark the
// end of the initial event set, carrying the consistent resourceVersion.
func initialEventsEndBookmark(rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	u.SetResourceVersion(rv)
	return u
}

// fakeStreamingClient returns a dynamic client whose every Watch returns a fresh fake
// watcher preloaded by load(action). Buffered, non-blocking channels let a test stage
// the whole stream (adds, bookmark, optional close) up front and read it deterministically.
func fakeStreamingClient(load func(action clienttesting.Action, fw *watch.FakeWatcher)) dynamic.Interface {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependWatchReactor("*", func(action clienttesting.Action) (bool, watch.Interface, error) {
		fw := watch.NewFakeWithChanSize(32, false)
		load(action, fw)
		return true, fw, nil
	})
	return client
}

// A healthy stream folds every initial ADDED object into the desired set and returns at
// the initial-events-end bookmark with the pinned resourceVersion.
func TestStreamInitialEvents_FoldsAddsUntilBookmark(t *testing.T) {
	dc := fakeStreamingClient(func(_ clienttesting.Action, fw *watch.FakeWatcher) {
		fw.Add(streamedCM("default", "a", "10"))
		fw.Add(streamedCM("default", "b", "11"))
		fw.Action(watch.Bookmark, initialEventsEndBookmark("12"))
	})

	desired, rv, err := (&Manager{}).streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.NoError(t, err)
	assert.Equal(t, "12", rv, "the bookmark's resourceVersion pins the set")
	require.Len(t, desired, 2)
	names := []string{desired[0].Resource.Name, desired[1].Resource.Name}
	assert.ElementsMatch(t, []string{"a", "b"}, names)
	assert.NotNil(t, desired[0].Object, "each desired entry carries its sanitized object")
}

// A stream that closes before its bookmark is an incomplete initial sync, so it errors
// rather than returning a truncated set the caller might mistake for the full cluster.
func TestStreamInitialEvents_ClosedBeforeBookmarkErrors(t *testing.T) {
	dc := fakeStreamingClient(func(_ clienttesting.Action, fw *watch.FakeWatcher) {
		fw.Add(streamedCM("default", "a", "10"))
		fw.Stop() // channel closes before any initial-events-end bookmark
	})

	_, _, err := (&Manager{}).streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed before the initial-events-end bookmark")
}

// A watch.Error event during the initial sync aborts the stream.
func TestStreamInitialEvents_WatchErrorAborts(t *testing.T) {
	dc := fakeStreamingClient(func(_ clienttesting.Action, fw *watch.FakeWatcher) {
		fw.Error(&metav1.Status{Status: "Failure", Message: "boom"})
	})

	_, _, err := (&Manager{}).streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error event")
}

// A server that rejects sendInitialEvents (an aggregated apiserver without WatchList)
// falls back to one consistent LIST for that type, rather than aborting the snapshot.
func TestStreamInitialEvents_FallsBackToListWhenStreamingUnsupported(t *testing.T) {
	scheme := runtime.NewScheme()
	dc := dynamicfake.NewSimpleDynamicClient(scheme,
		streamedCM("default", "via-list-1", "10"),
		streamedCM("default", "via-list-2", "11"),
	)
	dc.PrependWatchReactor("*", func(_ clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewInvalid(
			schema.GroupKind{Group: "meta.k8s.io", Kind: "ListOptions"}, "",
			field.ErrorList{field.Forbidden(
				field.NewPath("sendInitialEvents"), "forbidden unless the WatchList feature gate is enabled")},
		)
	})

	desired, _, err := (&Manager{Log: logr.Discard()}).
		streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.NoError(t, err, "a non-streaming server must fall back to a consistent list, not abort")
	names := make([]string, len(desired))
	for i, d := range desired {
		names[i] = d.Resource.Name
	}
	assert.ElementsMatch(t, []string{"via-list-1", "via-list-2"}, names)
}

// A transient (non-streaming-related) Watch error still aborts — the fallback is only
// for servers that genuinely cannot stream, never a mask for a flaky API surface.
func TestStreamInitialEvents_TransientWatchErrorAborts(t *testing.T) {
	scheme := runtime.NewScheme()
	dc := dynamicfake.NewSimpleDynamicClient(scheme)
	dc.PrependWatchReactor("*", func(_ clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	_, _, err := (&Manager{Log: logr.Discard()}).
		streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.Error(t, err, "a transient watch error must abort, not fall back")
}

// A non-terminal bookmark (no initial-events-end annotation) is a progress notification
// and is ignored; the stream keeps reading until the real terminal bookmark.
func TestStreamInitialEvents_IgnoresNonTerminalBookmark(t *testing.T) {
	dc := fakeStreamingClient(func(_ clienttesting.Action, fw *watch.FakeWatcher) {
		progress := &unstructured.Unstructured{Object: map[string]interface{}{}}
		progress.SetResourceVersion("5")
		fw.Action(watch.Bookmark, progress) // no terminal annotation
		fw.Add(streamedCM("default", "a", "10"))
		fw.Action(watch.Bookmark, initialEventsEndBookmark("12"))
	})

	desired, rv, err := (&Manager{}).streamInitialEvents(context.Background(), dc, configMapGVR, "default")
	require.NoError(t, err)
	assert.Equal(t, "12", rv)
	require.Len(t, desired, 1)
}

// joinSnapshotStreams unions every stream's desired set and pins the revision to the max
// bookmark across types.
func TestJoinSnapshotStreams_UnionsAndPinsMaxRevision(t *testing.T) {
	dc := fakeStreamingClient(func(action clienttesting.Action, fw *watch.FakeWatcher) {
		switch action.GetNamespace() {
		case "team-a":
			fw.Add(streamedCM("team-a", "a", "10"))
			fw.Action(watch.Bookmark, initialEventsEndBookmark("15"))
		case "team-b":
			fw.Add(streamedCM("team-b", "b", "20"))
			fw.Action(watch.Bookmark, initialEventsEndBookmark("25"))
		}
	})

	tasks := []snapshotStreamTask{
		{gvr: configMapGVR, namespace: "team-a"},
		{gvr: configMapGVR, namespace: "team-b"},
	}
	desired, rv, err := (&Manager{}).joinSnapshotStreams(context.Background(), dc, tasks)
	require.NoError(t, err)
	assert.Len(t, desired, 2)
	assert.Equal(t, "25", rv, "the joined revision is the max bookmark across streams")
}

// If any stream fails before its bookmark, the whole join aborts and returns nothing —
// a partial mark must never drive a sweep.
func TestJoinSnapshotStreams_OneFailureAbortsAll(t *testing.T) {
	dc := fakeStreamingClient(func(action clienttesting.Action, fw *watch.FakeWatcher) {
		if action.GetNamespace() == "bad" {
			fw.Stop() // never reaches its bookmark
			return
		}
		fw.Add(streamedCM("good", "ok", "10"))
		fw.Action(watch.Bookmark, initialEventsEndBookmark("15"))
	})

	tasks := []snapshotStreamTask{
		{gvr: configMapGVR, namespace: "good"},
		{gvr: configMapGVR, namespace: "bad"},
	}
	_, _, err := (&Manager{}).joinSnapshotStreams(context.Background(), dc, tasks)
	require.Error(t, err, "a partial stream aborts the whole snapshot")
}

func TestSnapshotStreamTasks_ExpandsScopes(t *testing.T) {
	gvrA := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tasks := snapshotStreamTasks([]snapshotGVR{
		{gvr: configMapGVR, namespaces: []string{"ns1", "ns2"}},
		{gvr: gvrA}, // cluster-wide
	})
	require.Len(t, tasks, 3)
	assert.Empty(t, tasks[2].namespace, "a cluster-wide type is one unscoped stream")
}

func TestMaxResourceVersion(t *testing.T) {
	assert.Equal(t, "12", maxResourceVersion("12", "9"), "numeric max")
	assert.Equal(t, "100", maxResourceVersion("9", "100"), "numeric max ignores string length")
	assert.Equal(t, "7", maxResourceVersion("7", ""), "empty is ignored")
	assert.Equal(t, "8", maxResourceVersion("", "8"), "empty is ignored")
}

func TestDesiredFromObject(t *testing.T) {
	dr, ok := desiredFromObject(configMapGVR, streamedCM("default", "app", "3"))
	require.True(t, ok)
	assert.Equal(t, "configmaps", dr.Resource.Resource)
	assert.Equal(t, "app", dr.Resource.Name)
	assert.Equal(t, "default", dr.Resource.Namespace)

	_, ok = desiredFromObject(configMapGVR, (*unstructured.Unstructured)(nil))
	assert.False(t, ok, "a nil object is not a desired entry")
}
