// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
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

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// admittingEnqueuer is a branch worker's admission as a live event sees it: it takes events
// until told to refuse them, the way a full queue or a closed admission gate does.
type admittingEnqueuer struct {
	mu      sync.Mutex
	refuse  bool
	events  []git.Event
	refused int
}

func (a *admittingEnqueuer) Enqueue(event git.Event) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse {
		a.refused++
		return false
	}
	a.events = append(a.events, event)
	return true
}

func (a *admittingEnqueuer) setRefuse(refuse bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuse = refuse
}

func (a *admittingEnqueuer) accepted() []git.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]git.Event(nil), a.events...)
}

// dedupConfigMap is the demo ConfigMap at one resourceVersion with one data value, carrying a
// UID so the dedup cache keys it the way a live object is keyed.
func dedupConfigMap(rv, value string) *unstructured.Unstructured {
	obj := configMapObject(rv)
	obj.SetUID("cm-uid")
	obj.Object["data"] = map[string]interface{}{"key": value}
	return obj
}

func admissionTestManager(gitDest types.ResourceReference) (*Manager, *admittingEnqueuer, *fakeWatchCursorStore) {
	enqueuer := &admittingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	store := &fakeWatchCursorStore{}
	manager := &Manager{
		Log: logr.Discard(),
		EventRouter: &EventRouter{
			Log:              logr.Discard(),
			gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
		},
		WatchCursorStore: store,
	}
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))
	return manager, enqueuer, store
}

// The producer half of admission backpressure: a refused UPDATE is redelivered by a cursor resume,
// and only that redelivery's ACCEPTANCE may move the cursor past it. Before the fix the refused
// attempt had already stored its content hash, so the redelivered frame matched it, was skipped as
// a no-op, and advanced the cursor past a change Git never received.
func TestProcessLiveTargetWatchEvent_ARefusedUpdateIsOfferedAgainOnCursorResume(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	manager, enqueuer, store := admissionTestManager(gitDest)
	stream := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})
	process := func(ev watch.Event) error {
		return manager.processLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest, stream, ev)
	}

	require.NoError(t, process(watch.Event{Type: watch.Added, Object: dedupConfigMap("10", "A")}))
	require.Equal(t, "10", store.lastRecordedRV())

	enqueuer.setRefuse(true)
	require.Error(t, process(watch.Event{Type: watch.Modified, Object: dedupConfigMap("11", "B")}),
		"a refused event ends the session")
	assert.Equal(t, "10", store.lastRecordedRV(), "the cursor stays before the refused change")

	// The reconnect resumes from 10 and the API server delivers the same frame again.
	enqueuer.setRefuse(false)
	require.NoError(t, process(watch.Event{Type: watch.Modified, Object: dedupConfigMap("11", "B")}))
	accepted := enqueuer.accepted()
	require.Len(t, accepted, 2, "the redelivered UPDATE must reach the worker, not be deduped away")
	assert.Equal(t, "UPDATE", accepted[1].Operation)
	assert.Equal(t, "11", store.lastRecordedRV())

	// Accepted content still suppresses a /status-only UPDATE that sanitizes to it.
	require.NoError(t, process(watch.Event{Type: watch.Modified, Object: dedupConfigMap("12", "B")}))
	assert.Len(t, enqueuer.accepted(), 2, "a no-op UPDATE against accepted content is still skipped")
	assert.Equal(t, "12", store.lastRecordedRV(), "and a skipped no-op still advances the cursor")
}

// An event a cancelled stream never enqueued is neither a baseline nor a cursor position.
func TestProcessLiveTargetWatchEvent_ACancelledStreamNeitherDedupsNorAdvances(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	manager, enqueuer, store := admissionTestManager(gitDest)
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	old := testStream(key)

	require.NoError(t, manager.processLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest, old,
		watch.Event{Type: watch.Added, Object: dedupConfigMap("10", "A")}))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, manager.processLiveTargetWatchEvent(cancelled, logr.Discard(), gitDest, old,
		watch.Event{Type: watch.Modified, Object: dedupConfigMap("11", "B")}))
	assert.Len(t, enqueuer.accepted(), 1, "a cancelled stream enqueues nothing")
	assert.Equal(t, "10", store.lastRecordedRV(), "and records no cursor past what it did not enqueue")

	replacement := testStream(key)
	require.NoError(t, manager.processLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest,
		replacement, watch.Event{Type: watch.Modified, Object: dedupConfigMap("11", "B")}))
	assert.Len(t, enqueuer.accepted(), 2, "the next stream's B is a change: the cancelled one never delivered it")
}

// A stream resumes from a stored cursor only after one of its own sessions completed a replay.
// The first session here ends mid-replay, with a previous stream's cursor (41) still stored: the
// reconnect must replay again, because resuming from 41 would skip this stream's snapshot and its
// sweep. Once that replay completes (cursor 50), the next reconnect resumes from it.
func TestRunTargetWatch_ResumesOnlyAfterItsOwnReplayCompleted(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	store := &fakeWatchCursorStore{rv: "41", ok: true}
	opened := make(chan openedWatch, 4)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.runTargetWatch(ctx, logr.Discard(), gitDest, testStream(key))
	}()

	first := receiveOpenedWatch(t, opened)
	require.NotNil(t, first.opts.SendInitialEvents, "a new stream replays")
	first.watch.Add(configMapObject("45"))
	first.watch.Stop()

	second := receiveReconnect(t, opened)
	require.NotNil(t, second.opts.SendInitialEvents,
		"a stream whose replay never completed must replay again, not resume a previous stream's cursor")
	assert.Empty(t, second.opts.ResourceVersion)

	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("50")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	second.watch.Action(watch.Bookmark, bookmark)
	require.Eventually(t, func() bool { return store.lastRecordedRV() == "50" }, time.Second, 10*time.Millisecond)
	store.mu.Lock()
	store.rv = "50"
	store.mu.Unlock()
	second.watch.Stop()

	third := receiveReconnect(t, opened)
	assert.Nil(t, third.opts.SendInitialEvents, "after its own replay the stream resumes")
	assert.Equal(t, "50", third.opts.ResourceVersion)

	cancel()
	<-done
}

// receiveReconnect waits out the reconnect backoff for the next open.
func receiveReconnect(t *testing.T, opened <-chan openedWatch) openedWatch {
	t.Helper()
	select {
	case got := <-opened:
		return got
	case <-time.After(2 * targetWatchBackoff):
		t.Fatal("expected the target watch to reconnect")
		return openedWatch{}
	}
}

// pausingResyncs is a branch worker that has paused intake: it refuses every snapshot until it
// reopens, and says so through IntakePaused, the way the real worker does.
type pausingResyncs struct {
	mu     sync.Mutex
	paused chan struct{}
}

func (p *pausingResyncs) EnqueueResync(request *git.ResyncRequest) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused != nil {
		request.Result <- git.ResyncResult{Err: git.ErrFinalizeQueueFull}
		return false
	}
	request.Result <- git.ResyncResult{}
	return true
}

func (p *pausingResyncs) IntakePaused() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *pausingResyncs) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = make(chan struct{})
}

// wake wakes the waiting producers; pausedAgain pauses again before they get to look.
func (p *pausingResyncs) wake(pausedAgain bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.paused)
	p.paused = nil
	if pausedAgain {
		p.paused = make(chan struct{})
	}
}

// A stream whose branch has paused intake waits for the branch to reopen instead of reconnecting on
// its backoff: reconnecting would gather a snapshot the branch refuses again, every two seconds,
// for as long as the outage lasts. A wake-up finds the branch paused again and keeps waiting; the
// reopening reconnects at once.
func TestRunTargetWatch_APausedBranchWaitsForIntakeToReopen(t *testing.T) {
	setTargetWatchBackoff(t, 10*time.Millisecond)
	gitDest := types.NewResourceReference("target", "default")
	worker := &pausingResyncs{}
	worker.pause()
	opened := make(chan openedWatch, 8)
	manager := &Manager{
		Log:              logr.Discard(),
		WatchCursorStore: &fakeWatchCursorStore{},
		EventRouter: &EventRouter{
			Log: logr.Discard(),
			resyncWorker: func(context.Context, types.ResourceReference) (resyncEnqueuer, error) {
				return worker, nil
			},
		},
		targetWatchOpen: func(
			_ context.Context, _ schema.GroupVersionResource, namespace string, opts metav1.ListOptions,
		) (watch.Interface, error) {
			fw := watch.NewFakeWithChanSize(8, false)
			opened <- openedWatch{namespace: namespace, opts: opts, watch: fw}
			return fw, nil
		},
	}
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
		manager.runTargetWatch(ctx, logr.Discard(), gitDest, testStream(key))
	}()

	first := receiveOpenedWatch(t, opened)
	completeInitialEvents(first.watch, "50") // the paused branch refuses the replay's snapshot
	assertNoReconnectWithin(t, opened, 300*time.Millisecond, "a paused branch is not offered the snapshot again")

	worker.wake(true)
	assertNoReconnectWithin(t, opened, 300*time.Millisecond, "a wake-up that finds the branch paused again waits on")

	worker.wake(false)
	second := receiveReconnect(t, opened)
	assert.NotNil(t, second.opts.SendInitialEvents, "the reopened branch gets the replay it refused")

	cancel()
	<-done
}

// completeInitialEvents ends an initial-events replay with its bookmark at rv.
func completeInitialEvents(fw *watch.FakeWatcher, rv string) {
	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion(rv)
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	fw.Action(watch.Bookmark, bookmark)
}

func assertNoReconnectWithin(t *testing.T, opened <-chan openedWatch, d time.Duration, msg string) {
	t.Helper()
	select {
	case got := <-opened:
		got.watch.Stop()
		t.Fatalf("reconnected: %s", msg)
	case <-time.After(d):
	}
}

// setTargetWatchBackoff shortens the reconnect backoff for one test.
func setTargetWatchBackoff(t *testing.T, d time.Duration) {
	t.Helper()
	original := targetWatchBackoff
	targetWatchBackoff = d
	t.Cleanup(func() { targetWatchBackoff = original })
}
