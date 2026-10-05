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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// filterObject is a ConfigMap "<uid>" in apps whose desired state is its data value.
func filterObject(uid, rv, value string) *unstructured.Unstructured {
	obj := configMapObject(rv)
	obj.SetName(uid)
	obj.SetUID(k8stypes.UID(uid))
	obj.Object["data"] = map[string]interface{}{"key": value}
	return obj
}

// verdict runs the filter's check on one object, as the live path does.
func verdict(f *desiredStateChangeFilter, u *unstructured.Unstructured, op string) (desiredStateChange, bool) {
	opType := watch.Modified
	switch op {
	case string(types.OperationCreate):
		opType = watch.Added
	case string(types.OperationDelete):
		opType = watch.Deleted
	}
	event := targetWatchGitEvent(configmapsGVR, u, operationForLiveTargetWatchEvent(opType, u))
	return f.check(u, &event, op)
}

// offer checks one event and, when it routes, accepts it as a worker that took it would.
func offer(f *desiredStateChangeFilter, u *unstructured.Unstructured, op string) bool {
	change, unchanged := verdict(f, u, op)
	if !unchanged {
		f.accept(change)
	}
	return !unchanged
}

func TestDesiredStateChangeFilter_Contract(t *testing.T) {
	create, update, del := string(types.OperationCreate), string(types.OperationUpdate), string(types.OperationDelete)

	t.Run("an UPDATE with no baseline passes", func(t *testing.T) {
		assert.True(t, offer(newDesiredStateChangeFilter(), filterObject("a", "1", "X"), update))
	})
	t.Run("a nil filter passes everything", func(t *testing.T) {
		var f *desiredStateChangeFilter
		assert.True(t, offer(f, filterObject("a", "1", "X"), create))
		assert.True(t, offer(f, filterObject("a", "2", "X"), update))
	})
	t.Run("an object without a UID is never filtered", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		assert.True(t, offer(f, filterObject("", "1", "X"), create))
		assert.True(t, offer(f, filterObject("", "2", "X"), update))
	})
	t.Run("X, then Y, then X: both changes route", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		assert.True(t, offer(f, filterObject("a", "2", "Y"), update))
		assert.True(t, offer(f, filterObject("a", "3", "X"), update), "a return to earlier content is a change")
		assert.False(t, offer(f, filterObject("a", "4", "X"), update))
	})
	t.Run("a CREATE always routes, even with a matching baseline", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		assert.True(t, offer(f, filterObject("a", "2", "X"), create))
	})
	t.Run("a refused event is not a baseline", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		_, unchanged := verdict(f, filterObject("a", "2", "Y"), update)
		require.False(t, unchanged)
		// The worker refused Y: nothing is accepted.
		assert.True(t, offer(f, filterObject("a", "2", "Y"), update), "Y redelivered after its refusal still routes")
	})
	t.Run("an accepted DELETE leaves no baseline for its UID", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		require.True(t, offer(f, filterObject("a", "2", "X"), del))
		assert.True(t, offer(f, filterObject("a", "3", "X"), update))
	})
	t.Run("a Terminating UPDATE is a removal and clears the baseline", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		terminating := filterObject("a", "2", "X")
		terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Unix(0, 0)})
		change, unchanged := verdict(f, terminating, del)
		require.False(t, unchanged)
		f.accept(change)
		assert.Empty(t, f.baselines)
	})
	t.Run("an accepted snapshot replaces every baseline", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		require.True(t, offer(f, filterObject("gone", "1", "X"), create))
		var snapshot replaySnapshot
		snapshot.add(configmapsGVR, filterObject("a", "5", "Z"))
		f.adopt(snapshot.baselines)
		assert.True(t, offer(f, filterObject("a", "6", "X"), update), "the replay's Z replaced the stale X")
		assert.True(t, offer(f, filterObject("gone", "6", "X"), update), "a UID the snapshot omits has no baseline")
	})
	t.Run("an accepted empty snapshot clears every baseline", func(t *testing.T) {
		f := newDesiredStateChangeFilter()
		require.True(t, offer(f, filterObject("a", "1", "X"), create))
		f.adopt(snapshotFromList(configmapsGVR, &unstructured.UnstructuredList{}).baselines)
		assert.True(t, offer(f, filterObject("a", "2", "X"), update))
		assert.False(t, offer(f, filterObject("a", "3", "X"), update), "and records again afterwards")
	})
}

// Only Git-visible content is compared: data, spec, and the labels and annotations sanitize keeps.
// Status and the metadata sanitize strips change nothing Git holds.
func TestDesiredStateChangeFilter_ComparesWhatGitHolds(t *testing.T) {
	update := string(types.OperationUpdate)
	base := func() *unstructured.Unstructured {
		obj := filterObject("a", "1", "X")
		obj.SetLabels(map[string]string{"team": "a"})
		obj.SetAnnotations(map[string]string{"note": "kept"})
		return obj
	}
	changes := map[string]func(*unstructured.Unstructured){
		"data":             func(o *unstructured.Unstructured) { o.Object["data"] = map[string]interface{}{"key": "Y"} },
		"spec":             func(o *unstructured.Unstructured) { o.Object["spec"] = map[string]interface{}{"replicas": int64(2)} },
		"a retained label": func(o *unstructured.Unstructured) { o.SetLabels(map[string]string{"team": "b"}) },
		"a retained annotation": func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{"note": "edited"})
		},
	}
	for name, mutate := range changes {
		t.Run(name+" routes", func(t *testing.T) {
			f := newDesiredStateChangeFilter()
			require.True(t, offer(f, base(), string(types.OperationCreate)))
			edited := base()
			mutate(edited)
			assert.True(t, offer(f, edited, update))
		})
	}
	noOps := map[string]func(*unstructured.Unstructured){
		"status": func(o *unstructured.Unstructured) {
			o.Object["status"] = map[string]interface{}{"phase": "Ready"}
		},
		"resourceVersion and generation": func(o *unstructured.Unstructured) {
			o.SetResourceVersion("99")
			o.SetGeneration(7)
		},
		"managedFields": func(o *unstructured.Unstructured) {
			o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: "Update"}})
		},
		"a stripped annotation": func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{
				"note": "kept", "kubectl.kubernetes.io/last-applied-configuration": "{}",
			})
		},
	}
	for name, mutate := range noOps {
		t.Run(name+" is unchanged", func(t *testing.T) {
			f := newDesiredStateChangeFilter()
			require.True(t, offer(f, base(), string(types.OperationCreate)))
			edited := base()
			mutate(edited)
			assert.False(t, offer(f, edited, update))
		})
	}
}

// admittingResyncs is a branch worker's admission as a replay snapshot sees it: it takes resyncs
// until told to refuse them, answering each on its reply channel as the worker does.
type admittingResyncs struct {
	mu       sync.Mutex
	refuse   bool
	requests []*git.ResyncRequest
}

func (a *admittingResyncs) EnqueueResync(request *git.ResyncRequest) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse {
		request.Result <- git.ResyncResult{Err: git.ErrFinalizeQueueFull}
		return false
	}
	a.requests = append(a.requests, request)
	request.Result <- git.ResyncResult{}
	return true
}

func (a *admittingResyncs) setRefuse(refuse bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuse = refuse
}

// recordingAuthors names alice as the author of every write to the object "a", and no one else.
type recordingAuthors struct{}

func (recordingAuthors) ResolveAuthor(_ context.Context, query AuthorQuery) (git.UserInfo, git.AttributionOutcome) {
	if query.Name == "a" {
		return git.UserInfo{Username: "alice"}, git.AttributionResolved
	}
	return git.UserInfo{}, git.AttributionUnresolved
}

// replayMode is how a session gathers its snapshot.
type replayMode string

const (
	initialEvents replayMode = "initial-events"
	listFallback  replayMode = "list-fallback"
)

// filterHarness drives whole watch sessions of one GitTarget's stream against a worker that
// admits live events and replay snapshots until told to refuse them.
type filterHarness struct {
	t        *testing.T
	manager  *Manager
	live     *admittingEnqueuer
	resyncs  *admittingResyncs
	gitDest  types.ResourceReference
	mode     replayMode
	opened   chan *watch.FakeWatcher
	listMu   sync.Mutex
	listNext *unstructured.UnstructuredList
}

func newFilterHarness(t *testing.T, mode replayMode) *filterHarness {
	t.Helper()
	gitDest := types.NewResourceReference("target", "default")
	h := &filterHarness{
		t:       t,
		live:    &admittingEnqueuer{},
		resyncs: &admittingResyncs{},
		gitDest: gitDest,
		mode:    mode,
		opened:  make(chan *watch.FakeWatcher, 1),
	}
	h.manager = &Manager{
		Log: logr.Discard(),
		EventRouter: &EventRouter{
			Log: logr.Discard(),
			gitTargetStreams: map[string]*reconcile.GitTargetEventStream{
				gitDest.Key(): reconcile.NewGitTargetEventStream(
					gitDest.Name, gitDest.Namespace, h.live, logr.Discard()),
			},
			resyncWorker: func(context.Context, types.ResourceReference) (resyncEnqueuer, error) {
				return h.resyncs, nil
			},
		},
		targetWatchOpen: func(
			_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
		) (watch.Interface, error) {
			if mode == listFallback && opts.SendInitialEvents != nil {
				return nil, errors.New("sendInitialEvents: Forbidden: sendInitialEvents is forbidden")
			}
			// Buffered, so a session that ends early never strands a send.
			fw := watch.NewFakeWithChanSize(64, false)
			h.opened <- fw
			return fw, nil
		},
		targetWatchList: func(
			context.Context, schema.GroupVersionResource, string, metav1.ListOptions,
		) (*unstructured.UnstructuredList, error) {
			h.listMu.Lock()
			defer h.listMu.Unlock()
			return h.listNext, nil
		},
	}
	return h
}

// session runs one session that replays objects pinned at rv, then delivers live events. complete
// false ends an initial-events replay before its bookmark. It returns the session's error.
func (h *filterHarness) session(
	stream targetWatchStream, objects []*unstructured.Unstructured, rv string, complete bool, live ...watch.Event,
) error {
	h.t.Helper()
	var events []watch.Event
	switch h.mode {
	case initialEvents:
		for _, obj := range objects {
			events = append(events, watch.Event{Type: watch.Added, Object: obj})
		}
		if complete {
			bookmark := &unstructured.Unstructured{}
			bookmark.SetResourceVersion(rv)
			bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
			events = append(events, watch.Event{Type: watch.Bookmark, Object: bookmark})
			events = append(events, live...)
		}
	case listFallback:
		list := &unstructured.UnstructuredList{}
		list.SetResourceVersion(rv)
		for _, obj := range objects {
			list.Items = append(list.Items, *obj)
		}
		h.listMu.Lock()
		h.listNext = list
		h.listMu.Unlock()
		events = live
	}
	return h.run(stream, false, events)
}

// resume runs one session that resumes from a stored cursor and delivers live events.
func (h *filterHarness) resume(stream targetWatchStream, live ...watch.Event) error {
	h.t.Helper()
	h.manager.WatchCursorStore = &fakeWatchCursorStore{rv: "1", ok: true}
	h.manager.rememberGitTargetUID(h.gitDest.WithUID("uid-1"))
	defer func() { h.manager.WatchCursorStore = nil }()
	return h.run(stream, true, live)
}

func (h *filterHarness) run(stream targetWatchStream, resume bool, events []watch.Event) error {
	h.t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- h.manager.targetWatchReplayAndStream(context.Background(), logr.Discard(), h.gitDest, stream, resume)
	}()
	var fw *watch.FakeWatcher
	select {
	case fw = <-h.opened:
	case err := <-done:
		return err
	case <-time.After(time.Second):
		h.t.Fatal("expected the session to open a watch")
	}
	for _, ev := range events {
		fw.Action(ev.Type, ev.Object)
	}
	fw.Stop()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		h.t.Fatal("expected the session to end")
		return nil
	}
}

// accepted returns the live events the worker took, as "<name>=<value>" for each.
func (h *filterHarness) accepted() []string {
	var out []string
	for _, event := range h.live.accepted() {
		value := ""
		if event.Object != nil {
			value, _, _ = unstructured.NestedString(event.Object.Object, "data", "key")
		}
		out = append(out, event.Identifier.Name+"="+value)
	}
	return out
}

func modified(obj *unstructured.Unstructured) watch.Event {
	return watch.Event{Type: watch.Modified, Object: obj}
}

// statusOnly is obj with a status change and nothing Git holds.
func statusOnly(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.Object["status"] = map[string]interface{}{"observed": obj.GetResourceVersion()}
	return obj
}

func bothReplayModes(t *testing.T, test func(t *testing.T, h *filterHarness)) {
	for _, mode := range []replayMode{initialEvents, listFallback} {
		t.Run(string(mode), func(t *testing.T) { test(t, newFilterHarness(t, mode)) })
	}
}

// The replay regression. Git holds X from a live event; the stream reconnects into a replay that
// finds Z, which the worker writes; the object then goes back to X. That return is a change to
// Git, and a baseline still at X would have dropped it and left Git at Z.
func TestDesiredStateChangeFilter_AnAcceptedReplayReplacesTheBaseline(t *testing.T) {
	bothReplayModes(t, func(t *testing.T, h *filterHarness) {
		stream := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})
		require.ErrorIs(t, h.session(stream, nil, "10", true,
			watch.Event{Type: watch.Added, Object: filterObject("a", "11", "X")}), errTargetWatchClosed)
		require.ErrorIs(t, h.session(stream, []*unstructured.Unstructured{filterObject("a", "20", "Z")}, "20", true,
			modified(filterObject("a", "21", "X"))), errTargetWatchClosed)

		assert.Equal(t, []string{"a=X", "a=X"}, h.accepted(), "the return to X must reach the worker")
		require.Len(t, h.resyncs.requests, 2)
	})
}

// The commit-window regression. B is replayed and then only its status changes, while alice is
// editing A. B's update has no author; routed between alice's two edits, it would close her window
// on the author change and split her save into two commits. The replay is B's baseline, so it is
// filtered and alice's edits reach the worker back to back.
func TestDesiredStateChangeFilter_AReplayedStatusUpdateStaysOutOfAnotherAuthorsWindow(t *testing.T) {
	bothReplayModes(t, func(t *testing.T, h *filterHarness) {
		reader, err := telemetry.InitTestExporter()
		require.NoError(t, err)
		h.manager.AuthorResolver = recordingAuthors{}
		stream := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})

		replayed := []*unstructured.Unstructured{filterObject("a", "10", "A0"), filterObject("b", "10", "B0")}
		require.ErrorIs(t, h.session(stream, replayed, "10", true,
			modified(filterObject("a", "11", "A1")),
			modified(statusOnly(filterObject("b", "12", "B0"))),
			modified(filterObject("a", "13", "A2")),
		), errTargetWatchClosed)

		accepted := h.live.accepted()
		require.Len(t, accepted, 2, "only alice's edits reach the worker")
		for _, event := range accepted {
			assert.Equal(t, "a", event.Identifier.Name)
			assert.Equal(t, "alice", event.UserInfo.Username)
		}
		assert.Equal(t, int64(1), unchangedEvents(t, reader, h.gitDest))
	})
}

// A snapshot the worker refused, or one the session never finished, is not the worker's state, so
// the stream keeps the baselines it had. The retry that is accepted replaces them.
func TestDesiredStateChangeFilter_ARefusedOrIncompleteReplayInstallsNothing(t *testing.T) {
	bothReplayModes(t, func(t *testing.T, h *filterHarness) {
		stream := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})
		require.ErrorIs(t, h.session(stream, []*unstructured.Unstructured{filterObject("a", "10", "X")}, "10", true),
			errTargetWatchClosed)
		baseline := stream.changes.baselines["a"]
		require.NotEmpty(t, baseline)

		h.resyncs.setRefuse(true)
		err := h.session(stream, []*unstructured.Unstructured{filterObject("a", "20", "Z")}, "20", true)
		require.ErrorIs(t, err, git.ErrFinalizeQueueFull)
		assert.Equal(t, baseline, stream.changes.baselines["a"], "a refused snapshot installs no candidate hash")
		h.resyncs.setRefuse(false)

		if h.mode == initialEvents {
			require.ErrorIs(
				t,
				h.session(stream, []*unstructured.Unstructured{filterObject("a", "30", "Z")}, "30", false),
				errTargetWatchClosed,
			)
			assert.Equal(t, baseline, stream.changes.baselines["a"], "an unfinished replay installs nothing")
		}

		require.ErrorIs(t, h.session(stream, []*unstructured.Unstructured{filterObject("a", "40", "Z")}, "40", true,
			modified(statusOnly(filterObject("a", "41", "Z"))),
			modified(filterObject("a", "42", "X")),
		), errTargetWatchClosed)
		assert.Equal(t, []string{"a=X"}, h.accepted(), "the accepted retry is the baseline: Z is unchanged, X routes")
	})
}

// A replay that no longer holds an object takes its baseline with it: the worker's snapshot swept
// it, so its next update is a change, whatever the stream last saw.
func TestDesiredStateChangeFilter_AReplayThatOmitsAnObjectDropsItsBaseline(t *testing.T) {
	bothReplayModes(t, func(t *testing.T, h *filterHarness) {
		stream := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})
		require.ErrorIs(t, h.session(stream, nil, "10", true,
			watch.Event{Type: watch.Added, Object: filterObject("q", "11", "X")}), errTargetWatchClosed)
		require.ErrorIs(t, h.session(stream, nil, "20", true, modified(filterObject("q", "21", "X"))),
			errTargetWatchClosed)
		assert.Equal(t, []string{"q=X", "q=X"}, h.accepted())
	})
}

// A reconnect stays within the stream and keeps its accepted baselines; a replacement stream starts
// with none; and no baseline survives an accepted DELETE.
func TestDesiredStateChangeFilter_LifetimeFollowsTheStream(t *testing.T) {
	h := newFilterHarness(t, initialEvents)
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	stream := testStream(key)
	require.ErrorIs(t, h.session(stream, []*unstructured.Unstructured{filterObject("a", "10", "X")}, "10", true),
		errTargetWatchClosed)

	require.ErrorIs(t, h.resume(stream, modified(statusOnly(filterObject("a", "11", "X")))), errTargetWatchClosed)
	assert.Empty(t, h.accepted(), "a reconnect keeps the replay's baseline")

	replacement := testStream(key)
	require.ErrorIs(t, h.resume(replacement, modified(statusOnly(filterObject("a", "12", "X")))),
		errTargetWatchClosed)
	assert.Equal(t, []string{"a=X"}, h.accepted(), "a replacement stream has no predecessor's baseline")

	require.ErrorIs(t, h.resume(stream,
		watch.Event{Type: watch.Deleted, Object: filterObject("a", "13", "X")},
		modified(filterObject("a", "14", "X")),
	), errTargetWatchClosed)
	assert.Equal(t, []string{"a=X", "a=", "a=X"}, h.accepted(), "no baseline survives the deletion")
}

// runTargetWatch gives each stream it runs a fresh filter, so a stream started again for the same
// collection never inherits a baseline.
func TestRunTargetWatch_GivesEachStreamItsOwnFilter(t *testing.T) {
	h := newFilterHarness(t, initialEvents)
	stream := targetWatchStream{key: targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}, gate: &producerGate{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.manager.runTargetWatch(ctx, logr.Discard(), h.gitDest, stream)
	}()
	fw := <-h.opened
	bookmark := &unstructured.Unstructured{}
	bookmark.SetResourceVersion("10")
	bookmark.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
	fw.Add(filterObject("a", "10", "X"))
	fw.Action(watch.Bookmark, bookmark)
	fw.Modify(statusOnly(filterObject("a", "11", "X")))
	fw.Modify(filterObject("a", "12", "Y"))
	require.Eventually(t, func() bool { return len(h.live.accepted()) == 1 }, time.Second, 10*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, []string{"a=Y"}, h.accepted(), "the stream's own replay was its baseline")
	assert.Nil(t, stream.changes, "the caller's copy is untouched; the filter lives with the run")
}

func unchangedEvents(t *testing.T, reader *sdkmetric.ManualReader, gitDest types.ResourceReference) int64 {
	t.Helper()
	count, _ := telemetry.CollectInt64Sum(reader, watchEventsMetric, map[string]string{
		"gittarget_namespace": gitDest.Namespace, "gittarget_name": gitDest.Name,
		"group": "", "version": "v1", "resource": "configmaps", "outcome": watchOutcomeUnchanged,
	})
	return count
}
