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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
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

	first, err := r.ensureEventStream(target, "shop", before, logr.Discard())
	require.NoError(t, err)

	steady, err := r.ensureEventStream(target, "shop", before, logr.Discard())
	require.NoError(t, err)
	assert.Same(t, first, steady, "a steady tick must not re-register anything")

	repointed, err := r.ensureEventStream(target, "shop", after, logr.Discard())
	require.NoError(t, err)

	assert.NotSame(t, first, repointed, "the route has to reach the worker for the repository named NOW")
	worker, ok := workers.GetWorkerForTarget("repo1", "shop", "main")
	require.True(t, ok)
	assert.True(t, repointed.ServesWorker(worker))
	assert.Same(t, repointed, router.GetGitTargetEventStream(types.NewResourceReference("apps", "shop")),
		"and it is the registered stream, not a copy the reconcile kept to itself")
}
