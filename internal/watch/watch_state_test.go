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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// fakeWatchStateWriter records every AppendWatchEvent and DeleteTypeWatchStream call so a test can
// assert what the runner observed without a Redis.
type fakeWatchStateWriter struct {
	mu      sync.Mutex
	appends []watchAppend
	deletes int
}

type watchAppend struct {
	group, resource, eventType, identity, rv, envelope string
}

func (w *fakeWatchStateWriter) AppendWatchEvent(
	_ context.Context, group, resource, eventType, identity, rv, envelope string,
) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appends = append(w.appends, watchAppend{group, resource, eventType, identity, rv, envelope})
	return nil
}

func (w *fakeWatchStateWriter) DeleteTypeWatchStream(_ context.Context, _, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deletes++
	return nil
}

func (w *fakeWatchStateWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.appends)
}

func (w *fakeWatchStateWriter) snapshot() []watchAppend {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]watchAppend(nil), w.appends...)
}

func (w *fakeWatchStateWriter) deleteCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deletes
}

// watcherFactory is the watchStateOpen test seam: it hands out pre-seeded watchers in order
// (recording the ResourceVersion each open requested, so resume-cursor behaviour is observable) and,
// once the seeded set is exhausted, returns a parking watcher so the runner blocks instead of
// hot-looping.
type watcherFactory struct {
	mu        sync.Mutex
	openedRVs []string
	queue     []watch.Interface
}

func (f *watcherFactory) open(
	_ context.Context, _ schema.GroupVersionResource, opts metav1.ListOptions,
) (watch.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openedRVs = append(f.openedRVs, opts.ResourceVersion)
	if len(f.queue) > 0 {
		w := f.queue[0]
		f.queue = f.queue[1:]
		return w, nil
	}
	return watch.NewFake(), nil // park until the runner's context cancels
}

func (f *watcherFactory) rvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.openedRVs...)
}

func (m *Manager) watchStateCount() int {
	m.watchStatesMu.Lock()
	defer m.watchStatesMu.Unlock()
	return len(m.watchStates)
}

func watchStateTestManager(writer StateWriter, factory *watcherFactory) *Manager {
	return &Manager{
		Log:                       logr.Discard(),
		WatchStateWriter:          writer,
		watchStateOpen:            factory.open,
		watchStateBackoffOverride: 5 * time.Millisecond,
	}
}

func cmObj(name, ns, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "resourceVersion": rv, "uid": "uid-" + name,
		},
		"data": map[string]any{"k": "v"},
	}}
}

func bookmarkObj(rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetResourceVersion(rv)
	return u
}

func closedWatcher() watch.Interface {
	w := watch.NewFake()
	w.Stop() // closed result channel -> the runner reads ok=false immediately
	return w
}

// TestWatchStateStream_RecordsEvents proves ADDED/MODIFIED/DELETED watch events each become one
// append with the right event type, identity, and resourceVersion, and a non-empty sanitized
// envelope that the checkpoint splice could fold.
func TestWatchStateStream_RecordsEvents(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	w := watch.NewFakeWithChanSize(16, false)
	factory := &watcherFactory{queue: []watch.Interface{w}}
	m := watchStateTestManager(writer, factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")
	w.Add(cmObj("cm1", "ns1", "101"))
	w.Modify(cmObj("cm1", "ns1", "102"))
	w.Delete(cmObj("cm1", "ns1", "103"))

	require.Eventually(t, func() bool { return writer.count() == 3 }, time.Second, 5*time.Millisecond,
		"all three watch events are recorded")

	got := writer.snapshot()
	require.Len(t, got, 3)
	assert.Equal(t, []string{"ADDED", "MODIFIED", "DELETED"},
		[]string{got[0].eventType, got[1].eventType, got[2].eventType})
	for i, want := range []string{"101", "102", "103"} {
		assert.Equal(t, "configmaps", got[i].resource)
		assert.Equal(t, "ns1/cm1", got[i].identity)
		assert.Equal(t, want, got[i].rv, "rv recorded per event")
	}

	// The envelope is the same shape ":objects:items" stores, so it must carry the identity and rv.
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(got[0].envelope), &env))
	assert.Equal(t, "cm1", env["name"])
	assert.Equal(t, "101", env["resource_version"])
	assert.NotEmpty(t, env["object"], "the sanitized object body is present")
}

// TestWatchStateStream_BookmarkAdvancesCursorNotRecorded proves a BOOKMARK is not recorded but
// advances the resume cursor, so the next session re-watches from the bookmark's rv (the standard
// watch-progress resume, no relist).
func TestWatchStateStream_BookmarkAdvancesCursorNotRecorded(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	w1 := watch.NewFakeWithChanSize(16, false)
	factory := &watcherFactory{queue: []watch.Interface{w1}}
	m := watchStateTestManager(writer, factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")
	w1.Action(watch.Bookmark, bookmarkObj("150"))
	w1.Stop() // close -> the runner re-opens from the advanced cursor

	require.Eventually(t, func() bool { return len(factory.rvs()) >= 2 }, time.Second, 5*time.Millisecond,
		"the runner re-opened after the channel closed")
	rvs := factory.rvs()
	assert.Equal(t, "100", rvs[0], "first session watches from the checkpoint rv")
	assert.Equal(t, "150", rvs[1], "second session resumes from the bookmark rv")
	assert.Equal(t, 0, writer.count(), "a bookmark is never recorded as state")
}

// TestWatchStateStream_ResetsToLiveEdgeAfterRepeatedFailures proves that after
// watchStateRelistThreshold consecutive abnormal sessions the runner drops its (un-resumable)
// cursor to "" — re-watching from the live edge rather than spinning on a 410-Gone rv. Correctness
// is owned by the checkpoint, so the gap is acceptable.
func TestWatchStateStream_ResetsToLiveEdgeAfterRepeatedFailures(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	factory := &watcherFactory{queue: []watch.Interface{closedWatcher(), closedWatcher(), closedWatcher()}}
	m := watchStateTestManager(writer, factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")

	require.Eventually(t, func() bool { return len(factory.rvs()) >= 4 }, 2*time.Second, 5*time.Millisecond,
		"the runner re-opened past the three failing sessions")
	rvs := factory.rvs()
	require.GreaterOrEqual(t, len(rvs), 4)
	assert.Equal(t, []string{"100", "100", "100"}, rvs[:3], "the first three sessions retry the same rv")
	assert.Empty(t, rvs[3], "after the threshold the cursor resets to the live edge")
}

// TestStartTypeWatchStream_StopsCleanly proves a running watcher is forgotten on stop.
func TestStartTypeWatchStream_StopsCleanly(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	factory := &watcherFactory{}
	m := watchStateTestManager(writer, factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")
	assert.Equal(t, 1, m.watchStateCount(), "one watcher is running")

	m.stopTypeWatchStream(configmapsGVR)
	assert.Equal(t, 0, m.watchStateCount(), "stop forgets the watcher")
}

// TestStartTypeWatchStream_Idempotent proves a repeated start (a periodic re-anchor's TypeSynced)
// never spawns a second watcher for the same type.
func TestStartTypeWatchStream_Idempotent(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	factory := &watcherFactory{}
	m := watchStateTestManager(writer, factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")
	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")
	m.startTypeWatchStream(ctx, logr.Discard(), configmapsGVR, "100")

	assert.Equal(t, 1, m.watchStateCount(), "repeat starts are no-ops")
	m.stopTypeWatchStream(configmapsGVR)
}

// TestStartTypeWatchStream_NilWriterIsNoop proves the parallel stream is cleanly disabled when no
// writer is wired (the flag off).
func TestStartTypeWatchStream_NilWriterIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.startTypeWatchStream(context.Background(), logr.Discard(), configmapsGVR, "100")
	assert.Equal(t, 0, m.watchStateCount(), "no watcher without a writer")
}

// TestStopTypeWatchStream_UnknownIsNoop proves stopping a type that never watched is harmless.
func TestStopTypeWatchStream_UnknownIsNoop(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.stopTypeWatchStream(configmapsGVR) // must not panic
	assert.Equal(t, 0, m.watchStateCount())
}

// TestDeleteTypeWatchStream proves release cleanup calls the writer, and is a no-op without one.
func TestDeleteTypeWatchStream(t *testing.T) {
	writer := &fakeWatchStateWriter{}
	m := &Manager{Log: logr.Discard(), WatchStateWriter: writer}
	m.deleteTypeWatchStream(context.Background(), logr.Discard(), configmapsGVR)
	assert.Equal(t, 1, writer.deleteCount(), "release drops the watch stream")

	nilM := &Manager{Log: logr.Discard()}
	nilM.deleteTypeWatchStream(context.Background(), logr.Discard(), configmapsGVR) // must not panic
}
