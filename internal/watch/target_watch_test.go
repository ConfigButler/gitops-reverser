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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestTargetWatchSpecs_UsesOneWatchPerScope(t *testing.T) {
	table := WatchedTypeTable{
		GitDest: types.NewResourceReference("target", "default"),
		Types: []WatchedType{
			{
				GVR: configmapsGVR,
				NamespaceOps: map[string]OperationSet{
					"apps": {"CREATE": struct{}{}},
					"ops":  {"UPDATE": struct{}{}},
				},
			},
			{
				GVR: schema.GroupVersionResource{
					Group:    "rbac.authorization.k8s.io",
					Version:  "v1",
					Resource: "clusterroles",
				},
				NamespaceOps: map[string]OperationSet{
					"": {"*": struct{}{}},
				},
			},
		},
	}

	specs := targetWatchSpecs(table)

	require.Len(t, specs, 3)
	assert.Equal(t, "[CREATE]", specs[targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}])
	assert.Equal(t, "[UPDATE]", specs[targetWatchKey{GVR: configmapsGVR, Namespace: "ops"}])
	assert.Equal(t, "[*]", specs[targetWatchKey{
		GVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	}])
}

func TestReplaceGitTargetWatches_ReusesUnchangedSetAndRestartsOnSpecChange(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opened := make(chan openedWatch, 4)
	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			namespace string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			fw := watch.NewFake()
			opened <- openedWatch{namespace: namespace, opts: opts, watch: fw}
			return fw, nil
		},
	}

	first := WatchedTypeTable{
		GitDest: gitDest,
		Types: []WatchedType{{
			GVR:          configmapsGVR,
			NamespaceOps: map[string]OperationSet{"apps": {"CREATE": struct{}{}}},
		}},
	}
	require.NoError(t, manager.replaceGitTargetWatches(ctx, first))
	initial := receiveOpenedWatch(t, opened)
	assert.Equal(t, "apps", initial.namespace)
	assert.True(t, *initial.opts.SendInitialEvents)
	assert.Equal(t, metav1.ResourceVersionMatchNotOlderThan, initial.opts.ResourceVersionMatch)
	assert.True(t, initial.opts.AllowWatchBookmarks)

	require.NoError(t, manager.replaceGitTargetWatches(ctx, first))
	assertNoOpenedWatch(t, opened)

	changed := WatchedTypeTable{
		GitDest: gitDest,
		Types: []WatchedType{{
			GVR:          configmapsGVR,
			NamespaceOps: map[string]OperationSet{"apps": {"UPDATE": struct{}{}}},
		}},
	}
	require.NoError(t, manager.replaceGitTargetWatches(ctx, changed))
	restarted := receiveOpenedWatch(t, opened)
	assert.Equal(t, "apps", restarted.namespace)
}

func TestRouteLiveTargetWatchEvent_ForwardsObjectEventsAsCommitter(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	manager := &Manager{EventRouter: router}

	obj := configMapObject("12")
	_, err := manager.routeLiveTargetWatchEvent(
		context.Background(),
		logr.Discard(),
		gitDest,
		targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
		OperationSet{"CREATE": struct{}{}},
		watch.Event{Type: watch.Added, Object: obj},
	)

	require.NoError(t, err)
	require.Len(t, enqueuer.events, 1)
	event := enqueuer.events[0]
	assert.Equal(t, "CREATE", event.Operation)
	assert.Equal(t, "target", event.GitTargetName)
	assert.Equal(t, "default", event.GitTargetNamespace)
	assert.Empty(t, event.UserInfo.Username, "unattributed watch events commit as the configured committer")
	assert.NotNil(t, event.Object)
	assert.Empty(t, event.Object.GetResourceVersion(), "live events are sanitized before entering Git")
}

func TestRouteLiveTargetWatchEvent_RespectsOperationFilters(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	manager := &Manager{EventRouter: router}

	_, err := manager.routeLiveTargetWatchEvent(
		context.Background(),
		logr.Discard(),
		gitDest,
		targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
		OperationSet{"DELETE": struct{}{}},
		watch.Event{Type: watch.Modified, Object: configMapObject("13")},
	)

	require.NoError(t, err)
	assert.Empty(t, enqueuer.events)
}

func TestRouteLiveTargetWatchEvent_AttributesAuthorFromResolver(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	manager := &Manager{
		EventRouter: router,
		AuthorResolver: NewAuthorResolver(
			&fakeLookup{fact: queue.AuthorFact{Author: "alice", Email: "alice@example.com"}, ok: true, hitAfter: 1},
			time.Second, SANamePolicyName, logr.Discard(),
		),
	}

	_, err := manager.routeLiveTargetWatchEvent(
		context.Background(),
		logr.Discard(),
		gitDest,
		targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
		OperationSet{"CREATE": struct{}{}},
		watch.Event{Type: watch.Added, Object: configMapObject("12")},
	)

	require.NoError(t, err)
	require.Len(t, enqueuer.events, 1)
	assert.Equal(t, "alice", enqueuer.events[0].UserInfo.Username, "a matched audit fact names the commit author")
	assert.Equal(t, "alice@example.com", enqueuer.events[0].UserInfo.Email)
}

func TestHandleTargetWatchSessionEvent_CompletesReplayWithoutRouter(t *testing.T) {
	manager := &Manager{}
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	var replay []manifestanalyzer.DesiredResource

	replaying, err := manager.handleTargetWatchSessionEvent(
		context.Background(),
		logr.Discard(),
		gitDest,
		key,
		nil,
		watch.Event{Type: watch.Added, Object: configMapObject("10")},
		true,
		&replay,
	)
	require.NoError(t, err)
	assert.True(t, replaying)
	require.Len(t, replay, 1)

	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("11")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	replaying, err = manager.handleTargetWatchSessionEvent(
		context.Background(),
		logr.Discard(),
		gitDest,
		key,
		nil,
		watch.Event{Type: watch.Bookmark, Object: bookmark},
		true,
		&replay,
	)
	require.NoError(t, err)
	assert.False(t, replaying)
	assert.Nil(t, replay)
}

func TestTargetWatchReplayAndStream_ReturnsWhenContextCancels(t *testing.T) {
	fw := watch.NewFake()
	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			_ metav1.ListOptions,
		) (watch.Interface, error) {
			return fw, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(
			ctx,
			logr.Discard(),
			types.NewResourceReference("target", "default"),
			targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
			nil,
		)
	}()

	fw.Add(configMapObject("10"))
	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("11")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	fw.Action(watch.Bookmark, bookmark)
	cancel()

	require.NoError(t, <-done)
}

func TestTargetWatchReplayAndStream_FallsBackWhenReplayWatchIsForbidden(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	fw := watch.NewFake()
	openCount := 0
	listed := make(chan struct{})
	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			openCount++
			if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
				return nil, errors.New("sendInitialEvents: Forbidden: sendInitialEvents is forbidden")
			}
			return fw, nil
		},
		targetWatchList: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			_ metav1.ListOptions,
		) (*unstructured.UnstructuredList, error) {
			list := &unstructured.UnstructuredList{}
			list.SetResourceVersion("9")
			close(listed)
			return list, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(
			ctx,
			logr.Discard(),
			gitDest,
			targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
			nil,
		)
	}()

	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("expected fallback list to run")
	}
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, 2, openCount)
}

func TestTargetWatchReplayAndStream_ResumesFromStoredCursor(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	fw := watch.NewFake()
	store := &fakeWatchCursorStore{rv: "41", ok: true}
	manager := &Manager{
		Log:              logr.Discard(),
		EventRouter:      router,
		WatchCursorStore: store,
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			assert.Nil(t, opts.SendInitialEvents)
			assert.Equal(t, "41", opts.ResourceVersion)
			return fw, nil
		},
		targetWatchList: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			_ metav1.ListOptions,
		) (*unstructured.UnstructuredList, error) {
			t.Fatal("cursor resume should not list")
			return nil, errors.New("cursor resume should not list")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(
			ctx,
			logr.Discard(),
			gitDest,
			targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
			nil,
		)
	}()

	fw.Modify(configMapObject("42"))
	require.Eventually(t, func() bool {
		return len(enqueuer.events) == 1
	}, time.Second, 10*time.Millisecond)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, "42", store.recordedRV)
	assert.Equal(t, "UPDATE", enqueuer.events[0].Operation)
}

func TestOpenTargetWatch_UsesConfiguredHook(t *testing.T) {
	fw := watch.NewFake()
	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context,
			gvr schema.GroupVersionResource,
			namespace string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			assert.Equal(t, configmapsGVR, gvr)
			assert.Equal(t, "apps", namespace)
			assert.Equal(t, "42", opts.ResourceVersion)
			return fw, nil
		},
	}

	w, err := manager.openTargetWatch(
		context.Background(),
		configmapsGVR,
		"apps",
		metav1.ListOptions{ResourceVersion: "42"},
	)
	require.NoError(t, err)
	assert.Same(t, fw, w)
}

func TestTargetWatchOperationHelpers(t *testing.T) {
	specs := map[targetWatchKey]string{
		{GVR: configmapsGVR, Namespace: "b"}: "[CREATE]",
		{GVR: configmapsGVR, Namespace: "a"}: "[UPDATE]",
	}
	keys := sortedTargetWatchSpecKeys(specs)
	require.Len(t, keys, 2)
	assert.Equal(t, "a", keys[0].Namespace)
	assert.True(t, equalTargetWatchSpecs(specs, map[targetWatchKey]string{
		{GVR: configmapsGVR, Namespace: "b"}: "[CREATE]",
		{GVR: configmapsGVR, Namespace: "a"}: "[UPDATE]",
	}))
	assert.False(t, equalTargetWatchSpecs(specs, map[targetWatchKey]string{
		{GVR: configmapsGVR, Namespace: "a"}: "[UPDATE]",
	}))

	table := WatchedTypeTable{Types: []WatchedType{{
		GVR:          configmapsGVR,
		NamespaceOps: map[string]OperationSet{"": {"CREATE": struct{}{}}},
	}}}
	assert.True(t, table.operationsFor(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}).Match("CREATE"))
	assert.False(t, table.operationsFor(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}).Match("UPDATE"))
	assert.True(t, OperationSet(nil).Match("DELETE"))
	assert.True(t, OperationSet{"*": struct{}{}}.Match("UPDATE"))
	assert.Equal(t, "DELETE", operationForWatchEvent(watch.Deleted))
	assert.Empty(t, operationForWatchEvent(watch.Error))
}

func TestFoldTargetReplayEvent_AccumulatesUntilInitialEventsBookmark(t *testing.T) {
	manager := &Manager{}
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	var desired []manifestanalyzer.DesiredResource

	done, rv, err := manager.foldTargetReplayEvent(
		logr.Discard(),
		gitDest,
		key,
		watch.Event{Type: watch.Added, Object: configMapObject("10")},
		&desired,
	)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, rv)
	require.Len(t, desired, 1)

	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("11")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	done, rv, err = manager.foldTargetReplayEvent(
		logr.Discard(),
		gitDest,
		key,
		watch.Event{Type: watch.Bookmark, Object: bookmark},
		&desired,
	)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, "11", rv)
}

func TestForgetGitTargetWatches_CancelsAndRemovesSet(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	childCtx, childCancel := context.WithCancel(ctx)
	store := &fakeWatchCursorStore{}
	watchKey := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	manager := &Manager{
		WatchCursorStore: store,
		targetWatches: map[string]*targetWatchSet{
			gitDest.Key(): {
				cancel: func() {
					childCancel()
					close(cancelled)
				},
				specs: map[targetWatchKey]string{watchKey: "[*]"},
			},
		},
	}

	manager.forgetGitTargetWatches(gitDest)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expected target watch cancellation")
	}
	require.ErrorIs(t, childCtx.Err(), context.Canceled)
	assert.Empty(t, manager.targetWatches)
	assert.True(t, store.deleted)
}

func receiveOpenedWatch(t *testing.T, opened <-chan openedWatch) openedWatch {
	t.Helper()
	select {
	case got := <-opened:
		return got
	case <-time.After(time.Second):
		t.Fatal("expected target watch to open")
		return openedWatch{}
	}
}

func assertNoOpenedWatch(t *testing.T, opened <-chan openedWatch) {
	t.Helper()
	select {
	case got := <-opened:
		got.watch.Stop()
		t.Fatalf("unexpected target watch opened for namespace %q", got.namespace)
	case <-time.After(50 * time.Millisecond):
	}
}

type openedWatch struct {
	namespace string
	opts      metav1.ListOptions
	watch     *watch.FakeWatcher
}

type recordingEnqueuer struct {
	events []git.Event
}

func (r *recordingEnqueuer) Enqueue(event git.Event) {
	r.events = append(r.events, event)
}

func configMapObject(rv string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"namespace":       "apps",
			"name":            "demo",
			"resourceVersion": rv,
		},
		"data": map[string]interface{}{"key": "value"},
	}}
	obj.SetNamespace("apps")
	obj.SetName("demo")
	obj.SetResourceVersion(rv)
	return obj
}

type fakeWatchCursorStore struct {
	rv         string
	ok         bool
	recordedRV string
	deleted    bool
}

func (f *fakeWatchCursorStore) LookupWatchCursor(
	_ context.Context,
	_, _ string,
	_ schema.GroupVersionResource,
	_ string,
) (string, bool) {
	return f.rv, f.ok
}

func (f *fakeWatchCursorStore) RecordWatchCursor(
	_ context.Context,
	_, _ string,
	_ schema.GroupVersionResource,
	_, rv string,
) error {
	f.recordedRV = rv
	return nil
}

func (f *fakeWatchCursorStore) DeleteWatchCursor(
	_ context.Context,
	_, _ string,
	_ schema.GroupVersionResource,
	_ string,
) error {
	f.deleted = true
	return nil
}
