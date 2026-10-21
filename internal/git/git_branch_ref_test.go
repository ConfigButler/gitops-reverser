// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func refTarget(namespace, gitBranch string) *configv1alpha3.GitTarget {
	target := &configv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "apps"}}
	if gitBranch != "" {
		target.Spec.GitBranchRef = &meta.NamespacedObjectReference{Name: gitBranch, Namespace: namespace}
	}
	return target
}

// TestResolveGitBranch_FollowsTheTargetToItsBranchAndProvider: a GitTarget's reference resolves to
// its GitBranch and that GitBranch's GitProvider, in the GitBranch's namespace. Anything missing is
// nil without an error: it is a status, not a failure to retry.
func TestResolveGitBranch_FollowsTheTargetToItsBranchAndProvider(t *testing.T) {
	branch := &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "main"},
		Spec: configv1alpha3.GitBranchSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "acme"}, Branch: "main",
		},
	}
	orphan := &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "orphan"},
		Spec:       configv1alpha3.GitBranchSpec{GitProviderRef: meta.LocalObjectReference{Name: "gone"}, Branch: "x"},
	}
	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "acme"},
		Spec:       configv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
	}
	reader := fake.NewClientBuilder().WithScheme(setupScheme()).WithObjects(branch, orphan, provider).Build()
	ctx := context.Background()

	gotBranch, gotProvider, err := ResolveGitBranch(ctx, reader, refTarget("platform", "main"))
	require.NoError(t, err)
	assert.Equal(t, "main", gotBranch.Spec.Branch)
	assert.Equal(t, provider.Spec.URL, gotProvider.Spec.URL)

	gotBranch, gotProvider, err = ResolveGitBranch(ctx, reader, refTarget("platform", "orphan"))
	require.NoError(t, err)
	assert.NotNil(t, gotBranch)
	assert.Nil(t, gotProvider, "a GitBranch whose GitProvider is gone")

	gotBranch, _, err = ResolveGitBranch(ctx, reader, refTarget("", "main"))
	require.NoError(t, err)
	assert.Nil(t, gotBranch, "no namespace means the target's own, where there is no such GitBranch")

	gotBranch, _, err = ResolveGitBranch(ctx, reader, refTarget("", ""))
	require.NoError(t, err)
	assert.Nil(t, gotBranch, "a target with no reference resolves to nothing")
}

// TestAuthForProvider_AnAnonymousProviderSendsNothing: a GitProvider with no secretRef proves
// itself, and pushes, with no credential at all.
func TestAuthForProvider_AnAnonymousProviderSendsNothing(t *testing.T) {
	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "acme"},
		Spec:       configv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
	}
	k8s := fake.NewClientBuilder().WithScheme(setupScheme()).Build()

	auth, err := AuthForProvider(context.Background(), k8s, provider, SSHHostKeyConfig{}, CredentialTransportPolicy{})
	require.NoError(t, err)
	assert.Empty(t, auth)
}

// TestWorkerForTarget_AndNotifyPushFindTheHoldersWorker: a GitTarget reaches its worker through its
// gitBranchRef, and a push notification, which names a GitProvider and a branch, reaches the worker
// of the GitBranch writing them.
func TestWorkerForTarget_AndNotifyPushFindTheHoldersWorker(t *testing.T) {
	m, ctx := startedManager(t)
	request := claimForTest(m, "acme", "team-a", "main", RepoIdentity{URL: ownerTestURL})
	require.NoError(t, m.EnsureWorker(ctx, request))

	worker, found := m.WorkerForTarget(refTarget("", testGitBranchName("acme", "main")))
	require.True(t, found)
	assert.Equal(t, request.GitBranch, worker.GitBranch)
	_, found = m.WorkerForTarget(refTarget("", "other"))
	assert.False(t, found)

	assert.True(t, m.NotifyPush("team-a", "acme", "main", "abc"))
	assert.False(t, m.NotifyPush("team-a", "acme", "release", "abc"), "no worker writes that branch")
	assert.False(t, m.NotifyPush("team-b", "acme", "main", "abc"), "nor through that GitProvider")
}

// TestRecordRemoteObservation_WakesTheGitBranch: what the worker proves reaches the GitBranch's
// reconcile at once, so status.remote follows a push within one reconcile.
func TestRecordRemoteObservation_WakesTheGitBranch(t *testing.T) {
	m := NewWorkerManager(nil, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	key := BranchKeyFor(ownerTestURL, "main")
	gitBranch := testGitBranch("acme", "team-a", "main")
	m.workers[key] = &BranchWorker{GitBranch: gitBranch, Branch: "main"}

	m.recordRemoteObservation(key, RemoteObservation{Commit: "abc"})

	require.Len(t, m.GitBranchEvents(), 1)
	woken := (<-m.GitBranchEvents()).Object
	assert.Equal(t, gitBranch, client.ObjectKeyFromObject(woken))
}
