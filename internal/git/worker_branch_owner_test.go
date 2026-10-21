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
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const ownerTestURL = "https://example.invalid/platform.git"

// TestEnsureWorker_RefusesASecondGitProviderOnOneRepositoryBranch: a worker is keyed by repository
// and branch, and pushes with one GitProvider's credential. A second GitProvider naming the same
// repository, however spelled, gets no worker of its own on that branch; on another branch it does.
func TestEnsureWorker_RefusesASecondGitProviderOnOneRepositoryBranch(t *testing.T) {
	m, ctx := startedManager(t)
	teamA := RepoIdentity{ProviderUID: "uid-a", URL: ownerTestURL}
	teamB := RepoIdentity{ProviderUID: "uid-b", URL: "git@example.invalid:platform"}

	require.NoError(t, m.EnsureWorker(ctx, "monorepo", "team-a", "main", teamA, ""))
	err := m.EnsureWorker(ctx, "monorepo", "team-b", "main", teamB, "")

	require.ErrorIs(t, err, ErrBranchOwnedElsewhere)
	_, found := m.GetWorkerForTarget("monorepo", "team-b", "main")
	assert.False(t, found, "the refused GitProvider has no worker")
	owner, found := m.GetWorkerForTarget("monorepo", "team-a", "main")
	require.True(t, found)
	assert.Equal(t, BranchKeyFor(ownerTestURL, "main"), owner.branchKey())

	require.NoError(t, m.EnsureWorker(ctx, "monorepo", "team-b", "release", teamB, ""),
		"another branch of the repository is another worker")
}

// TestReconcileWorkers_StopsTheWorkerOfAGitProviderThatDoesNotOwnItsBranch: two GitTargets created
// too close together for either reconcile to see the other can both be wired. The sweep decides as
// the reconciler does, so the younger GitTarget's worker is stopped, and the owner's next reconcile
// gets the branch.
func TestReconcileWorkers_StopsTheWorkerOfAGitProviderThatDoesNotOwnItsBranch(t *testing.T) {
	m, ctx := startedManager(t)
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, ns := range []string{"team-a", "team-b"} {
		require.NoError(t, m.Client.Create(ctx, &configv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "monorepo"},
			Spec:       configv1alpha3.GitProviderSpec{URL: ownerTestURL, AllowedBranches: []string{"*"}},
		}))
	}
	target := func(ns string, age time.Duration) client.Object {
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: "apps", CreationTimestamp: metav1.NewTime(created.Add(-age)),
			},
			Spec: configv1alpha3.GitTargetSpec{
				GitProviderRef: meta.NamespacedObjectReference{Name: "monorepo"}, Branch: "main", Path: ns,
			},
		}
	}
	require.NoError(t, m.Client.Create(ctx, target("team-a", 2*time.Hour)))
	require.NoError(t, m.Client.Create(ctx, target("team-b", time.Hour)))

	// The younger GitTarget's reconcile won the race to the worker.
	require.NoError(t, m.EnsureWorker(ctx, "monorepo", "team-b", "main", RepoIdentity{URL: ownerTestURL}, ""))
	require.ErrorIs(t, m.EnsureWorker(ctx, "monorepo", "team-a", "main", RepoIdentity{URL: ownerTestURL}, ""),
		ErrBranchOwnedElsewhere)

	require.NoError(t, m.ReconcileWorkers(ctx))
	_, found := m.GetWorkerForTarget("monorepo", "team-b", "main")
	assert.False(t, found, "the GitProvider of the younger GitTarget does not own the branch")

	require.NoError(t, m.EnsureWorker(ctx, "monorepo", "team-a", "main", RepoIdentity{URL: ownerTestURL}, ""))
	require.NoError(t, m.ReconcileWorkers(context.Background()))
	_, found = m.GetWorkerForTarget("monorepo", "team-a", "main")
	assert.True(t, found, "and the owner's worker is the one the sweep keeps")
}
