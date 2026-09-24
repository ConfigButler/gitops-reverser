// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// TestWorkerManager_SetMapperInjectsIntoWorkers proves the production wiring: a mapper
// set on the manager is handed to every worker it creates, so the live writer builds a
// resource-identity inventory. Without injection worker.mapper is nil and object-less
// deletes have no resource index to target.
func TestWorkerManager_SetMapperInjectsIntoWorkers(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(setupScheme()).Build()
	manager := NewWorkerManager(client, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})

	mapper := typeset.NewSnapshotRegistry(typeset.Snapshot{})
	manager.SetMapper(mapper)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = manager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // allow Start to set m.ctx

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", testProviderNamespace, "main"))
	worker, exists := manager.GetWorkerForTarget("repo1", testProviderNamespace, "main")
	require.True(t, exists)
	require.NotNil(t, worker)
	assert.NotNil(t, worker.mapper, "the created worker must carry the injected mapper")
	assert.Equal(t, typeset.Lookup(mapper), worker.mapper)
}

const (
	testProviderNamespace = "gitops-system"
	testTargetNamespace   = "default"
)

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	return scheme
}

func createProviderWithLocalRepo(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name string,
) {
	t.Helper()

	remotePath := filepath.Join(t.TempDir(), name+".git")
	createBareRepo(t, remotePath)

	provider := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{
			URL: "file://" + remotePath,
		},
	}
	provider.Name = name
	provider.Namespace = testProviderNamespace
	require.NoError(t, k8sClient.Create(ctx, provider))
}

func createTargetForRegister(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name, providerName, branch, path string,
) {
	t.Helper()
	target := &configv1alpha3.GitTarget{}
	target.Name = name
	target.Namespace = testTargetNamespace
	target.Spec.GitProviderRef = meta.LocalObjectReference{
		Name: providerName,
	}
	target.Spec.Branch = branch
	target.Spec.Path = path
	require.NoError(t, k8sClient.Create(ctx, target))
}

// TestEnsureWorker_CreatesAWorkerWithTheIdentityItWasAskedFor.
func TestEnsureWorker_CreatesAWorkerWithTheIdentityItWasAskedFor(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log, BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start manager
	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // Allow manager to start
	createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target1", "repo1", "main", "clusters/prod")

	// Register first target
	err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to register target: %v", err)
	}

	// Verify worker was created
	worker, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist after registration")
	}
	if worker == nil {
		t.Fatal("Worker should not be nil")
	}

	// Verify worker has correct identity
	if worker.GitProviderRef != "repo1" {
		t.Errorf("Worker RepoRef = %q, want 'repo1'", worker.GitProviderRef)
	}
	if worker.GitProviderNamespace != "gitops-system" {
		t.Errorf("Worker Namespace = %q, want 'gitops-system'", worker.GitProviderNamespace)
	}
	if worker.Branch != "main" {
		t.Errorf("Worker Branch = %q, want 'main'", worker.Branch)
	}

	// Verify target registration succeeded (no longer tracks internally)
	// The worker exists and registration completed without error

	// Cleanup
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerMultipleTargetsSameBranch verifies multiple targets can share a worker.
func TestWorkerManagerMultipleTargetsSameBranch(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log, BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(ctx, t, client, "shared-repo")
	createTargetForRegister(ctx, t, client, "target-apps", "shared-repo", "main", "apps/")
	createTargetForRegister(ctx, t, client, "target-infra", "shared-repo", "main", "infra/")

	// Register two targets for same repo+branch, different paths
	err := manager.EnsureWorker(ctx, "shared-repo", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to register target-apps: %v", err)
	}

	err = manager.EnsureWorker(ctx, "shared-repo", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to register target-infra: %v", err)
	}

	// Verify only one worker exists
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker for shared repo+branch, got %d", workerCount)
	}

	// Verify worker exists for both targets
	_, exists := manager.GetWorkerForTarget("shared-repo", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerDifferentBranches verifies different branches get different workers.
func TestWorkerManagerDifferentBranches(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log, BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target-main", "repo1", "main", "base/")
	createTargetForRegister(ctx, t, client, "target-dev", "repo1", "develop", "base/")

	// Register targets for same repo, different branches
	err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to register target-main: %v", err)
	}

	err = manager.EnsureWorker(ctx, "repo1", "gitops-system", "develop")
	if err != nil {
		t.Fatalf("Failed to register target-dev: %v", err)
	}

	// Verify two workers exist
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 2 {
		t.Errorf("Should have 2 workers for different branches, got %d", workerCount)
	}

	// Verify each worker exists and has correct branch
	workerMain, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if !exists || workerMain.Branch != "main" {
		t.Error("Main branch worker not found or has wrong branch")
	}

	workerDev, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "develop")
	if !exists || workerDev.Branch != "develop" {
		t.Error("Develop branch worker not found or has wrong branch")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// targetForRegister addresses a GitTarget createTargetForRegister made, for a delete.
func targetForRegister(name string) *configv1alpha3.GitTarget {
	target := &configv1alpha3.GitTarget{}
	target.Name = name
	target.Namespace = testTargetNamespace
	return target
}

// TestReconcileWorkers_KeepsAWorkerASiblingStillNeeds is the rule a per-target count gets wrong.
// One worker serves every GitTarget on its (provider, branch), so deleting one of them says
// nothing about the others: the sweep decides from the API, where the siblings are still listed.
func TestReconcileWorkers_KeepsAWorkerASiblingStillNeeds(t *testing.T) {
	scheme := setupScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = manager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	createProviderWithLocalRepo(ctx, t, k8sClient, "repo1")
	createTargetForRegister(ctx, t, k8sClient, "target1", "repo1", "main", "apps/")
	createTargetForRegister(ctx, t, k8sClient, "target2", "repo1", "main", "infra/")
	// The provider lives beside its targets, which is the namespace the sweep derives.
	require.NoError(t, manager.EnsureWorker(ctx, "repo1", testTargetNamespace, "main"))

	// One of the two goes.
	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target1")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, exists := manager.GetWorkerForTarget("repo1", testTargetNamespace, "main")
	assert.True(t, exists, "target2 still writes to this branch, so its worker must survive")

	// And now the last one.
	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target2")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, exists = manager.GetWorkerForTarget("repo1", testTargetNamespace, "main")
	assert.False(t, exists,
		"nothing needs this worker now, and it holds a goroutine, a queue and a clone until it stops")
}

// TestReconcileWorkers_StopsNothingWhenTheTargetsCannotBeRead. Treating "I could not read the
// GitTargets" as "there are no GitTargets" would stop every live worker in the process, which is
// the one outcome a cleanup sweep must never produce.
func TestReconcileWorkers_StopsNothingWhenTheTargetsCannotBeRead(t *testing.T) {
	scheme := setupScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("the cache is not having it")
			},
		}).Build()

	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = manager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, manager.EnsureWorker(ctx, "repo1", testTargetNamespace, "main"))

	err := manager.ReconcileWorkers(ctx)

	require.Error(t, err)
	_, exists := manager.GetWorkerForTarget("repo1", testTargetNamespace, "main")
	assert.True(t, exists, "a read that failed proves nothing about which workers are needed")
}

// TestWorkerManagerConcurrentRegistration verifies thread safety.
func TestWorkerManagerConcurrentRegistration(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log, BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target", "repo1", "main", "base/")

	// Concurrently register multiple targets
	done := make(chan bool, 10)
	for i := range 10 {
		go func(index int) {
			err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main")
			if err != nil {
				t.Errorf("Failed to ensure worker %d: %v", index, err)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}

	// Verify only one worker was created (same repo+branch)
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker despite concurrent registration, got %d", workerCount)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerGetNonexistentWorker verifies getting nonexistent worker returns false.
func TestWorkerManagerGetNonexistentWorker(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log, BranchWorkerLimits{}, types.SensitiveResourcePolicy{})

	worker, exists := manager.GetWorkerForTarget("nonexistent", "default", "main")
	if exists {
		t.Error("Should return exists=false for nonexistent worker")
	}
	if worker != nil {
		t.Error("Worker should be nil for nonexistent key")
	}
}

// TestRemoveWorkers_IgnoresAKeyThatNamesNoWorker keeps the sweep idempotent: it computes its
// orphan list outside the lifecycle lock, so a worker can have gone by the time it is removed.
func TestRemoveWorkers_IgnoresAKeyThatNamesNoWorker(t *testing.T) {
	scheme := setupScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})

	assert.NotPanics(t, func() {
		manager.removeWorkers([]BranchKey{{
			RepoNamespace: "gitops-system", RepoName: "repo1", Branch: "main",
		}}, "test")
	})
}

// TestReconcileWorkers_RemovesTheRetiredWorkersCheckout. Stopping the goroutine was only half the
// leak. Each worker keeps a clone per remote under its own (provider, branch) directory, and that
// half survives a restart, so a cluster that creates and deletes targets accumulates checkouts
// until somebody notices the disk.
func TestReconcileWorkers_RemovesTheRetiredWorkersCheckout(t *testing.T) {
	scheme := setupScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = manager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	createProviderWithLocalRepo(ctx, t, k8sClient, "repo1")
	createTargetForRegister(ctx, t, k8sClient, "target1", "repo1", "main", "apps/")
	require.NoError(t, manager.EnsureWorker(ctx, "repo1", testTargetNamespace, "main"))

	worker, exists := manager.GetWorkerForTarget("repo1", testTargetNamespace, "main")
	require.True(t, exists)
	// Stand in for the clone a first publication would leave behind.
	checkout := worker.repoPathForRemote("https://example.invalid/repo.git")
	require.NoError(t, os.MkdirAll(checkout, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	t.Cleanup(func() { _ = os.RemoveAll(worker.repoRootPath()) })

	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target1")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, err := os.Stat(checkout)
	assert.True(t, os.IsNotExist(err), "the retired worker's checkout must go with it")
}

// TestRemoveLocalState_RefusesAWorkerWithNoIdentity. repoRootPath joins the three identity fields
// into a fixed prefix, so a worker with empty fields resolves to the ROOT of every worker's state.
// Deleting that would take out the live workers beside it, which is a far worse outcome than the
// leak this reclaims.
func TestRemoveLocalState_RefusesAWorkerWithNoIdentity(t *testing.T) {
	root := (&BranchWorker{Log: logr.Discard()}).repoRootPath()
	require.NoError(t, os.MkdirAll(root, 0o750))
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	(&BranchWorker{Log: logr.Discard()}).removeLocalState()

	_, err := os.Stat(root)
	assert.NoError(t, err, "a worker with no identity must never delete the shared root")
}
