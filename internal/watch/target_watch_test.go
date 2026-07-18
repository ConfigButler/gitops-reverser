// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	store := &fakeWatchCursorStore{rv: "41", ok: true}
	manager := &Manager{
		Log:              logr.Discard(),
		WatchCursorStore: store,
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
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))

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
	assert.True(t, *restarted.opts.SendInitialEvents,
		"a target-watch replacement must replay the existing scope for its new fidelity epoch")
	assert.Empty(t, restarted.opts.ResourceVersion,
		"the first session of a replacement must not resume an old epoch from a durable cursor")
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
			&fakeLookup{
				resolution: queue.AuthorResolution{
					Fact:   queue.AuthorFact{Author: "alice", Email: "alice@example.com"},
					Result: queue.AttributionExactUser,
				},
				hitAfter: 1,
			},
			time.Second, logr.Discard(),
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

// TestRouteLiveTargetWatchEvent_DeletionTimestampRendersAsDelete proves the
// deletion-as-intent rule: a MODIFIED carrying a deletionTimestamp renders as a
// bodyless DELETE (the object is logically absent), not an UPDATE that would commit
// the terminating state.
func TestRouteLiveTargetWatchEvent_DeletionTimestampRendersAsDelete(t *testing.T) {
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
		watch.Event{Type: watch.Modified, Object: terminatingConfigMapObject("20")},
	)

	require.NoError(t, err)
	require.Len(t, enqueuer.events, 1)
	assert.Equal(t, "DELETE", enqueuer.events[0].Operation,
		"a terminating object renders as a removal, not an update")
	assert.Nil(t, enqueuer.events[0].Object, "a DELETE event carries no body")
	assert.Equal(t, "demo", enqueuer.events[0].Identifier.Name)
}

// TestRouteLiveTargetWatchEvent_ModifiedWithoutDeletionTimestampRendersAsUpdate is
// the guard: a normal MODIFIED still renders as a sanitized UPDATE.
func TestRouteLiveTargetWatchEvent_ModifiedWithoutDeletionTimestampRendersAsUpdate(t *testing.T) {
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
		OperationSet{"UPDATE": struct{}{}},
		watch.Event{Type: watch.Modified, Object: configMapObject("21")},
	)

	require.NoError(t, err)
	require.Len(t, enqueuer.events, 1)
	assert.Equal(t, "UPDATE", enqueuer.events[0].Operation)
	assert.NotNil(t, enqueuer.events[0].Object, "an UPDATE carries the sanitized object")
}

// TestRouteLiveTargetWatchEvent_TerminatingThenDeletedAreIdenticalRemovals proves the
// follow-on events fold: the deletionTimestamp MODIFIED and the eventual DELETED emit
// the same bodyless removal for the same path, so the writer collapses the second to
// a no-op against the already-absent file.
func TestRouteLiveTargetWatchEvent_TerminatingThenDeletedAreIdenticalRemovals(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	manager := &Manager{EventRouter: router}
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	ops := OperationSet{"DELETE": struct{}{}}

	_, err := manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest, key, ops,
		watch.Event{Type: watch.Modified, Object: terminatingConfigMapObject("20")})
	require.NoError(t, err)
	_, err = manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest, key, ops,
		watch.Event{Type: watch.Deleted, Object: configMapObject("22")})
	require.NoError(t, err)

	require.Len(t, enqueuer.events, 2)
	assert.Equal(t, "DELETE", enqueuer.events[0].Operation)
	assert.Equal(t, "DELETE", enqueuer.events[1].Operation)
	assert.Equal(t, enqueuer.events[0].Identifier, enqueuer.events[1].Identifier,
		"both removals target the same path, so the writer folds the second to a no-op")
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
			false,
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
			true,
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
	// The data-plane gitDest carries no UID (it comes from the rule-derived watch
	// table); the controller's Declare is what teaches the manager the UID.
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(
			ctx,
			logr.Discard(),
			gitDest,
			targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
			nil,
			true,
		)
	}()

	fw.Modify(configMapObject("42"))
	require.Eventually(t, func() bool {
		return len(enqueuer.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, "42", store.lastRecordedRV())
	assert.Equal(t, "uid-1", store.lastRecordedUID(), "cursor is keyed by the remembered GitTarget UID")
	assert.Equal(t, "UPDATE", enqueuer.snapshot()[0].Operation)
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
		configPlaneClusterID,
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

	// Deletion-as-intent: a deletionTimestamp forces DELETE regardless of event type;
	// otherwise the helper mirrors operationForWatchEvent.
	assert.Equal(t, "UPDATE", operationForLiveTargetWatchEvent(watch.Modified, configMapObject("1")))
	assert.Equal(t, "DELETE", operationForLiveTargetWatchEvent(watch.Modified, terminatingConfigMapObject("1")))
	assert.Equal(t, "CREATE", operationForLiveTargetWatchEvent(watch.Added, configMapObject("1")))
	assert.Equal(t, "DELETE", operationForLiveTargetWatchEvent(watch.Added, terminatingConfigMapObject("1")))
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
	watchKey := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	manager := &Manager{
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

	// Forget only cancels and drops in-memory state; the durable cursors are left to
	// expire by TTL, so a recreated GitTarget (new UID) never inherits them.
	manager.forgetGitTargetWatches(gitDest)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expected target watch cancellation")
	}
	require.ErrorIs(t, childCtx.Err(), context.Canceled)
	assert.Empty(t, manager.targetWatches)
}

func TestTargetWatchReplayAndStream_ExpiredCursorFallsBackToFreshReplay(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default").WithUID("uid-1")
	store := &fakeWatchCursorStore{rv: "41", ok: true}
	fresh := watch.NewFake()
	var resumeAttempts, replayOpens int
	manager := &Manager{
		Log:              logr.Discard(),
		WatchCursorStore: store,
		targetWatchOpen: func(
			_ context.Context,
			_ schema.GroupVersionResource,
			_ string,
			opts metav1.ListOptions,
		) (watch.Interface, error) {
			if opts.ResourceVersion == "41" && opts.SendInitialEvents == nil {
				resumeAttempts++
				return nil, apierrors.NewResourceExpired("resourceVersion too old")
			}
			require.NotNil(t, opts.SendInitialEvents)
			assert.True(t, *opts.SendInitialEvents)
			replayOpens++
			return fresh, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(
			ctx, logr.Discard(), gitDest,
			targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}, nil, true,
		)
	}()

	// The stale cursor fails to resume (410 Gone), so the watch rebuilds from a fresh
	// sendInitialEvents replay, which overwrites the cursor — no delete required.
	fresh.Add(configMapObject("42"))
	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("43")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	fresh.Action(watch.Bookmark, bookmark)
	require.Eventually(t, func() bool {
		return store.lastRecordedRV() == "43"
	}, time.Second, 10*time.Millisecond)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, 1, resumeAttempts)
	assert.Equal(t, 1, replayOpens)
	assert.Equal(t, "uid-1", store.lastLookedUpUID(), "the GitTarget UID scopes the cursor lookup")
	assert.Equal(t, "uid-1", store.lastRecordedUID(), "the GitTarget UID scopes the cursor write")
}

func TestManager_GitTargetUIDLifecycle(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("target", "default")

	// A UID-less data-plane gitDest resolves to nothing until the controller's Declare
	// teaches the manager the UID.
	assert.Empty(t, m.resolveGitTargetUID(gitDest))

	m.rememberGitTargetUID(gitDest.WithUID("uid-1"))
	assert.Equal(t, "uid-1", m.resolveGitTargetUID(gitDest), "the data-plane gitDest resolves via the remembered map")
	assert.Equal(t, "uid-9", m.resolveGitTargetUID(gitDest.WithUID("uid-9")), "an explicit UID on gitDest wins")

	// A UID-less forget (the delete path's NotFound reaction) is a no-op: it must not wipe the
	// stored UID, or a delete racing behind a same-name recreate would drop the fresh cursor.
	m.forgetGitTargetUID(gitDest)
	assert.Equal(t, "uid-1", m.resolveGitTargetUID(gitDest), "UID-less forget leaves the stored UID intact")

	// A forget carrying a stale UID is likewise ignored; only a matching UID clears the entry.
	m.forgetGitTargetUID(gitDest.WithUID("uid-9"))
	assert.Equal(t, "uid-1", m.resolveGitTargetUID(gitDest), "a non-matching UID does not evict the entry")

	m.forgetGitTargetUID(gitDest.WithUID("uid-1"))
	assert.Empty(t, m.resolveGitTargetUID(gitDest), "the matching UID clears the entry")
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
	mu     sync.Mutex
	events []git.Event
}

func (r *recordingEnqueuer) Enqueue(event git.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return true
}

// snapshot returns a copy of the recorded events under lock, safe to read while a
// watch goroutine is still enqueuing.
func (r *recordingEnqueuer) snapshot() []git.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]git.Event(nil), r.events...)
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

// terminatingConfigMapObject is a ConfigMap that has been marked for deletion: it
// carries a deletionTimestamp (and a finalizer keeping it alive) but still exists in
// the cluster. It models the object the watch delivers as a MODIFIED while Kubernetes
// finalization is in flight.
func terminatingConfigMapObject(rv string) *unstructured.Unstructured {
	obj := configMapObject(rv)
	obj.SetDeletionTimestamp(&metav1.Time{Time: time.Unix(0, 0).UTC()})
	obj.SetFinalizers([]string{"example.com/cleanup"})
	return obj
}

type fakeWatchCursorStore struct {
	mu          sync.Mutex
	rv          string
	ok          bool
	recordedRV  string
	recordedUID string
	lookedUpUID string
}

func (f *fakeWatchCursorStore) LookupWatchCursor(
	_ context.Context,
	gitTargetUID string,
	_ schema.GroupVersionResource,
	_ string,
) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookedUpUID = gitTargetUID
	return f.rv, f.ok
}

func (f *fakeWatchCursorStore) RecordWatchCursor(
	_ context.Context,
	gitTargetUID string,
	_ schema.GroupVersionResource,
	_, rv string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedUID = gitTargetUID
	f.recordedRV = rv
	return nil
}

// lastRecordedRV, lastRecordedUID, and lastLookedUpUID read the recorded values under
// lock, safe to call while a watch goroutine is still recording cursors.
func (f *fakeWatchCursorStore) lastRecordedRV() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordedRV
}

func (f *fakeWatchCursorStore) lastRecordedUID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordedUID
}

func (f *fakeWatchCursorStore) lastLookedUpUID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookedUpUID
}
