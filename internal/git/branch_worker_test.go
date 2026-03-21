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

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

func setupBranchWorkerTest() (*BranchWorker, func()) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "test-repo", "gitops-system", "main", nil)

	cleanup := func() {
		if worker.started {
			worker.Stop()
		}
	}

	return worker, cleanup
}

func TestRepoCacheKey_DeterministicAndDistinct(t *testing.T) {
	a := repoCacheKey("https://example.com/foo.git")
	b := repoCacheKey("https://example.com/foo.git")
	c := repoCacheKey("https://example.com/bar.git")
	d := repoCacheKey(" https://example.com/foo.git ")

	require.Equal(t, a, b, "same URL should produce same cache key")
	require.NotEqual(t, a, c, "different URL should produce different cache key")
	require.Equal(t, a, d, "cache key should ignore surrounding whitespace")
}

// TestListResourcesInPath_BasicFunctionality verifies ListResourcesInPath can be called.
func TestListResourcesInPath_BasicFunctionality(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test verifies the method can be called without panicking
	// In a real scenario, this would require setting up a Git repository
	// For now, we just ensure the method signature and basic flow work
	_, err := worker.ListResourcesInPath("apps")

	// We expect an error since no GitProvider exists in the fake client
	// But the important thing is that the method doesn't panic
	if err == nil {
		t.Error("Expected error due to missing GitProvider, but got nil")
	}
}

// TestListResourcesInPath_WithGitProvider verifies resources are listed correctly.
func TestListResourcesInPath_WithGitProvider(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitProvider in the fake client
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL:             "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitProvider: %v", err)
	}

	// Call ListResourcesInPath - with new abstraction, initialization succeeds
	// but listing resources will return empty list for fake repo
	resources, err := worker.ListResourcesInPath("apps")

	// With the new abstraction, we expect success but empty resource list
	if err != nil {
		t.Logf("Got expected error during fetch: %v", err)
	} else {
		// If no error (abstraction handles it gracefully), verify empty list
		assert.Empty(t, resources, "Should return empty list for fake repository")
	}
}

// TestListResourcesInPath_DifferentPaths verifies different paths are handled.
func TestListResourcesInPath_DifferentPaths(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitProvider in the fake client
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL:             "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitProvider: %v", err)
	}

	// Test different paths - with new abstraction, method handles them gracefully
	paths := []string{"apps", "infra", "", "clusters/prod"}

	for _, path := range paths {
		resources, err := worker.ListResourcesInPath(path)

		// With new abstraction, we either get an error during fetch or empty list
		if err != nil {
			t.Logf("Got expected error for path %q: %v", path, err)
		} else {
			// Method succeeded - verify it returns empty list for fake repo
			assert.Empty(t, resources, "Should return empty list for path %q", path)
		}
	}
}

func TestListResourceIdentifiersInPath_PathPrefixParsesAsCoreGroup(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	repoPath := t.TempDir()
	targetPath := "live-cluster"

	resourcePath := filepath.Join(repoPath, targetPath, "v1", "configmaps", "ns1", "oeps3.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(resourcePath), 0o755))
	require.NoError(t, os.WriteFile(resourcePath, []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o600))

	markerPath := filepath.Join(repoPath, targetPath, ".configbutler")
	require.NoError(t, os.WriteFile(markerPath, []byte("marker"), 0o600))

	resources, err := worker.listResourceIdentifiersInPath(repoPath, targetPath)
	require.NoError(t, err)
	require.Len(t, resources, 1, "marker files should be ignored")

	assert.Empty(t, resources[0].Group)
	assert.Equal(t, "v1", resources[0].Version)
	assert.Equal(t, "configmaps", resources[0].Resource)
	assert.Equal(t, "ns1", resources[0].Namespace)
	assert.Equal(t, "oeps3", resources[0].Name)
}

// TestListResourcesInPath_MissingGitProvider verifies proper error when GitProvider is missing.
func TestListResourcesInPath_MissingGitProvider(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Don't create GitProvider - should fail
	_, err := worker.ListResourcesInPath("apps")

	if err == nil {
		t.Error("Expected error when GitProvider is missing, but got nil")
	}
}

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
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := logr.Discard()
	worker := NewBranchWorker(client, logger, "test-repo", "default", "main", nil)

	// Create a GitProvider in the fake client pointing to our empty repo
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
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

	// Test GetBranchMetadata - branch should still be unborn until first write/bootstrap
	exists, sha, fetchTime := worker.GetBranchMetadata()
	assert.False(t, exists, "Branch should not exist remotely for empty repository")
	assert.Empty(t, sha, "SHA should be empty while branch is unborn")
	assert.False(t, fetchTime.IsZero(), "Fetch time should be set")

	// Test ListResourcesInPath - should work with empty repo
	resources, err := worker.ListResourcesInPath("")
	require.NoError(t, err, "ListResourcesInPath should succeed with empty repository")
	assert.Empty(t, resources, "Should return empty resources list for empty repository")

	// Verify metadata was updated after ListResourcesInPath
	exists2, sha2, fetchTime2 := worker.GetBranchMetadata()
	assert.False(t, exists2, "Branch should remain unborn after listing")
	assert.Empty(t, sha2, "SHA should remain empty after listing")
	assert.False(t, fetchTime2.Before(fetchTime), "Fetch time should not move backwards")
}

// TestBranchWorker_IdentityFields verifies worker identity is set correctly.
func TestBranchWorker_IdentityFields(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "my-repo", "my-namespace", "develop", nil)

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

func TestBranchWorker_EnsurePathBootstrapped_EmptyPathCreatesTemplate(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))
	createTargetWithEncryption(ctx, t, k8sClient, "bootstrap-target", "default", "test-repo", "main", "clusters/prod")

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/prod", "bootstrap-target", "default"))
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/prod", "bootstrap-target", "default"))

	_, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "Bootstrap should not create the branch remotely")

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", sopsConfigFileName))
	require.NoError(t, err)
}

func TestBranchWorker_EnsurePathBootstrapped_NonEmptyPathBootstrapsMissingFiles(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)
	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	require.NoError(t, os.MkdirAll(filepath.Join(seedPath, "clusters/prod"), 0750))
	commitFileChange(t, seedWorktree, seedPath, "clusters/prod/existing.txt", "already populated")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))
	createTargetWithEncryption(ctx, t, k8sClient, "bootstrap-target", "default", "test-repo", "main", "clusters/prod")

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/prod", "bootstrap-target", "default"))
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/prod", "bootstrap-target", "default"))

	ref, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, 1, countDepth(t, serverRepo, ref.Hash()), "Bootstrap should not create a remote commit")

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", sopsConfigFileName))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", "existing.txt"))
	require.NoError(t, err)
}

func TestBranchWorker_EnsurePathBootstrapped_NoEncryptionSkipsSOPSConfig(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	_ = createBareRepo(t, remotePath)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))
	createTargetWithoutEncryption(ctx, t, k8sClient, "bootstrap-target", "default", "test-repo", "main", "clusters/dev")

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err := os.Stat(filepath.Join(repoPath, "clusters/dev", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	assert.True(t, os.IsNotExist(err), "Bootstrap SOPS config should be skipped when encryption is not configured")
}

func TestBranchWorker_EnsurePathBootstrapped_ExistingFileNotOverwritten(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	_ = createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	require.NoError(t, os.MkdirAll(filepath.Join(seedPath, "clusters/prod"), 0750))
	customREADME := "# custom readme\n"
	commitFileChange(t, seedWorktree, seedPath, "clusters/prod/README.md", customREADME)
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec("refs/heads/main:refs/heads/main"),
		},
	}))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))
	createTargetWithEncryption(ctx, t, k8sClient, "bootstrap-target", "default", "test-repo", "main", "clusters/prod")

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/prod", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	readmeContent, err := os.ReadFile(filepath.Join(repoPath, "clusters/prod", "README.md"))
	require.NoError(t, err)
	assert.Equal(t, customREADME, string(readmeContent), "Bootstrap must not overwrite existing files")
	_, err = os.Stat(filepath.Join(repoPath, "clusters/prod", sopsConfigFileName))
	require.NoError(t, err)
}

func TestBranchWorker_EnsurePathBootstrapped_EnableEncryptionLaterAddsSOPSConfig(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))
	createTargetWithoutEncryption(ctx, t, k8sClient, "bootstrap-target", "default", "test-repo", "main", "clusters/dev")

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err := os.Stat(filepath.Join(repoPath, "clusters/dev", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	assert.True(t, os.IsNotExist(err), "SOPS config should not exist before encryption is configured")

	attachEncryptionToTarget(ctx, t, k8sClient, "bootstrap-target", "default")
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	_, err = serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "Bootstrap should not create a remote commit")
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	require.NoError(t, err)
}

func TestBranchWorker_EnsurePathBootstrapped_InvalidEncryptionSecretSkipsSOPSConfig(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	_ = createBareRepo(t, remotePath)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	createTargetWithEncryptionSecretData(
		ctx,
		t,
		k8sClient,
		"bootstrap-target",
		"default",
		"test-repo",
		"main",
		"clusters/dev",
		map[string][]byte{
			"identity.agekey": []byte("not-an-age-identity"),
		},
	)

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err := os.Stat(filepath.Join(repoPath, "clusters/dev", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	assert.True(t, os.IsNotExist(err), "Bootstrap SOPS config should be skipped for invalid encryption secret")
}

func TestBranchWorker_EnsurePathBootstrapped_MissingSOPSKeySkipsSOPSConfig(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	_ = createBareRepo(t, remotePath)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	createTargetWithEncryptionSecretData(
		ctx,
		t,
		k8sClient,
		"bootstrap-target",
		"default",
		"test-repo",
		"main",
		"clusters/dev",
		map[string][]byte{
			"OTHER_ENV": []byte("value"),
		},
	)

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	_, err := os.Stat(filepath.Join(repoPath, "clusters/dev", "README.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	assert.True(t, os.IsNotExist(err), "Bootstrap SOPS config should be skipped when no .agekey entry is present")
}

func TestBranchWorker_EnsurePathBootstrapped_RendersAllResolvedRecipients(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	_ = createBareRepo(t, remotePath)

	secretIdentity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	secondaryIdentity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	publicOnlyIdentity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	encryptionSecret := &corev1.Secret{}
	encryptionSecret.Name = "sops-age-key"
	encryptionSecret.Namespace = "default"
	encryptionSecret.Data = map[string][]byte{
		"identity.agekey": []byte(secretIdentity.String()),
		"backup.agekey":   []byte(secondaryIdentity.String()),
	}
	require.NoError(t, k8sClient.Create(ctx, encryptionSecret))

	target := &configv1alpha1.GitTarget{}
	target.Name = "bootstrap-target"
	target.Namespace = "default"
	target.Spec.ProviderRef = configv1alpha1.GitProviderReference{
		Kind: "GitProvider",
		Name: "test-repo",
	}
	target.Spec.Branch = "main"
	target.Spec.Path = "clusters/dev"
	target.Spec.Encryption = &configv1alpha1.EncryptionSpec{
		Provider: "sops",
		SecretRef: configv1alpha1.LocalSecretReference{
			Name: encryptionSecret.Name,
		},
		Age: &configv1alpha1.AgeEncryptionSpec{
			Enabled: true,
			Recipients: configv1alpha1.AgeRecipientsSpec{
				PublicKeys: []string{
					publicOnlyIdentity.Recipient().String(),
				},
				ExtractFromSecret: true,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
	require.NoError(t, worker.EnsurePathBootstrapped("clusters/dev", "bootstrap-target", "default"))

	repoPath := worker.repoPathForRemote(remoteURL)
	sopsConfig, err := os.ReadFile(filepath.Join(repoPath, "clusters/dev", sopsConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(sopsConfig), secretIdentity.Recipient().String())
	assert.Contains(t, string(sopsConfig), secondaryIdentity.Recipient().String())
	assert.Contains(t, string(sopsConfig), publicOnlyIdentity.Recipient().String())
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
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil)
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
		CommitMode: CommitModePerEvent,
	}

	worker.commitAndPushRequest(request)

	localRepoPath := worker.repoPathForRemote(remoteURL)
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

	manifestPath := filepath.Join(localRepoPath, "clusters/dev", "v1", "configmaps", "default", "example.yaml")
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
	_ = configv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: remoteURL,
		},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "feature", nil)
	worker.ctx = ctx

	// Pre-create a stale local checkout while remote main is still at commit A.
	staleRepoPath := worker.repoPathForRemote(remoteURL)
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
		CommitMode: CommitModePerEvent,
	}

	worker.commitAndPushRequest(request)

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
		"v1",
		"configmaps",
		"default",
		"example-feature.yaml",
	)
	manifestContent, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Contains(t, string(manifestContent), "name: example-feature")
}

func createTargetWithEncryption(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name, namespace, providerName, branch, path string,
) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	encryptionSecret := &corev1.Secret{}
	encryptionSecret.Name = "sops-age-key"
	encryptionSecret.Namespace = namespace
	encryptionSecret.Data = map[string][]byte{
		"identity.agekey": []byte(identity.String()),
	}
	require.NoError(t, k8sClient.Create(ctx, encryptionSecret))

	target := &configv1alpha1.GitTarget{}
	target.Name = name
	target.Namespace = namespace
	target.Spec.ProviderRef = configv1alpha1.GitProviderReference{
		Kind: "GitProvider",
		Name: providerName,
	}
	target.Spec.Branch = branch
	target.Spec.Path = path
	target.Spec.Encryption = &configv1alpha1.EncryptionSpec{
		Provider: "sops",
		SecretRef: configv1alpha1.LocalSecretReference{
			Name: "sops-age-key",
		},
		Age: &configv1alpha1.AgeEncryptionSpec{
			Enabled: true,
			Recipients: configv1alpha1.AgeRecipientsSpec{
				ExtractFromSecret: true,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
}

func createTargetWithoutEncryption(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name, namespace, providerName, branch, path string,
) {
	t.Helper()
	target := &configv1alpha1.GitTarget{}
	target.Name = name
	target.Namespace = namespace
	target.Spec.ProviderRef = configv1alpha1.GitProviderReference{
		Kind: "GitProvider",
		Name: providerName,
	}
	target.Spec.Branch = branch
	target.Spec.Path = path
	require.NoError(t, k8sClient.Create(ctx, target))
}

func createTargetWithEncryptionSecretData(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	name, namespace, providerName, branch, path string,
	secretData map[string][]byte,
) {
	t.Helper()

	encryptionSecret := &corev1.Secret{}
	encryptionSecret.Name = "sops-age-key"
	encryptionSecret.Namespace = namespace
	encryptionSecret.Data = secretData
	require.NoError(t, k8sClient.Create(ctx, encryptionSecret))

	target := &configv1alpha1.GitTarget{}
	target.Name = name
	target.Namespace = namespace
	target.Spec.ProviderRef = configv1alpha1.GitProviderReference{
		Kind: "GitProvider",
		Name: providerName,
	}
	target.Spec.Branch = branch
	target.Spec.Path = path
	target.Spec.Encryption = &configv1alpha1.EncryptionSpec{
		Provider: "sops",
		SecretRef: configv1alpha1.LocalSecretReference{
			Name: encryptionSecret.Name,
		},
		Age: &configv1alpha1.AgeEncryptionSpec{
			Enabled: true,
			Recipients: configv1alpha1.AgeRecipientsSpec{
				ExtractFromSecret: true,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
}

func attachEncryptionToTarget(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	targetName, targetNamespace string,
) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	encryptionSecret := &corev1.Secret{}
	encryptionSecret.Name = "sops-age-key"
	encryptionSecret.Namespace = targetNamespace
	encryptionSecret.Data = map[string][]byte{
		"identity.agekey": []byte(identity.String()),
	}
	require.NoError(t, k8sClient.Create(ctx, encryptionSecret))

	target := &configv1alpha1.GitTarget{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: targetName, Namespace: targetNamespace}, target))
	target.Spec.Encryption = &configv1alpha1.EncryptionSpec{
		Provider: "sops",
		SecretRef: configv1alpha1.LocalSecretReference{
			Name: encryptionSecret.Name,
		},
		Age: &configv1alpha1.AgeEncryptionSpec{
			Enabled: true,
			Recipients: configv1alpha1.AgeRecipientsSpec{
				ExtractFromSecret: true,
			},
		},
	}
	require.NoError(t, k8sClient.Update(ctx, target))
}
