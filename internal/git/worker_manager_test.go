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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
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

	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	mustEnsureWorker(ctx, t, manager, "repo1", testProviderNamespace, "main", repo)
	worker, exists := manager.WorkerForGitBranch(testGitBranch("repo1", testProviderNamespace, "main"))
	require.True(t, exists)
	require.NotNil(t, worker)
	assert.NotNil(t, worker.mapper, "the created worker must carry the injected mapper")
	assert.Equal(t, typeset.Lookup(mapper), worker.mapper)
}

const (
	testProviderNamespace = "gitops-system"
	testTargetNamespace   = "default"
)

// mustEnsureWorker wires a worker and fails the test if it could not. It then does what a
// GitTarget reconcile does with the branch: take any pending replacement recovery and acknowledge
// it, reporting whether there was one.
func mustEnsureWorker(
	ctx context.Context,
	t *testing.T,
	m *WorkerManager,
	providerName, providerNamespace, branch string,
	repo RepoIdentity,
) bool {
	t.Helper()
	require.NoError(t, m.EnsureWorker(ctx, claimForTest(m, providerName, providerNamespace, branch, repo)))
	key := BranchKeyFor(repo.URL, branch)
	pending := m.ReplacementPending(key)
	m.AcknowledgeReplacement(key)
	return pending
}

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	return scheme
}

// createProviderWithLocalRepo creates a GitProvider over a local bare repository and returns the
// identity a reconcile would hand EnsureWorker for it.
func createProviderWithLocalRepo(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name string,
) RepoIdentity {
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
	provider.UID = k8stypes.UID("uid-" + name)
	require.NoError(t, k8sClient.Create(ctx, provider))
	return RepoIdentity{ProviderUID: provider.UID, URL: provider.Spec.URL}
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
	target.Spec.GitBranchRef = meta.NamespacedObjectReference{Name: testGitBranchName(providerName, branch)}
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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target1", "repo1", "main", "clusters/prod")

	// Register first target
	err := manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", repo))
	if err != nil {
		t.Fatalf("Failed to register target: %v", err)
	}

	// Verify worker was created
	worker, exists := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
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
	repo := createProviderWithLocalRepo(ctx, t, client, "shared-repo")
	createTargetForRegister(ctx, t, client, "target-apps", "shared-repo", "main", "apps/")
	createTargetForRegister(ctx, t, client, "target-infra", "shared-repo", "main", "infra/")

	// Register two targets for same repo+branch, different paths
	mustEnsureWorker(ctx, t, manager, "shared-repo", "gitops-system", "main", repo)
	mustEnsureWorker(ctx, t, manager, "shared-repo", "gitops-system", "main", repo)

	// Verify only one worker exists
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker for shared repo+branch, got %d", workerCount)
	}

	// Verify worker exists for both targets
	_, exists := manager.WorkerForGitBranch(testGitBranch("shared-repo", "gitops-system", "main"))
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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target-main", "repo1", "main", "base/")
	createTargetForRegister(ctx, t, client, "target-dev", "repo1", "develop", "base/")

	// Register targets for same repo, different branches
	err := manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", repo))
	if err != nil {
		t.Fatalf("Failed to register target-main: %v", err)
	}

	err = manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "develop", repo))
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
	workerMain, exists := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
	if !exists || workerMain.Branch != "main" {
		t.Error("Main branch worker not found or has wrong branch")
	}

	workerDev, exists := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "develop"))
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

	repo := createProviderWithLocalRepo(ctx, t, k8sClient, "repo1")
	createTargetForRegister(ctx, t, k8sClient, "target1", "repo1", "main", "apps/")
	createTargetForRegister(ctx, t, k8sClient, "target2", "repo1", "main", "infra/")
	// The provider lives beside its targets, which is the namespace the sweep derives.
	mustEnsureWorker(ctx, t, manager, "repo1", testTargetNamespace, "main", repo)

	// One of the two goes.
	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target1")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, exists := manager.WorkerForGitBranch(testGitBranch("repo1", testTargetNamespace, "main"))
	assert.True(t, exists, "target2 still writes to this branch, so its worker must survive")

	// And now the last one.
	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target2")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, exists = manager.WorkerForGitBranch(testGitBranch("repo1", testTargetNamespace, "main"))
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
	mustEnsureWorker(ctx, t, manager, "repo1", testTargetNamespace, "main",
		RepoIdentity{URL: "file:///unread.git"})

	err := manager.ReconcileWorkers(ctx)

	require.Error(t, err)
	_, exists := manager.WorkerForGitBranch(testGitBranch("repo1", testTargetNamespace, "main"))
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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target", "repo1", "main", "base/")

	// Concurrently register multiple targets
	done := make(chan bool, 10)
	for i := range 10 {
		go func(index int) {
			err := manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", repo))
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

	worker, exists := manager.WorkerForGitBranch(testGitBranch("nonexistent", "default", "main"))
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
		manager.removeWorkers([]BranchKey{{Repository: "example.com/repo1", Branch: "main"}}, "test")
	})
}

// TestReconcileWorkers_RemovesTheRetiredWorkersCheckout. Stopping the goroutine was only half the
// leak. Each worker keeps a clone per remote under its own (provider, branch) directory, and that
// half survives a restart, so a cluster that creates and deletes targets accumulates checkouts
// until somebody notices the disk.
func TestReconcileWorkers_RemovesTheRetiredWorkersCheckout(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	scheme := setupScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = manager.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	repo := createProviderWithLocalRepo(ctx, t, k8sClient, "repo1")
	createTargetForRegister(ctx, t, k8sClient, "target1", "repo1", "main", "apps/")
	mustEnsureWorker(ctx, t, manager, "repo1", testTargetNamespace, "main", repo)

	worker, exists := manager.WorkerForGitBranch(testGitBranch("repo1", testTargetNamespace, "main"))
	require.True(t, exists)
	// Stand in for the clone a first publication would leave behind.
	checkout := worker.repoPath()
	require.NoError(t, os.MkdirAll(checkout, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))

	require.NoError(t, k8sClient.Delete(ctx, targetForRegister("target1")))
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, err := os.Stat(checkout)
	assert.True(t, os.IsNotExist(err), "the retired worker's checkout must go with it")
}

// TestRemoveLocalState_RefusesAWorkerWithNoIdentity. Only a worker the WorkerManager started has a
// GitBranch UID, and only its directory is known to be its own. Deleting anything else risks the
// live workers beside it, which is a far worse outcome than the leak this reclaims.
func TestRemoveLocalState_RefusesAWorkerWithNoIdentity(t *testing.T) {
	root := withTemporaryWorkerStateRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "someone-elses-state"), 0o750))

	(&BranchWorker{Log: logr.Discard()}).removeLocalState()
	(&BranchWorker{Log: logr.Discard(), repo: RepoIdentity{URL: "https://example.invalid/repo.git"}, Branch: "main"}).
		removeLocalState()

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a worker with no GitBranch UID must never delete anything")
	assert.Equal(t, "someone-elses-state", entries[0].Name())
}

// withTemporaryWorkerStateRoot points every worker's on-disk state at this test's own directory,
// so a test that DELETES worker state cannot reach the shared root — where another test's
// fixtures, or a developer's running operator, keep theirs.
func withTemporaryWorkerStateRoot(t *testing.T) string {
	t.Helper()
	previous := workerStateRoot
	root := t.TempDir()
	workerStateRoot = root
	t.Cleanup(func() { workerStateRoot = previous })
	return root
}

// TestRepoRootPath_IsOneDirectoryPerGitBranch. The GitBranch's UID is the whole path below the
// root, so the branch name, slashes included, never shapes it: `release/v1`'s checkout cannot sit
// inside `release`'s, where retiring `release` would take it.
func TestRepoRootPath_IsOneDirectoryPerGitBranch(t *testing.T) {
	root := withTemporaryWorkerStateRoot(t)
	repo := RepoIdentity{ProviderUID: "uid-provider", URL: "https://example.invalid/repo.git"}
	parent := &BranchWorker{GitBranchUID: "uid-release", repo: repo, Branch: "release"}
	child := &BranchWorker{GitBranchUID: "uid-release-v1", repo: repo, Branch: "release/v1"}

	assert.Equal(t, filepath.Join(root, "uid-release"), parent.repoRootPath())
	assert.Equal(t, filepath.Join(root, "uid-release-v1"), child.repoRootPath())

	// A worker with no GitBranch behind it, which only tests build, has its remote and branch to be
	// unique by, and two of them must still not share a directory.
	tests := map[string]*BranchWorker{
		"another remote": {repo: RepoIdentity{URL: "https://example.invalid/other.git"}, Branch: "main"},
		"another branch": {repo: RepoIdentity{URL: "https://example.invalid/repo.git"}, Branch: "release"},
	}
	base := &BranchWorker{repo: RepoIdentity{URL: "https://example.invalid/repo.git"}, Branch: "main"}
	for name, other := range tests {
		t.Run(name, func(t *testing.T) {
			assert.NotEqual(t, base.repoRootPath(), other.repoRootPath())
			assert.Equal(t, root, filepath.Dir(other.repoRootPath()), "still one directory below the root")
		})
	}
}

// TestWorkerLifecycle_BuildsAndReclaimsItsOwnState walks one worker's whole life on disk, because
// the build-up and the tear-down are the two halves of one rule and only ever break together: a
// worker that reclaims the wrong directory takes a live checkout with it, and one that reclaims
// nothing leaks a clone per branch the operator ever mirrored until the process restarts.
func TestWorkerLifecycle_BuildsAndReclaimsItsOwnState(t *testing.T) {
	manager, ctx := startedManager(t)
	// startedManager points the worker state root at its own directory, so the neighbour below has
	// to be placed under THAT root: one created beforehand would sit outside the tree this test is
	// about, and the assertion would hold even if the sweep deleted the whole active root.
	root := workerStateRoot
	repo := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}

	require.NoError(t, manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", repo)))
	worker, ok := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
	require.True(t, ok)
	own := filepath.Join(root, string(testGitBranchUID(testGitBranch("repo1", "gitops-system", "main"))))
	require.Equal(t, own, worker.repoPath(),
		"precondition: the worker keeps its state under the root this test owns, named by its GitBranch")

	// Three directories under that root: this worker's, and those of two other GitBranches — one
	// writing another branch of the same repository, one writing the same branch name elsewhere.
	// Both GitBranches exist, so the sweep of orphaned checkouts keeps their directories too.
	require.NoError(t, os.MkdirAll(worker.repoPath(), 0o750))
	sibling := createGitBranchCheckout(ctx, t, manager, "gitops-system", "repo1-release")
	neighbour := createGitBranchCheckout(ctx, t, manager, "other", "repo2-main")
	for _, dir := range []string{sibling, neighbour} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	}

	// Nothing needs it: the sweep retires it, and what it reclaims is its own directory.
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, err := os.Stat(worker.repoPath())
	assert.True(t, os.IsNotExist(err), "a retired worker takes its checkout with it")
	_, err = os.Stat(filepath.Join(sibling, "HEAD"))
	require.NoError(t, err, "another GitBranch on the same repository is not this worker's to delete")
	_, err = os.Stat(filepath.Join(neighbour, "HEAD"))
	require.NoError(t, err, "and neither is any other GitBranch's")
	_, err = os.Stat(root)
	require.NoError(t, err, "least of all the root every worker's state lives under")
	_, stillThere := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
	assert.False(t, stillThere)
}

// createGitBranchCheckout creates a GitBranch and the directory its worker would keep, and returns
// that directory.
func createGitBranchCheckout(ctx context.Context, t *testing.T, m *WorkerManager, namespace, name string) string {
	t.Helper()
	uid := k8stypes.UID("uid-" + namespace + "-" + name)
	require.NoError(t, m.Client.Create(ctx, &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid},
	}))
	dir := filepath.Join(workerStateRoot, string(uid))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	return dir
}

// TestReconcileWorkers_ReclaimsCheckoutsNoGitBranchNames. A retiring worker removes its own
// checkout, but a GitBranch deleted while no worker ran for it, across a restart that kept the
// disk, has nobody to do that. The sweep removes every directory no GitBranch names, and keeps
// the checkout of a GitBranch whose worker has not started yet, and that of any live worker.
func TestReconcileWorkers_ReclaimsCheckoutsNoGitBranchNames(t *testing.T) {
	manager, ctx := startedManager(t)
	root := workerStateRoot

	waiting := createGitBranchCheckout(ctx, t, manager, "team-a", "waiting")
	require.NoError(t, manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "team-b", "main",
		RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/repo.git"})))
	live, ok := manager.WorkerForGitBranch(testGitBranch("repo1", "team-b", "main"))
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(live.repoPath(), 0o750))
	deleted := filepath.Join(root, "uid-of-a-deleted-gitbranch")
	previousLayout := filepath.Join(root, "uid-of-a-provider", "main-0123456789abcdef")
	for _, dir := range []string{deleted, previousLayout} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	manager.lifecycleMu.Lock()
	err := manager.reclaimOrphanedCheckoutsLocked(ctx)
	manager.lifecycleMu.Unlock()
	require.NoError(t, err)

	for _, kept := range []string{waiting, live.repoPath()} {
		_, err := os.Stat(kept)
		require.NoError(t, err, "%s belongs to a GitBranch or a live worker", kept)
	}
	for _, gone := range []string{deleted, filepath.Dir(previousLayout)} {
		_, err := os.Stat(gone)
		assert.True(t, os.IsNotExist(err), "%s is named by no GitBranch", gone)
	}
}

// TestReconcileWorkers_ReclaimsNothingWhenTheGitBranchesCannotBeRead. Reading "I could not list
// the GitBranches" as "there are none" would delete every checkout the operator keeps.
func TestReconcileWorkers_ReclaimsNothingWhenTheGitBranchesCannotBeRead(t *testing.T) {
	root := withTemporaryWorkerStateRoot(t)
	orphan := filepath.Join(root, "uid-of-a-deleted-gitbranch")
	require.NoError(t, os.MkdirAll(orphan, 0o750))
	k8sClient := fake.NewClientBuilder().WithScheme(setupScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server is unreachable")
		},
	}).Build()
	manager := NewWorkerManager(k8sClient, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})

	require.Error(t, manager.reclaimOrphanedCheckoutsLocked(context.Background()))

	_, err := os.Stat(orphan)
	require.NoError(t, err, "a failed List deletes nothing")
}

// TestWorkerLifecycle_AReplacementStartsFromAnEmptyCheckout is the same rule across a repoint.
// The replacement writes for the same GitBranch, so it works in the same directory, and the clone
// there is about the repository the provider used to name. The old worker takes it with it before
// the replacement starts, so nothing of the old repository is left to plan against.
func TestWorkerLifecycle_AReplacementStartsFromAnEmptyCheckout(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	manager, ctx := startedManager(t)
	before := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	after := RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}

	require.NoError(t, manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", before)))
	old, ok := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(old.repoPath(), 0o750))
	sentinel := filepath.Join(old.repoPath(), "HEAD")
	require.NoError(t, os.WriteFile(sentinel, []byte("ref: refs/heads/main\n"), 0o600))

	require.NoError(t, manager.EnsureWorker(ctx, claimForTest(manager, "repo1", "gitops-system", "main", after)))
	replacement, ok := manager.WorkerForGitBranch(testGitBranch("repo1", "gitops-system", "main"))
	require.True(t, ok)
	require.NotSame(t, old, replacement)

	assert.Equal(t, old.repoPath(), replacement.repoPath(), "one GitBranch is one directory")
	_, err := os.Stat(sentinel)
	assert.True(t, os.IsNotExist(err), "the retired worker took the old repository's checkout with it")
	assert.True(t, manager.ReplacementPending(BranchKeyFor(after.URL, "main")),
		"and the targets on the branch still wait to be rebuilt in the new one")
}
