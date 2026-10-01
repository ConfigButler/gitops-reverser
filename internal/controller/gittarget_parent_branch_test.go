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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func parentBranchTarget(name, branch, path, parent string, created time.Time) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", CreationTimestamp: metav1.NewTime(created),
		},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "repo1"},
			Branch:         branch,
			Path:           path,
			ParentBranch:   parent,
		},
	}
}

func parentBranchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	return scheme
}

// TestCheckForConflicts_ParentBranch: one write branch, one checkout, one parent. Two targets on the
// same branch that name different parents conflict even on sibling paths, and the later one loses.
// Omitted is its own value, so it conflicts with an explicit "main".
func TestCheckForConflicts_ParentBranch(t *testing.T) {
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	cases := []struct {
		name         string
		existing     *configbutleraiv1alpha3.GitTarget
		target       *configbutleraiv1alpha3.GitTarget
		wantConflict bool
	}{
		{
			name:         "omitted versus main conflicts",
			existing:     parentBranchTarget("first", "edits", "apps/a", "", earlier),
			target:       parentBranchTarget("second", "edits", "apps/b", "main", later),
			wantConflict: true,
		},
		{
			name:     "the earlier target wins",
			existing: parentBranchTarget("second", "edits", "apps/b", "main", later),
			target:   parentBranchTarget("first", "edits", "apps/a", "", earlier),
		},
		{
			name:     "the same parent on sibling paths is fine",
			existing: parentBranchTarget("first", "edits", "apps/a", "release", earlier),
			target:   parentBranchTarget("second", "edits", "apps/b", "release", later),
		},
		{
			name:     "another write branch is another checkout",
			existing: parentBranchTarget("first", "edits", "apps/a", "", earlier),
			target:   parentBranchTarget("second", "other", "apps/b", "release", later),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().WithScheme(parentBranchScheme(t)).WithObjects(tc.existing).Build()
			r := &GitTargetReconciler{Client: k8sClient}

			conflict, msg, reason, err := r.checkForConflicts(context.Background(), tc.target, "shop")

			require.NoError(t, err)
			assert.Equal(t, tc.wantConflict, conflict, msg)
			if tc.wantConflict {
				assert.Equal(t, GitTargetReasonTargetConflict, reason)
				assert.Contains(t, msg, "(omitted: the remote's default branch)")
				assert.Contains(t, msg, "'main'")
			}
		})
	}
}

func startParentBranchWorkers(t *testing.T) (*GitTargetReconciler, *git.WorkerManager) {
	t.Helper()
	k8sClient := fake.NewClientBuilder().WithScheme(parentBranchScheme(t)).Build()
	workers := git.NewWorkerManager(k8sClient, logr.Discard(), git.BranchWorkerLimits{},
		types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = workers.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // Start installs the context EnsureWorker starts workers under
	router := watch.NewEventRouter(workers, nil, k8sClient, logr.Discard())
	return &GitTargetReconciler{Client: k8sClient, WorkerManager: workers, EventRouter: router}, workers
}

// TestEnsureEventStream_AParentBranchChangeKeepsTheWorker: changing spec.parentBranch abandons
// nothing written, so it is not a repoint. The worker stays and re-reads the remote.
func TestEnsureEventStream_AParentBranchChangeKeepsTheWorker(t *testing.T) {
	r, workers := startParentBranchWorkers(t)
	repo := git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/repo.git"}
	target := parentBranchTarget("apps", "edits", "apps", "", time.Now())

	_, err := r.ensureEventStream(context.Background(), target, "shop", repo, logr.Discard())
	require.NoError(t, err)
	before, ok := workers.GetWorkerForTarget("repo1", "shop", "edits")
	require.True(t, ok)
	before.SeedRemoteObservationForTest("", time.Now(), "Fetch")

	target.Spec.ParentBranch = "release"
	_, err = r.ensureEventStream(context.Background(), target, "shop", repo, logr.Discard())
	require.NoError(t, err)

	after, ok := workers.GetWorkerForTarget("repo1", "shop", "edits")
	require.True(t, ok)
	assert.Same(t, before, after, "the worker and its clone are kept")
	assert.Equal(t, "release", after.ParentBranch())
	assert.False(t, after.BaseTrustedForTest(), "and its checkout of the old parent is no longer trusted")
}

// TestParentBranchReadiness: a fetch that found neither the write branch nor its configured parent
// stalls the GitTarget as ParentBranchNotFound. An omitted parent never does.
func TestParentBranchReadiness(t *testing.T) {
	r, workers := startParentBranchWorkers(t)
	repo := git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/repo.git"}
	target := parentBranchTarget("apps", "edits", "apps", "release", time.Now())

	assert.Equal(t, metav1.ConditionTrue, r.parentBranchReadiness(target, "shop", repo).Status,
		"nothing has looked yet")

	_, err := r.ensureEventStream(context.Background(), target, "shop", repo, logr.Discard())
	require.NoError(t, err)
	worker, _ := workers.GetWorkerForTarget("repo1", "shop", "edits")
	worker.RecordMissingParentForTest("release")

	got := r.parentBranchReadiness(target, "shop", repo)
	assert.Equal(t, metav1.ConditionFalse, got.Status)
	assert.Equal(t, GitTargetReasonParentBranchNotFound, got.Reason)
	assert.Contains(t, got.Message, "'release'")

	rd := newGitTargetReadiness()
	gitTargetReadinessGates(rd, dataPlaneObservation{axes: gitTargetAxes{
		Streams: conditionValue{Status: metav1.ConditionTrue},
		GitPath: conditionValue{Status: metav1.ConditionTrue},
		Render:  conditionValue{Status: metav1.ConditionTrue},
	}}, got, healthyDependency(), healthyDependency(), healthyDependency())
	trio := rd.trio()
	assert.Equal(t, metav1.ConditionTrue, trio.Stalled.Status, "it needs somebody to create the branch")
	assert.Equal(t, GitTargetReasonParentBranchNotFound, trio.Ready.Reason)

	target.Spec.ParentBranch = "other"
	assert.Equal(t, metav1.ConditionTrue, r.parentBranchReadiness(target, "shop", repo).Status,
		"the observation is about another parent")
	target.Spec.ParentBranch = ""
	assert.Equal(t, metav1.ConditionTrue, r.parentBranchReadiness(target, "shop", repo).Status)
}

func TestReadyReasonIs(t *testing.T) {
	conditions := []metav1.Condition{{Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: GitTargetReasonParentBranchNotFound}}
	assert.True(t, readyReasonIs(conditions, GitTargetReasonParentBranchNotFound))
	assert.False(t, readyReasonIs(conditions, GitTargetReasonTargetConflict))
	assert.False(t, readyReasonIs(nil, GitTargetReasonParentBranchNotFound))
}

// TestCheckForConflicts_ParentBranchAppliesToInvalidPaths: a target whose path its writer will
// reject still wires the shared worker, so it must not escape the parent check in either role and
// repoint the healthy sibling's parent.
func TestCheckForConflicts_ParentBranchAppliesToInvalidPaths(t *testing.T) {
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	healthy := parentBranchTarget("first", "edits", "apps/a", "main", earlier)
	invalid := parentBranchTarget("second", "edits", "../bad", "release", earlier.Add(time.Hour))

	r, workers := startParentBranchWorkers(t)
	require.NoError(t, r.Client.Create(context.Background(), healthy))
	_, err := r.ensureEventStream(context.Background(), healthy, "shop", firstRepo, logr.Discard())
	require.NoError(t, err)

	conflict, _, reason, err := r.checkForConflicts(context.Background(), invalid, "shop")
	require.NoError(t, err)
	assert.True(t, conflict, "an invalid path must not bypass the parent conflict")
	assert.Equal(t, GitTargetReasonTargetConflict, reason)
	worker, _ := workers.GetWorkerForTarget("repo1", "shop", "edits")
	assert.Equal(t, "main", worker.ParentBranch())

	// And as the earlier object, the invalid target still wins the parent.
	k8sClient := fake.NewClientBuilder().WithScheme(parentBranchScheme(t)).
		WithObjects(parentBranchTarget("older", "edits", "../bad", "release", earlier)).Build()
	conflict, _, _, err = (&GitTargetReconciler{Client: k8sClient}).checkForConflicts(
		context.Background(), parentBranchTarget("newer", "edits", "apps/a", "main", earlier.Add(time.Hour)), "shop")
	require.NoError(t, err)
	assert.True(t, conflict)
}

// TestParentBranchReadiness_IgnoresAnotherRepository: the manager keeps a branch's observation across
// a repoint, and a missing parent in the old repository says nothing about the new one.
func TestParentBranchReadiness_IgnoresAnotherRepository(t *testing.T) {
	r, workers := startParentBranchWorkers(t)
	target := parentBranchTarget("apps", "edits", "apps", "release", time.Now())
	_, err := r.ensureEventStream(context.Background(), target, "shop", firstRepo, logr.Discard())
	require.NoError(t, err)
	worker, _ := workers.GetWorkerForTarget("repo1", "shop", "edits")
	worker.RecordMissingParentForTest("release")
	require.Equal(t, metav1.ConditionFalse, r.parentBranchReadiness(target, "shop", firstRepo).Status)

	_, err = r.ensureEventStream(context.Background(), target, "shop", secondRepo, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, r.parentBranchReadiness(target, "shop", secondRepo).Status,
		"the old repository's missing parent is not evidence about the replacement")
}
