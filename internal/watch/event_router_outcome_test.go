// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestLookupCommitRequestOutcome_ReadsWhatAStoppedWorkerDecided: the shutdown reads the outcome the
// worker that held the request decided, after every worker has stopped, without sending it anything.
// A request no worker holds has none.
func TestLookupCommitRequestOutcome_ReadsWhatAStoppedWorkerDecided(t *testing.T) {
	scheme := eventRouterScheme(t)
	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b-provider", Namespace: "team-a"},
		Spec:       configv1alpha3.GitProviderSpec{URL: "file:///tmp/does-not-need-to-exist"},
	}
	gitTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef: meta.NamespacedObjectReference{Name: gitBranchNameForTest("team-b-provider", "main")},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, gitTarget).Build()
	workerManager := git.NewWorkerManager(
		client, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	router := NewEventRouter(workerManager, nil, client, logr.Discard())
	attach := saveAttach("team-a-config", "team-a")

	_, resolved, err := router.LookupCommitRequestOutcome(context.Background(), attach)
	require.NoError(t, err)
	require.False(t, resolved, "no worker holds the request")

	// The shutdown reads it from inside the manager's stop, which holds lifecycleMu: the lookup must not
	// need that lock.
	var (
		atStop         git.FinalizeResult
		resolvedAtStop bool
		lookupReturned bool
	)
	workerManager.SetAfterWorkersStopped(func(ctx context.Context) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			atStop, resolvedAtStop, _ = router.LookupCommitRequestOutcome(ctx, attach)
		}()
		select {
		case <-done:
			lookupReturned = true
		case <-time.After(5 * time.Second):
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = workerManager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // allow the manager to record its context
	repo := git.RepoIdentity{URL: "file:///tmp/does-not-need-to-exist"}
	gitBranch := gitBranchForTest("team-b-provider", "team-a", "main")
	workerManager.Claims().Observe(git.BranchKeyFor(repo.URL, "main"), git.BranchClaimant{
		GitBranch: gitBranch, UID: "uid-team-b-branch", Created: time.Unix(0, 0),
	}, true)
	require.NoError(t, workerManager.EnsureWorker(ctx, git.WorkerRequest{
		GitBranch: gitBranch,
		Provider:  k8stypes.NamespacedName{Namespace: "team-a", Name: "team-b-provider"},
		Branch:    "main",
		Repo:      repo,
	}))
	require.Eventually(t, func() bool {
		_, resolved, err := router.ServiceCommitRequest(context.Background(), attach)
		require.NoError(t, err)
		return resolved
	}, 5*time.Second, 50*time.Millisecond, "the worker resolves the request")
	cancel()
	<-stopped

	require.True(t, lookupReturned, "the lookup inside the manager's stop does not deadlock")
	require.True(t, resolvedAtStop, "the shutdown's settle step reads the outcome")
	assert.Equal(t, git.FinalizeNoOpenWindow, atStop.Outcome)

	result, resolved, err := router.LookupCommitRequestOutcome(context.Background(), attach)
	require.NoError(t, err)
	require.True(t, resolved, "the stopped worker still answers for the request it decided")
	assert.Equal(t, git.FinalizeNoOpenWindow, result.Outcome)
}
