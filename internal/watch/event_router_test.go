/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// saturatedWorkerManager starts a WorkerManager, ensures a worker for (provider, ns, branch), stops
// its drain loop, and fills its FIFO to capacity — so the NEXT resync the worker receives is
// dropped. The worker stays registered (GetWorkerForTarget finds it). This drives the queue-full
// path deterministically without volume or timing guesses.
func saturatedWorkerManager(
	t *testing.T, c client.Client, providerName, providerNamespace, branch string,
) *git.WorkerManager {
	t.Helper()
	wm := git.NewWorkerManager(c, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = wm.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // allow the manager to record its context (matches the WM tests)
	require.NoError(t, wm.EnsureWorker(ctx, providerName, providerNamespace, branch))

	worker, ok := wm.GetWorkerForTarget(providerName, providerNamespace, branch)
	require.True(t, ok)
	worker.Stop() // halt the drain loop so the queue can be saturated deterministically
	for worker.EnqueueResync(&git.ResyncRequest{Result: make(chan git.ResyncResult, 1)}) {
	}
	return wm
}

func eventRouterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha2.AddToScheme(scheme))
	return scheme
}

func saveAttach(gitTargetName, gitTargetNamespace string) git.AttachCommitRequest {
	return git.AttachCommitRequest{
		Namespace:          gitTargetNamespace,
		Name:               "save",
		UID:                "uid-save",
		Author:             "alice",
		GitTargetName:      gitTargetName,
		GitTargetNamespace: gitTargetNamespace,
	}
}

func TestServiceCommitRequest_GitTargetNotFound(t *testing.T) {
	scheme := eventRouterScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	_, resolved, err := router.ServiceCommitRequest(context.Background(), saveAttach("missing", "team-a"))
	require.Error(t, err)
	assert.False(t, resolved)
	assert.Contains(t, err.Error(), "get GitTarget")
}

func TestServiceCommitRequest_NoWorkerResolvesNoOpenWindow(t *testing.T) {
	scheme := eventRouterScheme(t)
	gitTarget := &configv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha2.GitTargetSpec{
			ProviderRef: configv1alpha2.GitProviderReference{Name: "team-a-provider"},
			Branch:      "main",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitTarget).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	result, resolved, err := router.ServiceCommitRequest(context.Background(), saveAttach("team-a-config", "team-a"))
	require.NoError(t, err)
	assert.True(t, resolved, "no worker means no window to collect into; resolve immediately")
	assert.Equal(t, git.FinalizeNoOpenWindow, result.Outcome)
	assert.Equal(t, "main", result.Branch)
}

func TestServiceCommitRequest_RegisteredWorkerResolvesNoOpenWindow(t *testing.T) {
	scheme := eventRouterScheme(t)
	provider := &configv1alpha2.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-provider", Namespace: "team-a"},
		Spec:       configv1alpha2.GitProviderSpec{URL: "file:///tmp/does-not-need-to-exist"},
	}
	gitTarget := &configv1alpha2.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "team-a"},
		Spec: configv1alpha2.GitTargetSpec{
			ProviderRef: configv1alpha2.GitProviderReference{Name: "team-a-provider"},
			Branch:      "main",
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, gitTarget).Build()
	workerManager := git.NewWorkerManager(client, logr.Discard(), 0, types.SensitiveResourcePolicy{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = workerManager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // allow the manager to record its context

	require.NoError(t, workerManager.EnsureWorker(ctx, "team-a-provider", "team-a", "main"))

	router := NewEventRouter(workerManager, nil, client, logr.Discard())

	// No events routed and delaySeconds 0, so the worker has no window: the attach
	// is enqueued, processed by the worker loop, and resolves NoOpenWindow. The
	// controller polls via repeated (idempotent) ServiceCommitRequest calls.
	attach := saveAttach("team-a-config", "team-a")
	var result git.FinalizeResult
	require.Eventually(t, func() bool {
		var resolved bool
		var err error
		result, resolved, err = router.ServiceCommitRequest(context.Background(), attach)
		require.NoError(t, err)
		return resolved
	}, 5*time.Second, 50*time.Millisecond, "the worker must resolve the attached request")
	assert.Equal(t, git.FinalizeNoOpenWindow, result.Outcome)
	assert.Equal(t, "main", result.Branch)
}

// TestEmitTypeReconcileForGitDest_DroppedResyncSurfacesError is the P1 regression guard: when the
// worker's FIFO is full and the scoped reconcile is dropped, EmitTypeReconcileForGitDest must return
// an error (wrapping ErrFinalizeQueueFull) — not nil — so the initial-backfill caller forgets the
// type and retries on the next reconcile instead of starting the freshness tail over a baseline that
// never landed. It must also NOT publish the coverage watermark, since no reconcile is queued
// through Hc. See signing-snapshot-tail-replay-failure-investigation.md §7.4.
func TestEmitTypeReconcileForGitDest_DroppedResyncSurfacesError(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store) // my-target/gitops-reverser watches configmaps
	m := streamingManager(t, gitTargetFixture(), store)
	m.TypeSplicer = &fakeTypeSplicer{rv: "100", coverageHead: "100-0"}
	m.materializerInstance().RestoreSynced(configmapsGVR, "100") // the splice is serviceable (ready=true)

	// gitTargetFixture carries no provider/branch, so its worker key is {gitops-reverser, "", ""}.
	wm := saturatedWorkerManager(t, m.Client, "", "gitops-reverser", "")
	m.EventRouter = NewEventRouter(wm, m, m.Client, logr.Discard())

	err := m.EventRouter.EmitTypeReconcileForGitDest(context.Background(), myTargetRef(), configmapsGVR, false)
	require.ErrorIs(t, err, git.ErrFinalizeQueueFull,
		"a dropped scoped reconcile must surface as an error so the backfill caller retries and withholds the tail")

	_, published := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, published, "no watermark is published when the reconcile never entered the FIFO")
}

// TestDeclareForGitTarget_DroppedBackfillForgetsTypeAndWithholdsTail is the P1 chain end to end
// (reviewer item #1): when the initial-backfill reconcile is dropped by a full worker queue,
// DeclareForGitTarget must NOT start the freshness tail (no tail ahead of an un-backfilled baseline)
// and must forget the type so the NEXT Declare retries it — rather than recording it as done and
// relying on a heal. A non-nil AuditTailReader is wired so that a SUCCESSFUL backfill *would* start a
// tail, making "tail not started" a meaningful assertion.
func TestDeclareForGitTarget_DroppedBackfillForgetsTypeAndWithholdsTail(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	m.TypeSplicer = &fakeTypeSplicer{rv: "100", coverageHead: "100-0"}
	m.materializerInstance().RestoreSynced(configmapsGVR, "100")
	m.AuditTailReader = blockingTailReader{}
	m.auditTailBlockOverride = 40 * time.Millisecond

	wm := saturatedWorkerManager(t, m.Client, "", "gitops-reverser", "")
	m.EventRouter = NewEventRouter(wm, m, m.Client, logr.Discard())

	require.NoError(t, m.DeclareForGitTarget(context.Background(), myTargetRef()),
		"Declare returns nil: the claim lands; the dropped backfill is handled internally")

	assert.False(t, m.isAuditTailRunning(configmapsGVR),
		"a dropped initial backfill must NOT start the freshness tail over an un-backfilled baseline")
	_, published := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, published, "no watermark is published when the backfill never queued")

	// The type was forgotten (forgetDeclaredGVR), so the NEXT Declare re-classifies it as
	// newly-declared and retries the backfill — not recorded as done with a permanent hole.
	assert.Equal(t, []schema.GroupVersionResource{configmapsGVR},
		m.newlyDeclaredSyncedGVRs(myTargetRef(), []schema.GroupVersionResource{configmapsGVR}),
		"the de-recorded type is re-classified as needing a backfill on the next Declare")
}
