// SPDX-License-Identifier: Apache-2.0

package git

// A branch worker is keyed by (GitProvider namespace, GitProvider name, branch), and none of those
// fields says which REPOSITORY it is about. spec.url is immutable and repointed by deleting the
// GitProvider and creating it again, which the key survives untouched, so one live worker could
// meet a second repository holding everything it learned about the first: its clone, its base
// trust, its observation and its retained writes.
//
// These tests are about the replacement that makes that impossible. Meeting a different repository
// is not a state change to be undone field by field; it is a different subject, so the worker for
// the old one is stopped and a fresh one takes the slot.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// startedManager is a WorkerManager with its Runnable context published, which is what EnsureWorker
// hands to every worker it starts.
func startedManager(t *testing.T) (*WorkerManager, context.Context) {
	t.Helper()

	// Replacing a worker DELETES its on-disk state, so every test here points that state at its
	// own directory rather than the shared root a developer's operator also writes under.
	withTemporaryWorkerStateRoot(t)

	k8sClient := fake.NewClientBuilder().WithScheme(setupScheme()).Build()
	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go func() { _ = manager.Start(ctx) }()
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.ctx != nil
	}, 5*time.Second, 10*time.Millisecond, "the manager never published its context")
	return manager, ctx
}

// TestEnsureWorker_KeepsTheWorkerWhenTheRepositoryIsUnchanged is the rule every other test here
// depends on. The comparison runs on EVERY reconcile of every GitTarget on the branch, so an
// identity that is not stable across steady ticks is not a correctness detail: it is a worker
// restart loop, and one that would throw away a commit window each time round.
func TestEnsureWorker_KeepsTheWorkerWhenTheRepositoryIsUnchanged(t *testing.T) {
	manager, ctx := startedManager(t)
	repo := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/repo.git"}

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo))
	first, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	for range 3 {
		require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo))
	}

	second, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	assert.Same(t, first, second, "a steady tick must leave the worker exactly where it was")
	assert.NoError(t, first.ctx.Err(), "and must not have stopped it")
}

// TestEnsureWorker_ReplacesTheWorkerWhenTheProviderIsRecreated is the ordinary repoint: the
// operator deletes the GitProvider and creates it again against another repository, which mints a
// new UID.
func TestEnsureWorker_ReplacesTheWorkerWhenTheProviderIsRecreated(t *testing.T) {
	manager, ctx := startedManager(t)
	before := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	after := RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", before))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", after))

	replacement, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	assert.NotSame(t, old, replacement, "the slot holds the worker for the repository named NOW")
	assert.Equal(t, after, replacement.repo)
	assert.Error(t, old.ctx.Err(), "the worker for the old repository must have been stopped")
}

// TestEnsureWorker_ReplacesOnARecreateThatKeepsTheURL. The UID is the identity, and it changes on
// every recreate — including one that spells the same URL. Replacing there costs a re-clone of
// state that was arguably still good; not replacing would mean trusting that a repository somebody
// recreated the provider for is the same one, which is the belief this whole mechanism removes.
func TestEnsureWorker_ReplacesOnARecreateThatKeepsTheURL(t *testing.T) {
	manager, ctx := startedManager(t)
	const url = "https://example.invalid/repo.git"

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: url}))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-2", URL: url}))

	replacement, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	assert.NotSame(t, old, replacement, "a recreated GitProvider is a new object, so it is a new subject")
}

// TestEnsureWorker_ReplacesWhenTheURLMovedUnderTheSameObject is the safety-net half of the pair.
// spec.url is immutable, so a URL that changes without a new UID can only come from a rule that was
// bypassed — a CRD reinstalled without the CEL validation, or a write straight to etcd. The UID
// cannot see it, which is exactly why the URL is compared beside it.
func TestEnsureWorker_ReplacesWhenTheURLMovedUnderTheSameObject(t *testing.T) {
	manager, ctx := startedManager(t)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/second.git"}))

	replacement, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	assert.NotSame(t, old, replacement, "a differing URL is a different repository, whatever the UID says")
}

// TestEnsureWorker_ReplacementTakesTheOldCheckoutWithIt. The clone is the half of the worker that
// survives a restart, and it is keyed by remote URL under the worker's own directory: leaving it
// behind would accumulate one tree per repository the operator ever repointed away from.
func TestEnsureWorker_ReplacementTakesTheOldCheckoutWithIt(t *testing.T) {
	manager, ctx := startedManager(t)
	const oldURL = "https://example.invalid/first.git"

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: oldURL}))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	// Stand in for the clone a first publication would have left behind.
	checkout := old.repoPathForRemote(oldURL)
	require.NoError(t, os.MkdirAll(checkout, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}))

	_, err := os.Stat(checkout)
	assert.True(t, os.IsNotExist(err), "the replaced worker's checkout must go with it")
}

// TestEnsureWorker_ReplacementDropsWhatTheOldWorkerWasHolding states the one behaviour change
// deliberately: queued events do not follow the worker into the new repository.
//
// They were computed against the old tree, any local commits behind retained writes are in the old
// clone, and the resync rebuilds the whole folder from the cluster. Replaying them would be writing
// one repository's content into another on the strength of a queue position.
func TestEnsureWorker_ReplacementDropsWhatTheOldWorkerWasHolding(t *testing.T) {
	manager, ctx := startedManager(t)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)

	// Enqueued without a loop that can reach a remote, so they are still in hand at replacement.
	old.inflightItems.Add(2)
	old.eventQueue <- WorkItem{}
	old.eventQueue <- WorkItem{}

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}))

	replacement, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	assert.Zero(t, replacement.queueDepth(),
		"the new repository starts empty; nothing computed against the old tree follows it")
}

// TestEnsureWorker_TheSlotReportsOneQueueDepthAcrossAReplacement. git_queue_depth is labelled
// (provider namespace, provider name, branch) — the worker's key, which a replacement does not
// change — so two workers live at once for one slot would publish two series under one label set
// and silently add them together.
func TestEnsureWorker_TheSlotReportsOneQueueDepthAcrossAReplacement(t *testing.T) {
	manager, ctx := startedManager(t)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}))
	require.Len(t, manager.queueDepthSamples(), 1)

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main",
		RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}))

	assert.Len(t, manager.queueDepthSamples(), 1,
		"one slot is one series, whatever happened to the worker behind it")
}

// TestRepoIdentityString_DoesNotPrintACredential. `spec.url` is validated for length and nothing
// else, so it accepts userinfo, and a replacement logs both identities at default verbosity. The
// line has to stay useful — telling two repositories apart is its whole job — so the userinfo is
// stripped rather than the URL withheld.
func TestRepoIdentityString_DoesNotPrintACredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{
			"user and token",
			"https://git:ghp_supersecret@example.invalid/repo.git",
			"https://example.invalid/repo.git (uid uid-1)",
		},
		{"user only", "https://git@example.invalid/repo.git", "https://example.invalid/repo.git (uid uid-1)"},
		{"no userinfo", "https://example.invalid/repo.git", "https://example.invalid/repo.git (uid uid-1)"},
		{"ssh scp form, which url.Parse does not read as a URL", "git@example.invalid:org/repo.git",
			"git@example.invalid:org/repo.git (uid uid-1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RepoIdentity{ProviderUID: "uid-1", URL: tc.url}.String()
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "ghp_supersecret")
		})
	}
}

// TestEnqueue_RefusesOnceTheWorkerIsStopping is the cursor-safety contract across a replacement.
//
// A worker is stopped while the process keeps running, and the sibling GitTargets on its branch go
// on holding streams that point at it until their own next reconcile. Shutdown DRAINS the queue
// without processing it, so an event accepted in that window is thrown away — while the `true`
// return has already told the watch loop it may advance its durable cursor past it. Refusing turns
// that silent loss into the drop path every producer already handles.
func TestEnqueue_RefusesOnceTheWorkerIsStopping(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	worker := newMetricsTestWorker()
	// The loop reads its GitProvider on every pass; with none present it finds nothing to do,
	// which is all this test needs from it.
	worker.Client = fake.NewClientBuilder().WithScheme(setupScheme()).Build()
	require.NoError(t, worker.Start(context.Background()))

	require.True(t, worker.Enqueue(Event{Operation: "UPDATE"}),
		"a running worker accepts the event and owns it from then on")

	worker.Stop()

	assert.False(t, worker.Enqueue(Event{Operation: "UPDATE"}),
		"an event the shutdown drain would discard must not be reported as accepted")
	assert.False(t, worker.EnqueueResync(&ResyncRequest{}),
		"and a resync must be answered rather than left waiting on a reply that never comes")
}
