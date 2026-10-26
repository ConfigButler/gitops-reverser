// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestBranchWorker_EmptyRepository tests that BranchWorker properly handles empty repositories
// that have no commits yet. This is a critical scenario for bootstrapping new repositories.
func TestBranchWorker_EmptyRepository(t *testing.T) {
	ctx := context.Background()

	// Create a temporary directory for the empty repository
	tempDir := t.TempDir()
	repoPath := filepath.Join(tempDir, "empty-repo")

	// Initialize an empty git repository (no initial commit)
	_, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	// Create a BranchWorker for this empty repository
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := logr.Discard()
	worker := NewBranchWorker(
		client,
		logger,
		"test-repo",
		"default",
		"main",
		RepoIdentity{URL: "file://" + repoPath},
		nil,
		BranchWorkerLimits{},
	)

	// Create a GitProvider in the fake client pointing to our empty repo
	repoConfig := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{
			URL: "file://" + repoPath,
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "default"
	err = client.Create(ctx, repoConfig)
	require.NoError(t, err)

	// Test ensureRepositoryInitialized with empty repo
	err = worker.ensureRepositoryInitialized(ctx)
	require.NoError(t, err, "ensureRepositoryInitialized should succeed with empty repository")

	// The worker has looked, and what it saw is that the branch is not there. An empty revision
	// IS that observation: a branch does not exist without a commit.
	observed, ok := worker.LastRemoteObservation()
	require.True(t, ok, "preparing the branch is a look at the remote, so it must be recorded")
	assert.Empty(t, observed.Commit, "an empty repository advertises no branch")
	assert.Equal(t, ObservedByFetch, observed.By)
	assert.False(t, observed.At.IsZero(), "an observation is dated")
}

// TestBranchWorker_IdentityFields verifies worker identity is set correctly.
func TestBranchWorker_IdentityFields(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(
		client,
		log,
		"my-repo",
		"my-namespace",
		"develop",
		RepoIdentity{},
		nil,
		BranchWorkerLimits{},
	)

	if worker.GitProviderRef != "my-repo" {
		t.Errorf("Expected GitProviderRef 'my-repo', got %q", worker.GitProviderRef)
	}
	if worker.GitProviderNamespace != "my-namespace" {
		t.Errorf("Expected GitProviderNamespace 'my-namespace', got %q", worker.GitProviderNamespace)
	}
	if worker.Branch != "develop" {
		t.Errorf("Expected Branch 'develop', got %q", worker.Branch)
	}
}

func TestBranchWorker_CommitAndPushRequest_PreparesRepositoryBeforeFirstWrite(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	require.NoError(t, os.WriteFile(filepath.Join(seedPath, "README.md"), []byte("seed\n"), 0o600))
	_, err := seedWorktree.Add("README.md")
	require.NoError(t, err)
	_, err = seedWorktree.Commit("seed remote", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(
		k8sClient,
		logr.Discard(),
		"test-repo",
		"default",
		"main",
		RepoIdentity{URL: remoteURL},
		nil,
		BranchWorkerLimits{},
	)
	worker.ctx = ctx

	request := &WriteRequest{
		Events: []Event{
			{
				Operation: "CREATE",
				Identifier: itypes.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "configmaps",
					Namespace: "default",
					Name:      "example",
				},
				Object: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name":      "example",
							"namespace": "default",
						},
						"data": map[string]interface{}{
							"key": "value",
						},
					},
				},
				UserInfo: UserInfo{Username: "tester"},
				Path:     "clusters/dev",
			},
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, request.Events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	localRepoPath := worker.repoPath()
	localRepo, err := git.PlainOpen(localRepoPath)
	require.NoError(t, err)

	localHeadRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	remoteHeadRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, remoteHeadRef.Hash(), localHeadRef.Hash(), "local checkout should have been prepared and pushed")

	readmeContent, err := os.ReadFile(filepath.Join(localRepoPath, "README.md"))
	require.NoError(t, err)
	assert.Equal(
		t,
		"seed\n",
		string(readmeContent),
		"worker should prepare by pulling remote content before first write",
	)

	manifestPath := filepath.Join(localRepoPath, "clusters/dev", "default", "configmaps", "example.yaml")
	content, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "name: example")
	assert.Contains(t, string(content), "key: value")
}

func TestBranchWorker_CommitAndPushRequest_NewBranchStartsFromLatestMain(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	hashA := commitFileChange(t, seedWorktree, seedPath, "README.md", "v1\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(
		k8sClient,
		logr.Discard(),
		"test-repo",
		"default",
		"feature",
		RepoIdentity{URL: remoteURL},
		nil,
		BranchWorkerLimits{},
	)
	worker.ctx = ctx

	// Pre-create a stale local checkout while remote main is still at commit A.
	staleRepoPath := worker.repoPath()
	staleReport, err := PrepareBranch(ctx, remoteURL, staleRepoPath, worker.Branch, nil)
	require.NoError(t, err)
	require.Equal(t, hashA.String(), staleReport.HEAD.Sha)

	// Advance remote main to commit B after the local checkout is stale.
	hashB := commitFileChange(t, seedWorktree, seedPath, "LATEST.md", "from-main-b\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	request := &WriteRequest{
		Events: []Event{
			{
				Operation: "CREATE",
				Identifier: itypes.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "configmaps",
					Namespace: "default",
					Name:      "example-feature",
				},
				Object: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name":      "example-feature",
							"namespace": "default",
						},
						"data": map[string]interface{}{
							"key": "value",
						},
					},
				},
				UserInfo: UserInfo{Username: "tester"},
				Path:     "clusters/dev",
			},
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, request.Events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	remoteMainRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, hashB, remoteMainRef.Hash(), "remote main should remain at the latest commit")

	remoteFeatureRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("feature"), true)
	require.NoError(t, err)

	featureCommit, err := serverRepo.CommitObject(remoteFeatureRef.Hash())
	require.NoError(t, err)
	require.Len(t, featureCommit.ParentHashes, 1, "new feature branch commit should be based on main plus one commit")
	assert.Equal(t, hashB, featureCommit.ParentHashes[0], "feature branch should start from latest main commit")

	localRepo, err := git.PlainOpen(staleRepoPath)
	require.NoError(t, err)
	localFeatureRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("feature"), true)
	require.NoError(t, err)
	assert.Equal(t, remoteFeatureRef.Hash(), localFeatureRef.Hash(), "local and remote feature branches should match")

	featureCheckoutPath := filepath.Join(tempDir, "verify-feature")
	_, _ = initLocalRepo(t, featureCheckoutPath, remoteURL, "feature")

	latestMainContent, err := os.ReadFile(filepath.Join(featureCheckoutPath, "LATEST.md"))
	require.NoError(t, err)
	assert.Equal(t, "from-main-b\n", string(latestMainContent), "feature branch should include latest main content")

	manifestPath := filepath.Join(
		featureCheckoutPath,
		"clusters/dev",
		"default",
		"configmaps",
		"example-feature.yaml",
	)
	manifestContent, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Contains(t, string(manifestContent), "name: example-feature")
}

func TestBranchWorker_CommitAndPushRequest_UsesProviderCommitterAndTargetMessage(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	commitFileChange(t, seedWorktree, seedPath, "README.md", "seed\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// The committer identity comes from the connection, the message wording from the folder.
	provider := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{
			URL: remoteURL,
			Commit: &configv1alpha3.CommitSpec{
				Committer: &configv1alpha3.CommitterSpec{
					Name:  "Audit Bot",
					Email: "audit@example.com",
				},
			},
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	target := &configv1alpha3.GitTarget{}
	target.Name = "audit-target"
	target.Namespace = "default"
	target.Spec.GitBranchRef = meta.NamespacedObjectReference{Name: testGitBranchName("test-repo", "main")}
	target.Spec.Path = "clusters/dev"
	target.Spec.Commit = &configv1alpha3.GitTargetCommitSpec{
		Message: &configv1alpha3.CommitMessageSpec{
			LiveTemplate: "audit: {{.Author}} {{range .Resources}}{{.Operation}} {{.APIVersion}}/{{.Resource}}/{{.Name}}{{end}}",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	worker := NewBranchWorker(
		k8sClient,
		logr.Discard(),
		"test-repo",
		"default",
		"main",
		RepoIdentity{URL: remoteURL},
		nil,
		BranchWorkerLimits{},
	)
	worker.ctx = ctx

	request := &WriteRequest{
		Events: []Event{
			{
				Operation: "CREATE",
				Identifier: itypes.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "configmaps",
					Namespace: "default",
					Name:      "example",
				},
				Object: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name":      "example",
							"namespace": "default",
						},
					},
				},
				UserInfo:           UserInfo{Username: "alice"},
				Path:               "clusters/dev",
				GitTargetName:      "audit-target",
				GitTargetNamespace: "default",
			},
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, request.Events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	remoteHeadRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	commit, err := serverRepo.CommitObject(remoteHeadRef.Hash())
	require.NoError(t, err)
	assert.Equal(t, "audit: alice CREATE v1/configmaps/example", commit.Message)
	assert.Equal(t, "Audit Bot", commit.Committer.Name)
	assert.Equal(t, "audit@example.com", commit.Committer.Email)
	assert.Equal(t, "alice", commit.Author.Name)
	assert.Equal(t, ConstructSafeEmail("alice", "cluster.local"), commit.Author.Email)
}

func TestBranchWorker_CommitAndPushRequest_SignsCommitWhenConfigured(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	commitFileChange(t, seedWorktree, seedPath, "README.md", "seed\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	privateKey, publicKey, err := GenerateSSHSigningKeyPair(nil)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	signingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "signing-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			signingKeyDataKey: privateKey,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, signingSecret))

	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configv1alpha3.GitProviderSpec{
			URL: remoteURL,
			Commit: &configv1alpha3.CommitSpec{
				Signing: &configv1alpha3.CommitSigningSpec{
					SecretRef: meta.LocalObjectReference{Name: "signing-secret"},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(
		k8sClient,
		logr.Discard(),
		"test-repo",
		"default",
		"main",
		RepoIdentity{URL: remoteURL},
		nil,
		BranchWorkerLimits{},
	)
	worker.ctx = ctx

	request := &WriteRequest{
		Events: []Event{
			{
				Operation: "CREATE",
				Identifier: itypes.ResourceIdentifier{
					Version:   "v1",
					Resource:  "configmaps",
					Namespace: "default",
					Name:      "signed",
				},
				Object: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name":      "signed",
							"namespace": "default",
						},
					},
				},
				UserInfo: UserInfo{Username: "alice"},
				Path:     "clusters/dev",
			},
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, request.Events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	remoteHeadRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	commit, err := serverRepo.CommitObject(remoteHeadRef.Hash())
	require.NoError(t, err)
	assert.Contains(t, commit.Signature, "-----BEGIN SSH SIGNATURE-----")

	signingPublicKey, err := SSHAuthorizedPublicKeyFromSecret(signingSecret)
	require.NoError(t, err)
	assert.Equal(t, string(publicKey), signingPublicKey)
}

func TestBranchWorker_CommitAndPushRequest_SkipsWriteWhenSigningSecretIsInvalid(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	commitFileChange(t, seedWorktree, seedPath, "README.md", "seed\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha3.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	signingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "signing-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			signingKeyDataKey: []byte("not-a-private-key"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, signingSecret))

	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configv1alpha3.GitProviderSpec{
			URL: remoteURL,
			Commit: &configv1alpha3.CommitSpec{
				Signing: &configv1alpha3.CommitSigningSpec{
					SecretRef: meta.LocalObjectReference{Name: "signing-secret"},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(
		k8sClient,
		logr.Discard(),
		"test-repo",
		"default",
		"main",
		RepoIdentity{URL: remoteURL},
		nil,
		BranchWorkerLimits{},
	)
	worker.ctx = ctx

	request := &WriteRequest{
		Events: []Event{
			{
				Operation: "CREATE",
				Identifier: itypes.ResourceIdentifier{
					Version:   "v1",
					Resource:  "configmaps",
					Namespace: "default",
					Name:      "unsigned",
				},
				Object: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name":      "unsigned",
							"namespace": "default",
						},
					},
				},
				UserInfo: UserInfo{Username: "alice"},
				Path:     "clusters/dev",
			},
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, request.Events)
	require.Error(t, err)
	assert.Nil(t, pendingWrite)

	finalRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, initialRef.Hash(), finalRef.Hash())
}
