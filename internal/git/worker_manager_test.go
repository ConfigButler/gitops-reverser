// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, m.EnsureWorker(ctx, providerName, providerNamespace, branch, repo))
	key := BranchKey{RepoNamespace: providerNamespace, RepoName: providerName, Branch: branch}
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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target1", "repo1", "main", "clusters/prod")

	// Register first target
	err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo)
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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target-main", "repo1", "main", "base/")
	createTargetForRegister(ctx, t, client, "target-dev", "repo1", "develop", "base/")

	// Register targets for same repo, different branches
	err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo)
	if err != nil {
		t.Fatalf("Failed to register target-main: %v", err)
	}

	err = manager.EnsureWorker(ctx, "repo1", "gitops-system", "develop", repo)
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

	repo := createProviderWithLocalRepo(ctx, t, k8sClient, "repo1")
	createTargetForRegister(ctx, t, k8sClient, "target1", "repo1", "main", "apps/")
	createTargetForRegister(ctx, t, k8sClient, "target2", "repo1", "main", "infra/")
	// The provider lives beside its targets, which is the namespace the sweep derives.
	mustEnsureWorker(ctx, t, manager, "repo1", testTargetNamespace, "main", repo)

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
	mustEnsureWorker(ctx, t, manager, "repo1", testTargetNamespace, "main",
		RepoIdentity{URL: "file:///unread.git"})

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
	repo := createProviderWithLocalRepo(ctx, t, client, "repo1")
	createTargetForRegister(ctx, t, client, "target", "repo1", "main", "base/")

	// Concurrently register multiple targets
	done := make(chan bool, 10)
	for i := range 10 {
		go func(index int) {
			err := manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo)
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

	worker, exists := manager.GetWorkerForTarget("repo1", testTargetNamespace, "main")
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

// TestRemoveLocalState_RefusesAWorkerWithNoIdentity. repoRootPath joins the identity and the
// branch into a fixed prefix, so a worker with neither resolves to the ROOT of every worker's
// state. Deleting that would take out the live workers beside it, which is a far worse outcome
// than the leak this reclaims.
func TestRemoveLocalState_RefusesAWorkerWithNoIdentity(t *testing.T) {
	root := withTemporaryWorkerStateRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "someone-elses-state"), 0o750))

	(&BranchWorker{Log: logr.Discard()}).removeLocalState()

	_, err := os.Stat(filepath.Join(root, "someone-elses-state"))
	assert.NoError(t, err, "a worker with no identity must never delete the shared root")
}

// TestBranchPathComponent_KeepsOneBranchOutOfAnothersDirectory. Git branch names contain slashes,
// so joining one into a path made `release/v1`'s worker a child of `release`'s — and once a
// retired worker deletes its directory, retiring `release` would take the live worker's checkout
// with it.
func TestBranchPathComponent_KeepsOneBranchOutOfAnothersDirectory(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	provider := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/repo.git"}
	parent := &BranchWorker{repo: provider, Branch: "release"}
	child := &BranchWorker{repo: provider, Branch: "release/v1"}

	rel, err := filepath.Rel(parent.repoRootPath(), child.repoRootPath())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rel, ".."), "release/v1 must not be reachable from inside release")

	// And names that sanitising alone would collapse stay apart.
	lookalike := &BranchWorker{repo: provider, Branch: "release-v1"}
	assert.NotEqual(t, child.repoRootPath(), lookalike.repoRootPath())
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

// TestRepoRootPath_IsKeyedByTheProviderIncarnation. The UID is the whole of the first path
// component, which is what makes a repointed provider's worker unable to inherit, share or delete
// the directory its predecessor was using: a repoint is a recreate, and a recreate mints a UID.
func TestRepoRootPath_IsKeyedByTheProviderIncarnation(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	sameURL := "https://example.invalid/repo.git"
	before := &BranchWorker{repo: RepoIdentity{ProviderUID: "uid-1", URL: sameURL}, Branch: "main"}
	after := &BranchWorker{repo: RepoIdentity{ProviderUID: "uid-2", URL: sameURL}, Branch: "main"}

	assert.NotEqual(t, before.repoRootPath(), after.repoRootPath(),
		"the same URL under a recreated provider is a different checkout")

	// A worker with no provider behind it — the CLI, and tests — has only its remote to be unique
	// by, and two of them must still not share a directory.
	cliA := &BranchWorker{repo: RepoIdentity{URL: "https://example.invalid/a.git"}, Branch: "main"}
	cliB := &BranchWorker{repo: RepoIdentity{URL: "https://example.invalid/b.git"}, Branch: "main"}
	assert.NotEqual(t, cliA.repoRootPath(), cliB.repoRootPath())
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

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", repo))
	worker, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	require.True(t, strings.HasPrefix(worker.repoPath(), root+string(filepath.Separator)),
		"precondition: the worker keeps its state under the root this test owns")

	// Three directories under that root: this worker's, another provider's, and — the one that
	// matters most — a SIBLING BRANCH of the same provider. Branch siblings share the UID
	// component, so a removal one level too high takes every branch the provider mirrors.
	require.NoError(t, os.MkdirAll(worker.repoPath(), 0o750))
	sibling := filepath.Join(root, string(repo.ProviderUID), branchPathComponent("release"))
	neighbour := filepath.Join(root, "uid-2", branchPathComponent("main"))
	for _, dir := range []string{sibling, neighbour} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	}

	// Nothing needs it: the sweep retires it, and what it reclaims is its own directory.
	require.NoError(t, manager.ReconcileWorkers(ctx))

	_, err := os.Stat(worker.repoPath())
	assert.True(t, os.IsNotExist(err), "a retired worker takes its checkout with it")
	_, err = os.Stat(filepath.Join(sibling, "HEAD"))
	require.NoError(t, err, "a sibling BRANCH of the same provider is not this worker's to delete")
	_, err = os.Stat(filepath.Join(neighbour, "HEAD"))
	require.NoError(t, err, "and neither is another provider's")
	_, err = os.Stat(root)
	require.NoError(t, err, "least of all the root every worker's state lives under")
	_, stillThere := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	assert.False(t, stillThere)
}

// TestWorkerLifecycle_AReplacementDoesNotReclaimItsPredecessorsCheckout is the same rule across a
// repoint. The two workers are different incarnations of one (provider, branch), so they hold
// different directories, and stopping the old one must not touch the new one's — which is exactly
// what the old layout, keyed by provider NAME with a URL digest inside it, could not promise for a
// provider recreated against the same repository.
func TestWorkerLifecycle_AReplacementDoesNotReclaimItsPredecessorsCheckout(t *testing.T) {
	withTemporaryWorkerStateRoot(t)
	manager, ctx := startedManager(t)
	sameURL := "https://example.invalid/repo.git"
	before := RepoIdentity{ProviderUID: "uid-1", URL: sameURL}
	after := RepoIdentity{ProviderUID: "uid-2", URL: sameURL}

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", before))
	old, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(old.repoPath(), 0o750))

	// The replacement's directory, and a sentinel in it, BEFORE the replacement happens: a
	// directory created afterwards would prove nothing about what the removal deleted.
	successor := filepath.Join(workerStateRoot, string(after.ProviderUID), branchPathComponent("main"))
	require.NoError(t, os.MkdirAll(successor, 0o750))
	sentinel := filepath.Join(successor, "HEAD")
	require.NoError(t, os.WriteFile(sentinel, []byte("ref: refs/heads/main\n"), 0o600))

	require.NoError(t, manager.EnsureWorker(ctx, "repo1", "gitops-system", "main", after))
	replacement, ok := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	require.NotSame(t, old, replacement)

	require.Equal(t, successor, replacement.repoPath(),
		"the replacement's checkout is the one the sentinel was placed in")
	require.NotEqual(t, old.repoPath(), replacement.repoPath(),
		"one repository under two provider incarnations is two checkouts")
	_, err := os.Stat(old.repoPath())
	assert.True(t, os.IsNotExist(err), "the retired worker took its own checkout with it")
	_, err = os.Stat(sentinel)
	require.NoError(t, err, "and left the replacement's alone")
	assert.True(t, manager.ReplacementPending(
		BranchKey{RepoNamespace: "gitops-system", RepoName: "repo1", Branch: "main"}),
		"and the targets on the branch still owe a rebuild in the new one")
}
