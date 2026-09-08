// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Releasing a handled item must never let the depth read 0 while the worker still holds the work.
//
// queueDepth() sums the in-flight count and the retained-work flag, so an item that opened a commit
// window is covered by the first until the second is published. Releasing the count first leaves a
// window where neither covers it, and the loop does real work inside that window
// (serviceCommitRequests, applyDeferredHeals) before the flag is synced. A scrape landing there
// reports a drained queue for a worker holding an uncommitted event, which can satisfy a drain gate
// early.
func TestReleaseHandledItem_PublishesRetainedWorkBeforeReleasingInflight(t *testing.T) {
	w := newMetricsTestWorker()
	w.inflightItems.Store(1)

	loop := newBranchWorkerEventLoop(w, time.Second)
	// The state handleQueueItem leaves behind for an event that opened a commit window.
	loop.openWindow = &openWindow{}

	require.Positive(t, w.queueDepth(), "the in-flight count covers the item before the release")
	loop.releaseHandledItem()
	assert.Positive(t, w.queueDepth(), "the retained-work flag must cover it after the release")
}

// The git_queue_depth source must not wait on the worker lifecycle lock.
//
// queueDepthSamples runs on the metric SDK's collection goroutine. UnregisterTarget used to hold
// m.mu across worker.Stop(), which waits for the loop goroutine to finish, so a slow shutdown
// blocked collection for its whole duration — the staleness the observable gauge exists to remove,
// reintroduced through the manager's own lock.
func TestQueueDepthSamples_NotBlockedByASlowWorkerShutdown(t *testing.T) {
	manager := &WorkerManager{Log: logr.Discard(), workers: map[BranchKey]*BranchWorker{}}
	w := newMetricsTestWorker()
	manager.workers[BranchKey{RepoNamespace: "test-ns", RepoName: "test-provider", Branch: "main"}] = w

	// A worker whose Stop() blocks: started, so Stop proceeds, and one wg entry nothing releases
	// until this test does.
	w.started = true
	w.cancelFunc = func() {}
	w.wg.Add(1)
	var once sync.Once
	release := func() { once.Do(w.wg.Done) }
	t.Cleanup(release)

	var unregistered sync.WaitGroup
	unregistered.Add(1)
	go func() {
		defer unregistered.Done()
		_ = manager.UnregisterTarget("", "", "test-provider", "test-ns", "main")
	}()

	sampled := make(chan int, 1)
	go func() {
		// Give the unregister a moment to reach Stop() before sampling.
		time.Sleep(50 * time.Millisecond)
		sampled <- len(manager.queueDepthSamples())
	}()

	select {
	case <-sampled:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue-depth source blocked on a worker shutdown; it must not wait on the lifecycle lock")
	}

	release()
	unregistered.Wait()
}

// A replacement worker must not start while the one it replaces is still stopping.
//
// Workers for one BranchKey share an on-disk clone (repoPathForRemote is keyed by remote URL), so
// two live at once would operate on the same worktree. Detaching under m.mu and stopping outside it
// keeps the scrape unblocked, but the guarantee has to survive that: lifecycleMu is what holds it.
func TestEnsureWorker_WaitsForTheWorkerItReplacesToStop(t *testing.T) {
	key := BranchKey{RepoNamespace: "test-ns", RepoName: "test-provider", Branch: "main"}
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	manager := &WorkerManager{
		Log: logr.Discard(),
		ctx: context.Background(),
		// The replacement really starts, so it needs a client; with no GitProvider present its
		// loop finds nothing to do, which is all this test needs from it.
		Client:  fake.NewClientBuilder().WithScheme(scheme).Build(),
		workers: map[BranchKey]*BranchWorker{},
	}
	w := newMetricsTestWorker()
	manager.workers[key] = w

	w.started = true
	w.cancelFunc = func() {}
	w.wg.Add(1)
	var once sync.Once
	release := func() { once.Do(w.wg.Done) }
	t.Cleanup(release)
	t.Cleanup(func() {
		manager.mu.RLock()
		replacement, ok := manager.workers[key]
		manager.mu.RUnlock()
		if ok {
			replacement.Stop()
		}
	})

	stopping := make(chan struct{})
	go func() {
		close(stopping)
		_ = manager.UnregisterTarget("", "", "test-provider", "test-ns", "main")
	}()
	<-stopping
	time.Sleep(50 * time.Millisecond) // let the unregister reach Stop()

	ensured := make(chan struct{})
	go func() {
		_ = manager.EnsureWorker(context.Background(), "test-provider", "test-ns", "main")
		close(ensured)
	}()

	select {
	case <-ensured:
		t.Fatal("a replacement worker started while its predecessor was still stopping")
	case <-time.After(200 * time.Millisecond):
	}

	release()
	select {
	case <-ensured:
	case <-time.After(5 * time.Second):
		t.Fatal("the replacement never started after its predecessor stopped")
	}
}
