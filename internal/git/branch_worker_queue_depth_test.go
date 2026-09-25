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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
// queueDepthSamples runs on the metric SDK's collection goroutine. Worker removal used to hold
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

	var removed sync.WaitGroup
	removed.Add(1)
	go func() {
		defer removed.Done()
		manager.removeWorkers([]BranchKey{{RepoNamespace: "test-ns", RepoName: "test-provider", Branch: "main"}},
			"test")
	}()

	sampled := make(chan int, 1)
	go func() {
		// Give the removal a moment to reach Stop() before sampling.
		time.Sleep(50 * time.Millisecond)
		sampled <- len(manager.queueDepthSamples())
	}()

	select {
	case <-sampled:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue-depth source blocked on a worker shutdown; it must not wait on the lifecycle lock")
	}

	release()
	removed.Wait()
}

// A replacement worker must not start while the one it replaces is still stopping.
//
// Workers for one BranchKey share an on-disk clone (repoPath is keyed by the provider UID and branch), so
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
		manager.removeWorkers([]BranchKey{key}, "test")
	}()
	<-stopping
	time.Sleep(50 * time.Millisecond) // let the removal reach Stop()

	ensured := make(chan struct{})
	go func() {
		_ = manager.EnsureWorker(context.Background(), "test-provider", "test-ns", "main", RepoIdentity{})
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

// A sweep must not stop a worker EnsureWorker has just handed to a caller.
//
// ReconcileWorkers chooses its orphans from the map and then removes them. If EnsureWorker can run
// between those two steps it finds the key present, returns "already there" without creating
// anything, and the sweep then stops the worker that GitTarget was just told it has — leaving it
// with no worker until its next reconcile, and its live events failing to route in the meantime.
// Holding lifecycleMu across the selection AND the removal is what closes it, and the seam is what
// makes that observable: without the lock the EnsureWorker below completes inside the window and
// the key ends up with no worker at all.
func TestReconcileWorkers_DoesNotStopAWorkerEnsureWorkerJustHandedOut(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	key := BranchKey{RepoNamespace: "test-ns", RepoName: "test-provider", Branch: "main"}
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	manager := &WorkerManager{
		Log:     logr.Discard(),
		ctx:     context.Background(),
		Client:  fake.NewClientBuilder().WithScheme(scheme).Build(), // no GitTargets: every worker is an orphan
		workers: map[BranchKey]*BranchWorker{},
	}
	retiring := newMetricsTestWorker()
	manager.workers[key] = retiring

	ensured := make(chan struct{})
	afterOrphanSelection = func() {
		go func() {
			defer close(ensured)
			// The SAME repository the retiring worker is about, deliberately: that is what makes
			// an unguarded EnsureWorker take its "already there" path, which is the regression
			// this test is here to catch.
			_ = manager.EnsureWorker(context.Background(), "test-provider", "test-ns", "main", retiring.repo)
		}()
		// Long enough for an unguarded EnsureWorker to finish inside the window; a guarded one is
		// blocked on lifecycleMu and finishes after the sweep instead.
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { afterOrphanSelection = nil })

	require.NoError(t, manager.ReconcileWorkers(context.Background()))

	select {
	case <-ensured:
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureWorker never returned")
	}

	manager.mu.RLock()
	got, exists := manager.workers[key]
	manager.mu.RUnlock()
	t.Cleanup(func() {
		if exists && got != retiring {
			got.Stop()
		}
	})
	require.True(t, exists, "the target that asked for a worker must have one, not a slot the sweep emptied")
	assert.NotSame(t, retiring, got, "and it must be a fresh one, not the worker the sweep retired")
}

// A sweep must not stop a worker created for a GitTarget that its own snapshot is too old to
// contain.
//
// The selection window above is only half of it. ReconcileWorkers decides from the API, so a
// target created AFTER that list is taken is absent from it — and the worker EnsureWorker creates
// for that target moments later matches no needed key, so the sweep stops it, taking its queue
// and its checkout with it. The target is then wired to a dead worker until its next reconcile.
//
// The interceptor stands in the window a list outside the lock would open: it runs while the sweep
// is reading the targets, and a guarded sweep leaves the EnsureWorker below blocked on lifecycleMu
// until the removal is done.
func TestReconcileWorkers_DoesNotStopAWorkerCreatedAfterItsSnapshot(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	key := BranchKey{RepoNamespace: "test-ns", RepoName: "newcomer-provider", Branch: "main"}
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	manager := &WorkerManager{
		Log:     logr.Discard(),
		ctx:     context.Background(),
		workers: map[BranchKey]*BranchWorker{},
	}

	ensured := make(chan struct{})
	var once sync.Once
	manager.Client = interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(scheme).Build(),
		interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				// A GitTarget is created right after this snapshot was taken, and its reconcile
				// asks for the branch worker it needs.
				once.Do(func() {
					go func() {
						defer close(ensured)
						_ = manager.EnsureWorker(
							context.Background(), "newcomer-provider", "test-ns", "main", RepoIdentity{})
					}()
					time.Sleep(200 * time.Millisecond)
				})
				return nil
			},
		})

	require.NoError(t, manager.ReconcileWorkers(context.Background()))

	select {
	case <-ensured:
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureWorker never returned")
	}

	manager.mu.RLock()
	created, exists := manager.workers[key]
	manager.mu.RUnlock()
	t.Cleanup(func() {
		if exists {
			created.Stop()
		}
	})
	assert.True(t, exists,
		"a worker created after the sweep's snapshot is not an orphan; it is younger than the question")
}
