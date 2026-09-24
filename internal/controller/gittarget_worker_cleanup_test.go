// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

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
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestCleanupDeletedGitTarget_StopsAWorkerNothingNeedsAnyMore is the wiring, and the wiring is the
// whole bug: the sweep existed and was never called, so a branch worker — its goroutine, its event
// queue and its on-disk clone — outlived every GitTarget that justified it, until the process
// restarted. The reconcile that the delete watch event produces is where the set of needed workers
// can have shrunk, so that is where the sweep belongs.
func TestCleanupDeletedGitTarget_StopsAWorkerNothingNeedsAnyMore(t *testing.T) {
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

	require.NoError(t, workers.EnsureWorker(ctx, "repo1", "shop", "main"))
	_, exists := workers.GetWorkerForTarget("repo1", "shop", "main")
	require.True(t, exists)

	r := &GitTargetReconciler{Client: k8sClient, WorkerManager: workers}
	r.cleanupDeletedGitTarget(ctx,
		k8stypes.NamespacedName{Name: "checkout", Namespace: "shop"}, logr.Discard())

	_, exists = workers.GetWorkerForTarget("repo1", "shop", "main")
	assert.False(t, exists, "the GitTarget is gone and nothing else lists this branch")
}

// TestCleanupDeletedGitTarget_LeavesAWorkerAnotherGitTargetStillUses. The sweep reads the API
// rather than counting registrations, and this is why: one worker serves every GitTarget on its
// (provider, branch), so one deletion says nothing about the rest.
func TestCleanupDeletedGitTarget_LeavesAWorkerAnotherGitTargetStillUses(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	sibling := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "invoices", Namespace: "shop"},
	}
	sibling.Spec.GitProviderRef.Name = "repo1"
	sibling.Spec.Branch = "main"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sibling).Build()

	workers := git.NewWorkerManager(k8sClient, logr.Discard(), git.BranchWorkerLimits{},
		types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = workers.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, workers.EnsureWorker(ctx, "repo1", "shop", "main"))

	r := &GitTargetReconciler{Client: k8sClient, WorkerManager: workers}
	r.cleanupDeletedGitTarget(ctx,
		k8stypes.NamespacedName{Name: "checkout", Namespace: "shop"}, logr.Discard())

	_, exists := workers.GetWorkerForTarget("repo1", "shop", "main")
	assert.True(t, exists, "invoices still writes to this branch")
}

// TestCleanupDeletedGitTarget_WithoutAWorkerManager covers the CLI and the tests: the cleanup runs
// on a reconciler with no data plane wired, and must not reach for one.
func TestCleanupDeletedGitTarget_WithoutAWorkerManager(t *testing.T) {
	r := &GitTargetReconciler{}
	assert.NotPanics(t, func() {
		r.cleanupDeletedGitTarget(context.Background(),
			k8stypes.NamespacedName{Name: "checkout", Namespace: "shop"}, logr.Discard())
	})
}
