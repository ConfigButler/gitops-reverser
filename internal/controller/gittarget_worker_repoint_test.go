// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// TestEnsureEventStream_RepointsTheStreamAtTheReplacementWorker.
//
// A GitTargetEventStream holds its branch worker directly, and the reconcile used to return the
// registered stream without looking at which worker that was. A repointed GitProvider replaces the
// worker, so a stream left alone would go on handing live events to a stopped goroutine's queue:
// accepted, never written, and silently dropped once the queue filled.
func TestEnsureEventStream_RepointsTheStreamAtTheReplacementWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	workers := git.NewWorkerManager(k8sClient, logr.Discard(), git.BranchWorkerLimits{},
		types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = workers.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	router := watch.NewEventRouter(workers, nil, k8sClient, logr.Discard())
	r := &GitTargetReconciler{Client: k8sClient, WorkerManager: workers, EventRouter: router}

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: "shop"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "repo1"},
			Branch:         "main",
			Path:           "apps",
		},
	}
	before := git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	after := git.RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}

	first, err := r.ensureEventStream(ctx, target, "shop", before, logr.Discard())
	require.NoError(t, err)

	steady, err := r.ensureEventStream(ctx, target, "shop", before, logr.Discard())
	require.NoError(t, err)
	assert.Same(t, first, steady, "a steady tick must not re-register anything")

	repointed, err := r.ensureEventStream(ctx, target, "shop", after, logr.Discard())
	require.NoError(t, err)

	assert.NotSame(t, first, repointed, "the route has to reach the worker for the repository named NOW")
	worker, ok := workers.GetWorkerForTarget("repo1", "shop", "main")
	require.True(t, ok)
	assert.True(t, repointed.ServesWorker(worker))
	assert.Same(t, repointed, router.GetGitTargetEventStream(types.NewResourceReference("apps", "shop")),
		"and it is the registered stream, not a copy the reconcile kept to itself")
}

// TestRecoverBranchAfterReplacement_ReachesEverySiblingOnTheBranch.
//
// A replacement throws away the old worker's queue and its retained writes, which is only safe
// because the folder is rebuilt from the cluster. Nothing asks for that rebuild on its own: the
// declaration is level-triggered, its inputs did not change, so the next pass is a no-op and an
// idle target would leave the new repository empty.
//
// One worker serves every GitTarget on its (provider, branch), and the siblings have no reason of
// their own to reconcile, so recovering only the target whose reconcile noticed leaves the rest
// pointing at a repository nothing will repopulate. Targets on another branch, and on another
// provider, must be left alone: forcing a replay is a full cluster snapshot per target.
func TestRecoverBranchAfterReplacement_ReachesEverySiblingOnTheBranch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	target := func(name, provider, branch string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: k8stypes.UID("uid-" + name)},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				GitProviderRef: meta.LocalObjectReference{Name: provider},
				Branch:         branch,
				Path:           name,
			},
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		target("apps", "repo1", "main"),
		target("infra", "repo1", "main"),  // sibling: same worker, must be recovered
		target("release", "repo1", "rel"), // same provider, another branch: untouched
		target("other", "repo2", "main"),  // another provider: untouched
	).Build()

	manager := &watch.Manager{}
	// Only a declared target can be re-anchored; an undeclared one replays on its first pass
	// anyway, so the recovery has nothing to ask of it.
	for _, name := range []string{"apps", "infra", "release", "other"} {
		manager.DeclareForGitTarget(
			types.NewResourceReference(name, "shop").WithUID("uid-"+name), "default", "", "")
	}
	workers := git.NewWorkerManager(k8sClient, logr.Discard(), git.BranchWorkerLimits{},
		types.SensitiveResourcePolicy{})
	r := &GitTargetReconciler{
		Client:        k8sClient,
		WorkerManager: workers,
		EventRouter:   watch.NewEventRouter(workers, manager, k8sClient, logr.Discard()),
	}
	// A verdict proved against the repository that is about to stop being the destination.
	appsRef := types.NewResourceReference("apps", "shop")
	workers.RenderFidelityGate().Fail(appsRef, manifestanalyzer.RenderDivergence{})
	require.False(t, workers.RenderFidelityGate().AllowsWrites(appsRef))

	r.recoverBranchAfterReplacement(context.Background(), "shop", "repo1", "main", logr.Discard())

	assert.ElementsMatch(t, []string{"shop/apps", "shop/infra"}, manager.ForcedRecheckTargetsForTest(),
		"every GitTarget the replaced worker served, and nothing else")
	assert.True(t, workers.RenderFidelityGate().AllowsWrites(appsRef),
		"a verdict about the other repository must not survive into this one")
}
