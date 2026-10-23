// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// destinationFixture is a GitTarget team-a/apps on the GitBranch platform/main, which team-a may
// reference through the GitProvider platform/acme's accessFrom, with a started worker manager and a
// router with no watch plane.
type destinationFixture struct {
	r       *GitTargetReconciler
	client  ctrlclient.Client
	workers *git.WorkerManager
	branch  *configbutleraiv1alpha3.GitBranch
}

func newDestinationFixture(t *testing.T) *destinationFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	provider := branchProvider("platform", "acme")
	provider.Spec.AccessFrom = &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}}
	branch := branchObject("platform", "main", "acme", "main", time.Hour)
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "apps"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: claimProviderRef(claimLocalProvider),
			GitBranchRef:    meta.NamespacedObjectReference{Name: "main", Namespace: "platform"},
			Path:            "apps",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(provider, branch, target, admitting(claimLocalProvider, "team-a")).
		WithStatusSubresource(&configbutleraiv1alpha3.GitTarget{}).Build()
	workers := git.NewWorkerManager(c, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = workers.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	return &destinationFixture{
		r: &GitTargetReconciler{
			Client: c, WorkerManager: workers, EventRouter: watch.NewEventRouter(workers, nil, c, logr.Discard()),
		},
		client: c, workers: workers, branch: branch,
	}
}

func (f *destinationFixture) reconcile(t *testing.T) (ctrl.Result, *configbutleraiv1alpha3.GitTarget) {
	t.Helper()
	key := k8stypes.NamespacedName{Namespace: "team-a", Name: "apps"}
	result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.client.Get(context.Background(), key, got))
	return result, got
}

// TestReconcile_AGitBranchThatHoldsNothingKeepsTheTargetWaiting: until its GitBranch holds the
// branch, a GitTarget gets no worker and reports GitBranchNotReady as progress, coming back on the
// not-Ready cadence.
func TestReconcile_AGitBranchThatHoldsNothingKeepsTheTargetWaiting(t *testing.T) {
	f := newDestinationFixture(t)

	result, got := f.reconcile(t)

	ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, GitTargetReasonGitBranchNotReady, ready.Reason)
	assert.False(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
	assert.Equal(t, RequeueStreamSettleInterval, result.RequeueAfter)
	_, wired := f.workers.WorkerForTarget(got)
	assert.False(t, wired)
}

// TestReconcile_ATargetOnAHeldGitBranchInAnotherNamespaceIsWired: the GitBranch holds the branch and
// its GitProvider admits the target's namespace, so the target is wired to the holder's worker with
// that provider's identity.
func TestReconcile_ATargetOnAHeldGitBranchInAnotherNamespaceIsWired(t *testing.T) {
	f := newDestinationFixture(t)
	f.workers.Claims().Observe(git.BranchKeyFor(gitBranchTestURL, "main"), git.BranchClaimant{
		GitBranch: ctrlclient.ObjectKeyFromObject(
			f.branch,
		),
		UID:     f.branch.UID,
		Created: f.branch.CreationTimestamp.Time,
	}, true)

	_, got := f.reconcile(t)

	branchReady := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionGitBranchReady)
	require.NotNil(t, branchReady)
	assert.Equal(t, metav1.ConditionTrue, branchReady.Status, branchReady.Message)
	worker, wired := f.workers.WorkerForTarget(got)
	require.True(t, wired)
	assert.Equal(t, "platform", worker.GitProviderNamespace)
	assert.Equal(t, "acme", worker.GitProviderRef)
	assert.Equal(t, "main", worker.Branch)
}

// TestReconcile_AGitBranchWhoseProviderDoesNotAdmitTheNamespace: the cross-namespace reference is
// decided by the branch's GitProvider, against the TARGET's namespace.
func TestReconcile_AGitBranchWhoseProviderDoesNotAdmitTheNamespace(t *testing.T) {
	f := newDestinationFixture(t)
	provider := &configbutleraiv1alpha3.GitProvider{}
	require.NoError(t, f.client.Get(context.Background(), k8stypes.NamespacedName{Namespace: "platform", Name: "acme"},
		provider))
	provider.Spec.AccessFrom = nil
	require.NoError(t, f.client.Update(context.Background(), provider))

	_, got := f.reconcile(t)

	validated := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionValidated)
	require.NotNil(t, validated)
	assert.Equal(t, GitTargetReasonNamespaceNotAuthorized, validated.Reason)
	assert.Contains(t, validated.Message, "GitBranch platform/main")
}

// TestGitBranchWatches_MapToWhatTheyChange: a GitProvider event reaches the GitBranches in its
// namespace that name it; a GitTarget event reaches the GitBranch it references, from any namespace.
func TestGitBranchWatches_MapToWhatTheyChange(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		branchObject("platform", "main", "acme", "main", time.Hour),
		branchObject("platform", "release", "acme", "release", time.Hour),
		branchObject("platform", "other", "mirror", "main", time.Hour),
		branchObject("team-b", "main", "acme", "main", time.Hour),
	).Build()
	r := &GitBranchReconciler{Client: c}

	assert.Equal(t, []string{"platform/main", "platform/release"},
		requestsToKeys(r.gitProviderToGitBranches(context.Background(), branchProvider("platform", "acme"))))

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "apps"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: testKubeProviderRef(),
			GitBranchRef:    meta.NamespacedObjectReference{Name: "main", Namespace: "platform"},
		},
	}
	assert.Equal(t, []string{"platform/main"}, requestsToKeys(gitTargetToGitBranch(context.Background(), target)))
	target.Spec.GitBranchRef = meta.NamespacedObjectReference{}
	assert.Empty(t, gitTargetToGitBranch(context.Background(), target))
	assert.Empty(t, gitTargetToGitBranch(context.Background(), branchProvider("platform", "acme")))
}

// TestGitBranchReadyOrSpecChanged: a GitTarget is woken when its GitBranch appears, goes, or its
// Ready changes; a status.remote write alone, which moves on every push, is filtered.
func TestGitBranchReadyOrSpecChanged(t *testing.T) {
	p := gitBranchReadyOrSpecChanged()
	ready := func(status metav1.ConditionStatus, reason string) *configbutleraiv1alpha3.GitBranch {
		b := branchObject("platform", "main", "acme", "main", time.Hour)
		b.Status.Conditions = []metav1.Condition{{Type: ConditionTypeReady, Status: status, Reason: reason}}
		return b
	}
	held := ready(metav1.ConditionTrue, ReasonSucceeded)
	pushed := held.DeepCopy()
	pushed.Status.Remote = &configbutleraiv1alpha3.GitBranchRemoteStatus{Commit: "abc"}

	assert.True(t, p.Create(event.CreateEvent{Object: held}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: held}))
	assert.False(t, p.Generic(event.GenericEvent{Object: held}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: held, ObjectNew: pushed}), "status.remote alone")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: held,
		ObjectNew: ready(metav1.ConditionFalse, GitBranchReasonPushSessionFailed)}), "Ready went False")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ready(metav1.ConditionFalse, GitBranchReasonPushSessionFailed),
		ObjectNew: ready(metav1.ConditionFalse, GitBranchReasonBranchConflict)}), "the reason changed")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: held, ObjectNew: branchProvider("platform", "acme")}),
		"an object it cannot read is let through")
}

// TestGitTargetsOn_FindsTheTargetsOfABranchOrItsProvider is the shared mapping behind the GitTarget
// and applier-claim watches on GitBranch and GitProvider.
func TestGitTargetsOn_FindsTheTargetsOfABranchOrItsProvider(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	target := func(namespace, name, branchNS, branch string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef:    meta.NamespacedObjectReference{Name: branch, Namespace: branchNS},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		branchObject("platform", "main", "acme", "main", time.Hour),
		target("platform", "infra", "", "main"), target("team-a", "apps", "platform", "main"),
		target("team-a", "own", "", "main"),
	).Build()

	byBranch, err := gitTargetsOn(context.Background(), c, branchObject("platform", "main", "acme", "main", 0))
	require.NoError(t, err)
	assert.Len(t, byBranch, 2)
	byProvider, err := gitTargetsOn(context.Background(), c, branchProvider("platform", "acme"))
	require.NoError(t, err)
	assert.Len(t, byProvider, 2)
	none, err := gitTargetsOn(context.Background(), c, branchProvider("platform", "unused"))
	require.NoError(t, err)
	assert.Empty(t, none)
}
