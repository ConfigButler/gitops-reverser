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

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestRaceConditionIntegration tests the complete race condition resolution workflow
// This test simulates a real-world scenario where:
// 1. Multiple events are queued for commit
// 2. The remote repository is updated by another process (simulating race condition)
// 3. The push fails with non-fast-forward error
// 4. The system performs conflict resolution and retries.
func TestRaceConditionIntegration(t *testing.T) {
	// Create temporary directories for local and "remote" repositories
	tempDir := t.TempDir()

	localRepoPath := filepath.Join(tempDir, "local")
	remoteRepoPath := filepath.Join(tempDir, "remote")

	// Initialize "remote" repository as bare
	_, err := git.PlainInit(remoteRepoPath, true) // bare repo
	require.NoError(t, err)

	// Create a temp repo to initialize the remote
	tempRepoPath := filepath.Join(tempDir, "temp")
	tempRepo, err := git.PlainInit(tempRepoPath, false)
	require.NoError(t, err)

	err = createInitialCommit(tempRepo, tempRepoPath)
	require.NoError(t, err)

	// Push to remote
	_, err = tempRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteRepoPath},
	})
	require.NoError(t, err)

	err = tempRepo.Push(&git.PushOptions{
		RemoteName: "origin",
	})
	require.NoError(t, err)

	// Now clone the 'local' repo from the remote
	localRepo, err := git.PlainClone(localRepoPath, false, &git.CloneOptions{
		URL: remoteRepoPath,
	})
	require.NoError(t, err)
	repo := &Repo{
		Repository: localRepo,
		path:       localRepoPath,
		auth:       nil, // No auth needed for local test
		branch:     "main",
		remoteName: "origin",
	}

	ctx := context.Background()

	t.Run("full_race_condition_workflow", func(t *testing.T) {
		// Step 2: Prepare events to be committed
		events := []Event{
			{
				Object: createTestPodWithResourceVersion("app-pod", "production", "100"),
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "production",
					Name:      "app-pod",
				},
				Operation: "CREATE",
				UserInfo: UserInfo{
					Username: "developer@company.com",
				},
				BaseFolder: "",
			},
			{
				Object: createTestPodWithResourceVersion("cache-pod", "production", "200"),
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "production",
					Name:      "cache-pod",
				},
				Operation: "UPDATE",
				UserInfo: UserInfo{
					Username: "system:deployment-controller",
				},
				BaseFolder: "",
			},
		}

		// Step 3: Simulate another process updating the remote repository
		// This creates the race condition scenario
		err = simulateRemoteUpdate(remoteRepoPath)
		require.NoError(t, err)

		// Step 4: Attempt to push commits - this should trigger race condition resolution
		result, err := WriteEvents(ctx, repo.path, "main", "main", events, repo.auth)

		// The operation should succeed after conflict resolution
		require.NoError(t, err, "WriteEvents should succeed after conflict resolution")
		require.Equal(t, 2, result.CommitsCreated)
		require.GreaterOrEqual(t, result.Failures, 1)
		require.GreaterOrEqual(t, len(result.ConflictPulls), 1)

		// Step 5: Verify the final state
		// Check that files were created correctly
		appPodPath := filepath.Join(localRepoPath, "v1/pods/production/app-pod.yaml")
		cachePodPath := filepath.Join(localRepoPath, "v1/pods/production/cache-pod.yaml")
		conflictingFilePath := filepath.Join(localRepoPath, "v1/services/production/conflicting-service.yaml")

		// All files should exist
		assert.FileExists(t, appPodPath, "app-pod.yaml should exist")
		assert.FileExists(t, cachePodPath, "cache-pod.yaml should exist")
		assert.FileExists(t, conflictingFilePath, "conflicting-service.yaml should exist from remote update")

		// Verify file contents
		appPodContent, err := os.ReadFile(appPodPath)
		require.NoError(t, err)
		assert.Contains(t, string(appPodContent), "name: app-pod")
		assert.Contains(t, string(appPodContent), "namespace: production")
		// resourceVersion is correctly removed by sanitization

		cachePodContent, err := os.ReadFile(cachePodPath)
		require.NoError(t, err)
		assert.Contains(t, string(cachePodContent), "name: cache-pod")
		assert.Contains(t, string(cachePodContent), "namespace: production")
		// resourceVersion is correctly removed by sanitization

		// Step 6: Verify Git history
		commits, err := getCommitHistory(localRepo)
		require.NoError(t, err)

		// Should have at least: initial commit + remote update + our 2 commits
		assert.GreaterOrEqual(t, len(commits), 4, "Should have multiple commits in history")

		// Check that our commit messages are present
		commitMessages := make([]string, len(commits))
		for i, commit := range commits {
			commitMessages[i] = commit.Message
		}

		assert.Contains(t, commitMessages, "[CREATE] v1/pods/app-pod by user/developer@company.com")
		assert.Contains(
			t,
			commitMessages,
			"[UPDATE] v1/pods/cache-pod by user/system:deployment-controller",
		)
	})

	t.Run("error_handling", func(t *testing.T) {
		// Test various error scenarios

		// Test with empty events
		_, err := WriteEvents(ctx, repo.path, "main", "main", []Event{}, repo.auth)
		require.NoError(t, err, "Should handle empty events gracefully")

		// Test with invalid object (this should be handled gracefully)
		invalidEvent := Event{
			Object: &unstructured.Unstructured{}, // Empty object
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "testresources",
				Namespace: "default",
				Name:      "test",
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
			},
		}

		// This might fail, but shouldn't panic
		_, err = WriteEvents(ctx, repo.path, "main", "main", []Event{invalidEvent}, repo.auth)
		_ = err
	})
}

// Helper functions

func createInitialCommit(repo *git.Repository, repoPath string) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}

	// Create initial README
	readmePath := filepath.Join(repoPath, "README.md")
	err = os.WriteFile(readmePath, []byte("# GitOps Reverser Repository\n"), 0600)
	if err != nil {
		return err
	}

	_, err = worktree.Add("README.md")
	if err != nil {
		return err
	}

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "GitOps Reverser",
			Email: "gitops-reverser@configbutler.ai",
			When:  time.Now(),
		},
	})
	return err
}

func simulateRemoteUpdate(remoteRepoPath string) error {
	// Create a temp repo to simulate remote update
	tempRepoPath := filepath.Join(filepath.Dir(remoteRepoPath), "temp-remote")
	tempRepo, err := git.PlainClone(tempRepoPath, false, &git.CloneOptions{
		URL: remoteRepoPath,
	})
	if err != nil {
		return err
	}

	conflictingService := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":            "conflicting-service",
				"namespace":       "production",
				"resourceVersion": "999",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "conflicting-app",
				},
				"ports": []interface{}{
					map[string]interface{}{
						"port":       80,
						"targetPort": 8080,
					},
				},
			},
		},
	}

	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "services",
		Namespace: "production",
		Name:      "conflicting-service",
	}
	filePath := identifier.ToGitPath()
	fullPath := filepath.Join(tempRepoPath, filePath)

	err = os.MkdirAll(filepath.Dir(fullPath), 0750)
	if err != nil {
		return err
	}

	content, err := yaml.Marshal(conflictingService.Object)
	if err != nil {
		return err
	}

	err = os.WriteFile(fullPath, content, 0600)
	if err != nil {
		return err
	}

	// Commit and push the change
	worktree, err := tempRepo.Worktree()
	if err != nil {
		return err
	}

	_, err = worktree.Add(filePath)
	if err != nil {
		return err
	}

	_, err = worktree.Commit("Add conflicting service (simulating remote update)", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Another Process",
			Email: "another@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		return err
	}

	// Push to remote
	err = tempRepo.Push(&git.PushOptions{
		RemoteName: "origin",
	})
	return err
}

func getCommitHistory(repo *git.Repository) ([]*object.Commit, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}

	commitIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}
	defer commitIter.Close()

	var commits []*object.Commit
	err = commitIter.ForEach(func(c *object.Commit) error {
		commits = append(commits, c)
		return nil
	})

	return commits, err
}
