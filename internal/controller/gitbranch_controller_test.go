// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const gitBranchTestURL = "https://github.com/acme/platform.git"

var gitBranchEpoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// branchProvider is a GitProvider in namespace for the test repository, allowing branches, Ready
// unless told otherwise. It has no secretRef, so its credential resolves to anonymous and the
// fixture's prover decides what that credential may do.
func branchProvider(namespace, name string, allowed ...string) *configbutleraiv1alpha3.GitProvider {
	if len(allowed) == 0 {
		allowed = []string{"*"}
	}
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: k8stypes.UID("uid-" + name)},
		Spec:       configbutleraiv1alpha3.GitProviderSpec{URL: gitBranchTestURL, AllowedBranches: allowed},
		Status: configbutleraiv1alpha3.GitProviderStatus{Conditions: []metav1.Condition{{
			Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
			LastTransitionTime: metav1.Now(),
		}}},
	}
}

// branchObject is a GitBranch namespace/name writing branch through provider, created age ago.
func branchObject(namespace, name, provider, branch string, age time.Duration) *configbutleraiv1alpha3.GitBranch {
	return &configbutleraiv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: k8stypes.UID("uid-" + namespace + "-" + name),
			CreationTimestamp: metav1.NewTime(gitBranchEpoch.Add(-age)),
		},
		Spec: configbutleraiv1alpha3.GitBranchSpec{
			GitProviderRef: meta.LocalObjectReference{Name: provider}, Branch: branch,
		},
	}
}

// gitBranchFixture runs the GitBranch reconciler against a fake API and a started worker manager.
// Every credential proves itself unless a test sets r.proveProvider.
type gitBranchFixture struct {
	r       *GitBranchReconciler
	client  ctrlclient.Client
	workers *git.WorkerManager
}

func newGitBranchFixture(t *testing.T, objects ...ctrlclient.Object) *gitBranchFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.GitBranch{}, &configbutleraiv1alpha3.GitProvider{}).Build()
	workers := git.NewWorkerManager(c, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = workers.Start(ctx) }()
	time.Sleep(50 * time.Millisecond) // Start installs the context workers are started under

	f := &gitBranchFixture{client: c, workers: workers}
	f.r = &GitBranchReconciler{Client: c, WorkerManager: workers}
	f.r.proveProvider = func(*configbutleraiv1alpha3.GitProvider) error { return nil }
	return f
}

// reconcile runs one reconcile of namespace/name and returns the GitBranch as stored.
func (f *gitBranchFixture) reconcile(t *testing.T, namespace, name string) *configbutleraiv1alpha3.GitBranch {
	t.Helper()
	key := k8stypes.NamespacedName{Namespace: namespace, Name: name}
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &configbutleraiv1alpha3.GitBranch{}
	require.NoError(t, f.client.Get(context.Background(), key, got))
	return got
}

func readyOf(b *configbutleraiv1alpha3.GitBranch) *metav1.Condition {
	return findCondition(b.Status.Conditions, ConditionTypeReady)
}

// TestGitBranch_AProvenGitBranchHoldsItsBranch: a GitBranch whose credential opens a push session
// holds the repository branch, is Ready, carries its finalizer, publishes the normalized repository
// and counts the GitTargets that reference it from any namespace.
func TestGitBranch_AProvenGitBranchHoldsItsBranch(t *testing.T) {
	target := func(namespace, name string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef:    meta.NamespacedObjectReference{Name: "main", Namespace: "platform"}, Path: name,
			},
		}
	}
	f := newGitBranchFixture(t,
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", time.Hour),
		target("platform", "infra"), target("team-a", "apps"),
		&configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "other"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef:    meta.NamespacedObjectReference{Name: "main"}, Path: "other",
			},
		})

	got := f.reconcile(t, "platform", "main")

	ready := readyOf(got)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, ready.Message)
	assert.Contains(t, got.Finalizers, GitBranchFinalizer)
	assert.Equal(t, "github.com/acme/platform", got.Status.Repository)
	assert.Equal(t, int32(2), got.Status.GitTargetCount, "team-a/other references team-a/main, another GitBranch")
	key, _, held := f.workers.Claims().HeldBy(ctrlclient.ObjectKeyFromObject(got))
	require.True(t, held)
	assert.Equal(t, git.BranchKeyFor(gitBranchTestURL, "main"), key)
}

// TestGitBranch_ABadCredentialCreatedFirstBlocksNobody is the plan's first "done when" about the
// claim: a GitBranch with a bad credential, created first, does not stop a valid one created later
// from claiming the branch, and it reports which GitBranch holds it.
func TestGitBranch_ABadCredentialCreatedFirstBlocksNobody(t *testing.T) {
	f := newGitBranchFixture(t,
		branchProvider("squat", "acme"), branchObject("squat", "main", "acme", "main", 2*time.Hour),
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", time.Hour))
	f.r.proveProvider = func(provider *configbutleraiv1alpha3.GitProvider) error {
		if provider.Namespace == "squat" {
			return errors.New("authentication required")
		}
		return nil
	}

	squat := f.reconcile(t, "squat", "main")
	assert.Equal(t, GitBranchReasonPushSessionFailed, readyOf(squat).Reason)
	assert.Contains(t, readyOf(squat).Message, "authentication required")

	holder := f.reconcile(t, "platform", "main")
	assert.Equal(t, metav1.ConditionTrue, readyOf(holder).Status, readyOf(holder).Message)

	squat = f.reconcile(t, "squat", "main")
	assert.Equal(t, GitBranchReasonBranchConflict, readyOf(squat).Reason)
	assert.Contains(t, readyOf(squat).Message, "platform/main")
	assert.True(t, conditionIsTrue(squat.Status.Conditions, ConditionTypeStalled))
}

// TestGitBranch_TwoValidGitBranchesForOneBranchRunOneWorker is the create race: both pass admission
// (nothing refuses there), both prove themselves, and the younger reports BranchConflict, also when
// it names another GitProvider for the same repository.
func TestGitBranch_TwoValidGitBranchesForOneBranchRunOneWorker(t *testing.T) {
	f := newGitBranchFixture(t,
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", 2*time.Hour),
		branchProvider("team-b", "mirror"), branchObject("team-b", "main", "mirror", "main", time.Hour))

	younger := f.reconcile(t, "team-b", "main")
	assert.Equal(t, metav1.ConditionTrue, readyOf(younger).Status, "alone, it holds the branch")
	older := f.reconcile(t, "platform", "main")
	assert.Equal(t, metav1.ConditionTrue, readyOf(older).Status, readyOf(older).Message)

	younger = f.reconcile(t, "team-b", "main")
	assert.Equal(t, GitBranchReasonBranchConflict, readyOf(younger).Reason)
	assert.Contains(t, readyOf(younger).Message, "platform/main")
	holder, held := f.workers.Claims().Holder(git.BranchKeyFor(gitBranchTestURL, "main"))
	require.True(t, held)
	assert.Equal(t, "platform", holder.GitBranch.Namespace)
}

// TestGitBranch_TheHolderKeepsItsBranchWhenItsCredentialFails is rule 3: Ready goes False with the
// failure, the claim stays, and a younger valid GitBranch does not take it over.
func TestGitBranch_TheHolderKeepsItsBranchWhenItsCredentialFails(t *testing.T) {
	f := newGitBranchFixture(t,
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", 2*time.Hour),
		branchProvider("team-b", "mirror"), branchObject("team-b", "main", "mirror", "main", time.Hour))
	f.reconcile(t, "platform", "main")
	f.r.proveProvider = func(provider *configbutleraiv1alpha3.GitProvider) error {
		if provider.Namespace == "platform" {
			return errors.New("authentication required")
		}
		return nil
	}
	f.r.proofs = nil // the next reconcile proves again

	holder := f.reconcile(t, "platform", "main")
	assert.Equal(t, GitBranchReasonPushSessionFailed, readyOf(holder).Reason)
	assert.Contains(t, readyOf(holder).Message, "keeps the branch")
	assert.False(t, conditionIsTrue(holder.Status.Conditions, ConditionTypeStalled), "it recovers on its own")

	younger := f.reconcile(t, "team-b", "main")
	assert.Equal(t, GitBranchReasonBranchConflict, readyOf(younger).Reason)
}

// TestGitBranch_TheBranchMustBeAllowed: allowedBranches globs moved here from the GitTarget.
func TestGitBranch_TheBranchMustBeAllowed(t *testing.T) {
	for branch, allowed := range map[string]bool{
		"main": true, "develop": true, "feature/login": true, "release/v1.0": true,
		"hotfix/urgent": false, "staging": false,
	} {
		t.Run(branch, func(t *testing.T) {
			f := newGitBranchFixture(t,
				branchProvider("platform", "acme", "main", "develop", "feature/*", "release/v*"),
				branchObject("platform", "b", "acme", branch, time.Hour))

			got := f.reconcile(t, "platform", "b")

			if allowed {
				assert.Equal(t, metav1.ConditionTrue, readyOf(got).Status, readyOf(got).Message)
				return
			}
			assert.Equal(t, GitBranchReasonBranchNotAllowed, readyOf(got).Reason)
			assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
			_, _, held := f.workers.Claims().HeldBy(ctrlclient.ObjectKeyFromObject(got))
			assert.False(t, held, "a branch outside allowedBranches is never claimed")
		})
	}
}

// TestGitBranch_AMissingOrUnreadyGitProvider: a missing GitProvider stalls the GitBranch; one that
// is not Ready keeps it progressing.
func TestGitBranch_AMissingOrUnreadyGitProvider(t *testing.T) {
	f := newGitBranchFixture(t, branchObject("platform", "main", "acme", "main", time.Hour))
	got := f.reconcile(t, "platform", "main")
	assert.Equal(t, GitBranchReasonGitProviderNotFound, readyOf(got).Reason)
	assert.Contains(t, readyOf(got).Message, "platform/acme")

	unready := branchProvider("platform", "acme")
	unready.Status.Conditions[0].Status = metav1.ConditionFalse
	f = newGitBranchFixture(t, unready, branchObject("platform", "main", "acme", "main", time.Hour))
	got = f.reconcile(t, "platform", "main")
	assert.Equal(t, GitBranchReasonGitProviderNotReady, readyOf(got).Reason)
	assert.False(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
}

// TestGitBranch_DeletionStopsTheWorkerAndReleasesTheBranch: the finalizer stops the branch worker
// (which publishes or refuses its pending writes and settles its CommitRequests), releases the
// claim, and goes. The next candidate then takes the branch.
func TestGitBranch_DeletionStopsTheWorkerAndReleasesTheBranch(t *testing.T) {
	f := newGitBranchFixture(t,
		branchProvider("platform", "acme"), branchObject("platform", "main", "acme", "main", 2*time.Hour),
		branchProvider("team-b", "mirror"), branchObject("team-b", "main", "mirror", "main", time.Hour))
	f.reconcile(t, "platform", "main")
	f.reconcile(t, "team-b", "main")
	holder := k8stypes.NamespacedName{Namespace: "platform", Name: "main"}
	require.NoError(t, f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
		GitBranch: holder, Provider: "acme", Branch: "main",
		Repo: git.RepoIdentity{ProviderUID: "uid-acme", URL: gitBranchTestURL},
	}))

	stored := &configbutleraiv1alpha3.GitBranch{}
	require.NoError(t, f.client.Get(context.Background(), holder, stored))
	require.NoError(t, f.client.Delete(context.Background(), stored))
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: holder})
	require.NoError(t, err)

	_, live := f.workers.WorkerForGitBranch(holder)
	assert.False(t, live, "the worker stopped before the finalizer went")
	require.Error(t, f.client.Get(context.Background(), holder, stored), "the finalizer is gone, and so is the object")

	require.Len(t, f.workers.GitBranchEvents(), 1, "the next candidate is woken, not left stalled")
	woken := (<-f.workers.GitBranchEvents()).Object
	assert.Equal(t, "team-b/main", woken.GetNamespace()+"/"+woken.GetName())
	next := f.reconcile(t, "team-b", "main")
	assert.Equal(t, metav1.ConditionTrue, readyOf(next).Status, readyOf(next).Message)
}

// TestGitBranch_AMissingParentStallsTheBranch: ParentBranchNotFound moved from the GitTarget with its
// rules unchanged. A fetch that found neither the branch nor its configured parent stalls the
// GitBranch, for that parent only.
func TestGitBranch_AMissingParentStallsTheBranch(t *testing.T) {
	branch := branchObject("platform", "edits", "acme", "edits", time.Hour)
	branch.Spec.ParentBranch = "release"
	f := newGitBranchFixture(t, branchProvider("platform", "acme"), branch)
	got := f.reconcile(t, "platform", "edits")
	require.Equal(t, metav1.ConditionTrue, readyOf(got).Status, "nothing has looked yet")

	key := ctrlclient.ObjectKeyFromObject(branch)
	require.NoError(t, f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
		GitBranch: key, Provider: "acme", Branch: "edits", ParentBranch: "release",
		Repo: git.RepoIdentity{ProviderUID: "uid-acme", URL: gitBranchTestURL},
	}))
	worker, _ := f.workers.WorkerForGitBranch(key)
	worker.RecordMissingParentForTest("release")

	got = f.reconcile(t, "platform", "edits")
	assert.Equal(t, GitBranchReasonParentBranchNotFound, readyOf(got).Reason)
	assert.Contains(t, readyOf(got).Message, "'release'")
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled), "somebody has to create a branch")
	require.NotNil(t, got.Status.Remote)
	require.NotNil(t, got.Status.Remote.Parent)
	assert.Equal(t, configbutleraiv1alpha3.GitBranchParentMissing, got.Status.Remote.Parent.State)

	worker.RecordMissingParentForTest("other")
	got = f.reconcile(t, "platform", "edits")
	assert.NotEqual(t, GitBranchReasonParentBranchNotFound, readyOf(got).Reason,
		"an observation made under another parent says nothing about this one")
}

// TestGitBranch_AnUnresolvedDefaultBranchStallsToo: with spec.parentBranch omitted, a remote whose
// HEAD names a branch it does not carry stalls the GitBranch, with a message that says what to fix.
func TestGitBranch_AnUnresolvedDefaultBranchStallsToo(t *testing.T) {
	f := newGitBranchFixture(t, branchProvider("platform", "acme"),
		branchObject("platform", "edits", "acme", "edits", time.Hour))
	f.reconcile(t, "platform", "edits")
	key := k8stypes.NamespacedName{Namespace: "platform", Name: "edits"}
	require.NoError(t, f.workers.EnsureWorker(context.Background(), git.WorkerRequest{
		GitBranch: key, Provider: "acme", Branch: "edits",
		Repo: git.RepoIdentity{ProviderUID: "uid-acme", URL: gitBranchTestURL},
	}))
	worker, _ := f.workers.WorkerForGitBranch(key)

	worker.RecordUnresolvedDefaultBranchForTest("master")
	got := f.reconcile(t, "platform", "edits")
	assert.Equal(t, GitBranchReasonParentBranchNotFound, readyOf(got).Reason)
	assert.Contains(t, readyOf(got).Message, "HEAD -> 'master'")
	assert.Contains(t, readyOf(got).Message, "spec.parentBranch")
}

// TestGitBranch_ARepointedProviderMovesTheClaim: a GitProvider recreated against another repository
// is another key, so the GitBranch's claim moves with it.
func TestGitBranch_ARepointedProviderMovesTheClaim(t *testing.T) {
	provider := branchProvider("platform", "acme")
	f := newGitBranchFixture(t, provider, branchObject("platform", "main", "acme", "main", time.Hour))
	f.reconcile(t, "platform", "main")

	stored := &configbutleraiv1alpha3.GitProvider{}
	require.NoError(t, f.client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(provider), stored))
	require.NoError(t, f.client.Delete(context.Background(), stored))
	recreated := branchProvider("platform", "acme")
	recreated.UID = "uid-acme-2"
	recreated.ResourceVersion = ""
	recreated.Spec.URL = "https://github.com/acme/other.git"
	require.NoError(t, f.client.Create(context.Background(), recreated))

	got := f.reconcile(t, "platform", "main")
	assert.Equal(t, "github.com/acme/other", got.Status.Repository)
	_, held := f.workers.Claims().Holder(git.BranchKeyFor(gitBranchTestURL, "main"))
	assert.False(t, held, "the old repository's branch is free again")
	key, _, held := f.workers.Claims().HeldBy(ctrlclient.ObjectKeyFromObject(got))
	require.True(t, held)
	assert.Equal(t, "github.com/acme/other", key.Repository)
}
