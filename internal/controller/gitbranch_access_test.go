// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// sharedProvider is the GitProvider platform/shared-repo, letting the named namespaces claim every
// branch.
func sharedProvider(namespaces ...string) *configbutleraiv1alpha3.GitProvider {
	provider := branchProvider("platform", "shared-repo")
	provider.Spec.AllowedGitBranches = []configbutleraiv1alpha3.GitProviderGitBranchGrant{
		{
			ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
				From:  configbutleraiv1alpha3.GrantFromNames,
				Names: grantNames(namespaces...),
			},
			BranchPatterns: []configbutleraiv1alpha3.BranchPattern{"*"},
		},
	}
	return provider
}

// tenantBranch is a GitBranch namespace/main claiming main through platform/shared-repo, created
// age ago.
func tenantBranch(namespace string, age time.Duration) *configbutleraiv1alpha3.GitBranch {
	branch := branchObject(namespace, "main", "shared-repo", "main", age)
	branch.Spec.GitProviderRef = meta.NamespacedObjectReference{Name: "shared-repo", Namespace: "platform"}
	return branch
}

// grantOnly replaces the shared provider's grant so it admits exactly namespaces.
func (f *gitBranchFixture) grantOnly(t *testing.T, namespaces ...string) {
	t.Helper()
	stored := &configbutleraiv1alpha3.GitProvider{}
	key := k8stypes.NamespacedName{Namespace: "platform", Name: "shared-repo"}
	require.NoError(t, f.client.Get(context.Background(), key, stored))
	stored.Spec.AllowedGitBranches = sharedProvider(namespaces...).Spec.AllowedGitBranches
	if len(namespaces) == 0 {
		stored.Spec.AllowedGitBranches = nil
	}
	require.NoError(t, f.client.Update(context.Background(), stored))
}

func (f *gitBranchFixture) holder() (git.BranchClaimant, bool) {
	return f.workers.Claims().Holder(git.BranchKeyFor(gitBranchTestURL, "main"))
}

// TestGitBranch_AnUnadmittedGitBranchNeverOpensAPushSession: admission comes before the proof, so a
// GitBranch no grant admits never uses the shared credential.
func TestGitBranch_AnUnadmittedGitBranchNeverOpensAPushSession(t *testing.T) {
	f := newGitBranchFixture(t, sharedProvider("team-a"), tenantBranch("team-b", time.Hour))
	proofs := 0
	f.r.proveProvider = func(*configbutleraiv1alpha3.GitProvider) error { proofs++; return nil }

	got := f.reconcile(t, "team-b", "main")

	assert.Zero(t, proofs, "no push session is opened with a credential the GitBranch was not granted")
	assert.Equal(t, GitBranchReasonNamespaceNotAuthorized, readyOf(got).Reason)
	assert.Contains(
		t,
		readyOf(got).Message,
		`GitProvider platform/shared-repo admits team-a in spec.allowedGitBranches; `+
			`namespace "team-b" is not admitted`,
	)
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
	_, held := f.holder()
	assert.False(t, held)
}

// TestGitBranch_AGitBranchAdmittedLaterTakesTheBranchByAge: once a grant admits it, a GitBranch proves
// itself and becomes a candidate like any other, and the oldest candidate holds the branch, also
// when the claims are recomputed after a restart.
func TestGitBranch_AGitBranchAdmittedLaterTakesTheBranchByAge(t *testing.T) {
	f := newGitBranchFixture(t, sharedProvider("team-b"),
		tenantBranch("team-a", 2*time.Hour), tenantBranch("team-b", time.Hour))
	f.reconcile(t, "team-a", "main")
	f.reconcile(t, "team-b", "main")
	holder, held := f.holder()
	require.True(t, held)
	assert.Equal(t, "team-b/main", holder.GitBranch.String(), "only the admitted one holds the branch")

	f.grantOnly(t, "team-a", "team-b")
	older := f.reconcile(t, "team-a", "main")
	younger := f.reconcile(t, "team-b", "main")

	assert.Equal(t, metav1.ConditionTrue, readyOf(older).Status, readyOf(older).Message)
	assert.Equal(t, GitBranchReasonBranchConflict, readyOf(younger).Reason,
		"the older GitBranch takes the branch once it is admitted and proves itself")

	// After a restart the claims are recomputed from the objects alone, in whatever order.
	restarted := newGitBranchFixture(t)
	restarted.client = f.client
	restarted.r.Client = f.client
	restarted.reconcile(t, "team-b", "main")
	restarted.reconcile(t, "team-a", "main")
	holder, held = restarted.holder()
	require.True(t, held)
	assert.Equal(t, "team-a/main", holder.GitBranch.String())
}

// TestGitBranch_RevocationDrainsAndKeepsTheFinalizer: a GitBranch whose grant is withdrawn gives up
// its claim and retires its worker, as deletion does, but keeps its finalizer and reports why. The
// old worker takes nothing new, and the successor starts a worker of its own.
func TestGitBranch_RevocationDrainsAndKeepsTheFinalizer(t *testing.T) {
	f := newGitBranchFixture(t, sharedProvider("team-a", "team-b"),
		tenantBranch("team-a", 2*time.Hour), tenantBranch("team-b", time.Hour))
	f.reconcile(t, "team-a", "main")
	f.reconcile(t, "team-b", "main")
	revoked := k8stypes.NamespacedName{Namespace: "team-a", Name: "main"}
	repo := git.RepoIdentity{ProviderUID: "uid-shared-repo", URL: gitBranchTestURL}
	provider := k8stypes.NamespacedName{Namespace: "platform", Name: "shared-repo"}
	require.NoError(t, f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
		GitBranch: revoked, Provider: provider, Branch: "main", Repo: repo,
	}))
	old, live := f.workers.WorkerForGitBranch(revoked)
	require.True(t, live)

	f.grantOnly(t, "team-b")
	got := f.reconcile(t, "team-a", "main")

	assert.Equal(t, GitBranchReasonNamespaceNotAuthorized, readyOf(got).Reason)
	assert.Contains(t, readyOf(got).Message, "gave up the branch")
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
	assert.Contains(t, got.GetFinalizers(), GitBranchFinalizer, "a revoked GitBranch is not being deleted")
	_, live = f.workers.WorkerForGitBranch(revoked)
	assert.False(t, live, "the worker retired, pushing or refusing what it held")
	assert.False(t, old.Enqueue(git.Event{}), "an event arriving after the revocation is refused")

	_, held := f.holder()
	assert.False(t, held, "the branch passes to nobody until the successor has reconciled")
	require.Len(t, f.workers.GitBranchEvents(), 1, "the successor is woken")
	<-f.workers.GitBranchEvents()
	f.reconcile(t, "team-b", "main")
	holder, held := f.holder()
	require.True(t, held)
	assert.Equal(t, "team-b/main", holder.GitBranch.String())
	successor := k8stypes.NamespacedName{Namespace: "team-b", Name: "main"}
	require.NoError(t, f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
		GitBranch: successor, Provider: provider, Branch: "main", Repo: repo,
	}))
	fresh, live := f.workers.WorkerForGitBranch(successor)
	require.True(t, live)
	assert.NotSame(t, old, fresh, "the successor receives none of the old worker's work")

	// Restoring the grant re-admits it; it proves itself and, being older, takes the branch back.
	f.grantOnly(t, "team-a", "team-b")
	back := f.reconcile(t, "team-a", "main")
	assert.Equal(t, metav1.ConditionTrue, readyOf(back).Status, readyOf(back).Message)
}

// TestGitBranch_OneEditRevokesTwoCandidates: a grant edit that revokes the holder and its successor
// together hands the branch to neither. The holder's release elects nobody, so a GitTarget reconcile
// racing the handoff cannot start the revoked successor's worker; each waiting GitBranch takes the
// branch only through its own reconcile, which checks its grant first, in whichever order they run.
func TestGitBranch_OneEditRevokesTwoCandidates(t *testing.T) {
	for _, order := range [][]string{{"team-b", "team-c"}, {"team-c", "team-b"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			f := newGitBranchFixture(t, sharedProvider("team-a", "team-b", "team-c"),
				tenantBranch("team-a", 3*time.Hour), tenantBranch("team-b", 2*time.Hour),
				tenantBranch("team-c", time.Hour))
			for _, ns := range []string{"team-a", "team-b", "team-c"} {
				f.reconcile(t, ns, "main")
			}

			f.grantOnly(t, "team-c")
			f.reconcile(t, "team-a", "main")

			revoked := k8stypes.NamespacedName{Namespace: "team-b", Name: "main"}
			err := f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
				GitBranch: revoked, Provider: k8stypes.NamespacedName{Namespace: "platform", Name: "shared-repo"},
				Branch: "main", Repo: git.RepoIdentity{ProviderUID: "uid-shared-repo", URL: gitBranchTestURL},
			})
			require.ErrorIs(t, err, git.ErrBranchOwnedElsewhere, "the revoked successor was handed nothing")
			woken := []string{}
			for len(f.workers.GitBranchEvents()) > 0 {
				event := <-f.workers.GitBranchEvents()
				woken = append(woken, event.Object.GetNamespace())
			}
			assert.Equal(t, []string{"team-b", "team-c"}, woken, "the waiting GitBranches are woken, oldest first")

			for _, ns := range order {
				f.reconcile(t, ns, "main")
			}
			holder, held := f.holder()
			require.True(t, held)
			assert.Equal(t, "team-c/main", holder.GitBranch.String(), "team-b, also revoked, never holds the branch")
			_, live := f.workers.WorkerForGitBranch(revoked)
			assert.False(t, live)
			got := &configbutleraiv1alpha3.GitBranch{}
			require.NoError(t, f.client.Get(context.Background(), revoked, got))
			assert.Equal(t, GitBranchReasonNamespaceNotAuthorized, readyOf(got).Reason)
		})
	}
}

// TestGitBranch_ANamespaceLabelLossRevokesASelectorGrant: a label removed from a claimant's namespace
// revokes a Selector grant on its next reconcile.
func TestGitBranch_ANamespaceLabelLossRevokesASelectorGrant(t *testing.T) {
	tenant := map[string]string{"tenant": "true"}
	provider := branchProvider("platform", "shared-repo")
	provider.Spec.AllowedGitBranches = []configbutleraiv1alpha3.GitProviderGitBranchGrant{
		{
			ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
				From:     configbutleraiv1alpha3.GrantFromSelector,
				Selector: &metav1.LabelSelector{MatchLabels: tenant},
			},
			BranchPatterns: []configbutleraiv1alpha3.BranchPattern{"teams/{consumer.namespace}/*", "main"},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: tenant}}
	f := newGitBranchFixture(t, provider, namespace, tenantBranch("team-a", time.Hour))
	got := f.reconcile(t, "team-a", "main")
	require.Equal(t, metav1.ConditionTrue, readyOf(got).Status, readyOf(got).Message)

	stored := &corev1.Namespace{}
	require.NoError(t, f.client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(namespace), stored))
	stored.Labels = nil
	require.NoError(t, f.client.Update(context.Background(), stored))
	got = f.reconcile(t, "team-a", "main")

	assert.Equal(t, GitBranchReasonNamespaceNotAuthorized, readyOf(got).Reason)
	_, held := f.holder()
	assert.False(t, held)
}
