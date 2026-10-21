// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const ownerTestURL = "https://example.invalid/platform.git"

// TestEnsureWorker_OnlyTheHolderGetsAWorker: a worker is keyed by repository and branch, and pushes
// with the credential of the GitBranch holding it. A GitBranch that does not hold the branch gets no
// worker on it, however its GitProvider spells the repository; on another branch it can.
func TestEnsureWorker_OnlyTheHolderGetsAWorker(t *testing.T) {
	m, ctx := startedManager(t)
	teamA := RepoIdentity{ProviderUID: "uid-a", URL: ownerTestURL}
	teamB := RepoIdentity{ProviderUID: "uid-b", URL: "git@example.invalid:platform"}

	require.NoError(t, m.EnsureWorker(ctx, claimForTest(m, "monorepo", "team-a", "main", teamA)))
	refused := WorkerRequest{
		GitBranch: testGitBranch("monorepo", "team-b", "main"), Provider: "monorepo", Branch: "main", Repo: teamB,
	}
	require.ErrorIs(t, m.EnsureWorker(ctx, refused), ErrBranchOwnedElsewhere)
	_, found := m.WorkerForGitBranch(refused.GitBranch)
	assert.False(t, found, "the GitBranch that does not hold the branch has no worker")
	owner, found := m.WorkerForGitBranch(testGitBranch("monorepo", "team-a", "main"))
	require.True(t, found)
	assert.Equal(t, BranchKeyFor(ownerTestURL, "main"), owner.branchKey())

	require.NoError(t, m.EnsureWorker(ctx, claimForTest(m, "monorepo", "team-b", "release", teamB)),
		"another branch of the repository is another worker")
}

// TestReconcileWorkers_StopsTheWorkerOfAGitBranchThatLostItsBranch: an older GitBranch that proves
// itself later takes the branch over (rule 2). The sweep stops the previous holder's worker, the new
// holder's next reconcile starts its own, and the sweep keeps that one while a GitTarget references
// its GitBranch.
func TestReconcileWorkers_StopsTheWorkerOfAGitBranchThatLostItsBranch(t *testing.T) {
	m, ctx := startedManager(t)
	repo := RepoIdentity{URL: ownerTestURL}
	key := BranchKeyFor(ownerTestURL, "main")
	younger := testGitBranch("monorepo", "team-b", "main")
	older := testGitBranch("monorepo", "team-a", "main")
	for _, gitBranch := range []k8stypes.NamespacedName{younger, older} {
		require.NoError(t, m.Client.Create(ctx, &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Namespace: gitBranch.Namespace, Name: "apps"},
			Spec: configv1alpha3.GitTargetSpec{
				GitBranchRef: &meta.NamespacedObjectReference{Name: gitBranch.Name}, Path: gitBranch.Namespace,
			},
		}))
	}

	require.NoError(t, m.EnsureWorker(ctx, claimForTest(m, "monorepo", "team-b", "main", repo)))
	m.Claims().
		Observe(key, BranchClaimant{GitBranch: older, UID: "older", Created: time.Unix(0, 0).Add(-time.Hour)}, true)
	require.ErrorIs(
		t,
		m.EnsureWorker(ctx, WorkerRequest{GitBranch: older, Provider: "monorepo", Branch: "main", Repo: repo}),
		ErrBranchOwnedElsewhere,
		"the previous holder's worker is still live",
	)

	require.NoError(t, m.ReconcileWorkers(ctx))
	_, found := m.WorkerForGitBranch(younger)
	assert.False(t, found, "the GitBranch that lost the branch loses its worker")

	require.NoError(
		t,
		m.EnsureWorker(ctx, WorkerRequest{GitBranch: older, Provider: "monorepo", Branch: "main", Repo: repo}),
	)
	require.NoError(t, m.ReconcileWorkers(context.Background()))
	_, found = m.WorkerForGitBranch(older)
	assert.True(t, found, "and the holder's worker is the one the sweep keeps")
}

// TestRetireGitBranch_StopsTheWorkerAndForgetsTheBranch: a GitBranch being deleted stops its worker
// before its finalizer goes, and nothing the manager kept about the branch outlives it.
func TestRetireGitBranch_StopsTheWorkerAndForgetsTheBranch(t *testing.T) {
	m, ctx := startedManager(t)
	repo := RepoIdentity{URL: ownerTestURL}
	request := claimForTest(m, "monorepo", "platform", "main", repo)
	require.NoError(t, m.EnsureWorker(ctx, request))
	worker, found := m.WorkerForGitBranch(request.GitBranch)
	require.True(t, found)
	m.recordRemoteObservation(worker.branchKey(), RemoteObservation{Commit: "abc", At: time.Now()})

	m.RetireGitBranch(request.GitBranch)

	_, found = m.WorkerForGitBranch(request.GitBranch)
	assert.False(t, found)
	_, known := m.RemoteForBranch(worker.branchKey())
	assert.False(t, known, "the next holder starts from the remote")
	m.RetireGitBranch(request.GitBranch) // a second call finds nothing to do
}
