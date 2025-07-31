package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/yaml"
)

// TestRaceConditionIntegration tests the complete race condition resolution workflow
// This test simulates a real-world scenario where:
// 1. Multiple events are queued for commit
// 2. The remote repository is updated by another process (simulating race condition)
// 3. The push fails with non-fast-forward error
// 4. The system performs conflict resolution and retries
func TestRaceConditionIntegration(t *testing.T) {
	// Create temporary directories for local and "remote" repositories
	tempDir, err := os.MkdirTemp("", "race-condition-integration-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	localRepoPath := filepath.Join(tempDir, "local")
	remoteRepoPath := filepath.Join(tempDir, "remote")

	// Initialize "remote" repository (bare repository)
	_, err = git.PlainInit(remoteRepoPath, true)
	require.NoError(t, err)

	// Initialize local repository first, then add remote
	localRepo, err := git.PlainInit(localRepoPath, false)
	require.NoError(t, err)

	// Add remote origin
	_, err = localRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteRepoPath},
	})
	require.NoError(t, err)

	// Create our Repo wrapper
	repo := &Repo{
		Repository: localRepo,
		path:       localRepoPath,
		auth:       nil, // No auth needed for local test
		branch:     "main",
		remoteName: "origin",
	}

	ctx := context.Background()

	t.Run("full_race_condition_workflow", func(t *testing.T) {
		// Step 1: Create initial commit in local repo to establish main branch
		err := createInitialCommit(localRepo, localRepoPath)
		require.NoError(t, err)

		// Push initial commit to remote
		err = localRepo.Push(&git.PushOptions{})
		require.NoError(t, err)

		// Step 2: Prepare events to be committed
		events := []eventqueue.Event{
			{
				Object: createTestPodWithResourceVersion("app-pod", "production", "100"),
				Request: admission.Request{
					AdmissionRequest: admissionv1.AdmissionRequest{
						Operation: admissionv1.Create,
						UserInfo: authenticationv1.UserInfo{
							Username: "developer@company.com",
						},
					},
				},
				GitRepoConfigRef: "production-repo",
			},
			{
				Object: createTestPodWithResourceVersion("cache-pod", "production", "200"),
				Request: admission.Request{
					AdmissionRequest: admissionv1.AdmissionRequest{
						Operation: admissionv1.Update,
						UserInfo: authenticationv1.UserInfo{
							Username: "system:deployment-controller",
						},
					},
				},
				GitRepoConfigRef: "production-repo",
			},
		}

		// Step 3: Simulate another process updating the remote repository
		// This creates the race condition scenario
		err = simulateRemoteUpdate(remoteRepoPath, localRepoPath)
		require.NoError(t, err)

		// Step 4: Attempt to push commits - this should trigger race condition resolution
		err = repo.TryPushCommits(ctx, events)
		
		// The operation should succeed after conflict resolution
		assert.NoError(t, err, "TryPushCommits should succeed after conflict resolution")

		// Step 5: Verify the final state
		// Check that files were created correctly
		appPodPath := filepath.Join(localRepoPath, "namespaces/production/Pod/app-pod.yaml")
		cachePodPath := filepath.Join(localRepoPath, "namespaces/production/Pod/cache-pod.yaml")
		conflictingFilePath := filepath.Join(localRepoPath, "namespaces/production/Service/conflicting-service.yaml")

		// All files should exist
		assert.FileExists(t, appPodPath, "app-pod.yaml should exist")
		assert.FileExists(t, cachePodPath, "cache-pod.yaml should exist")
		assert.FileExists(t, conflictingFilePath, "conflicting-service.yaml should exist from remote update")

		// Verify file contents
		appPodContent, err := os.ReadFile(appPodPath)
		require.NoError(t, err)
		assert.Contains(t, string(appPodContent), "name: app-pod")
		assert.Contains(t, string(appPodContent), "resourceVersion: \"100\"")

		cachePodContent, err := os.ReadFile(cachePodPath)
		require.NoError(t, err)
		assert.Contains(t, string(cachePodContent), "name: cache-pod")
		assert.Contains(t, string(cachePodContent), "resourceVersion: \"200\"")

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
		
		assert.Contains(t, commitMessages, "[CREATE] Pod/app-pod in ns/production by user/developer@company.com")
		assert.Contains(t, commitMessages, "[UPDATE] Pod/cache-pod in ns/production by user/system:deployment-controller")
	})

	t.Run("stale_event_filtering", func(t *testing.T) {
		// Reset repository state
		err := resetToRemote(repo)
		require.NoError(t, err)

		// Create a file with newer resource version in the repository
		newerPod := createTestPodWithResourceVersion("existing-pod", "default", "500")
		filePath := GetFilePath(newerPod)
		fullPath := filepath.Join(localRepoPath, filePath)
		
		err = os.MkdirAll(filepath.Dir(fullPath), 0755)
		require.NoError(t, err)
		
		content, err := yaml.Marshal(newerPod.Object)
		require.NoError(t, err)
		
		err = os.WriteFile(fullPath, content, 0644)
		require.NoError(t, err)

		// Commit this file
		worktree, err := localRepo.Worktree()
		require.NoError(t, err)
		
		_, err = worktree.Add(filePath)
		require.NoError(t, err)
		
		_, err = worktree.Commit("Add existing pod with newer version", &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now(),
			},
		})
		require.NoError(t, err)

		// Push to remote
		err = localRepo.Push(&git.PushOptions{})
		require.NoError(t, err)

		// Now create events with mixed staleness
		events := []eventqueue.Event{
			{
				Object: createTestPodWithResourceVersion("existing-pod", "default", "300"), // Stale - should be filtered
			},
			{
				Object: createTestPodWithResourceVersion("existing-pod", "default", "600"), // Newer - should be kept
			},
			{
				Object: createTestPodWithResourceVersion("new-pod", "default", "100"), // New - should be kept
			},
		}

		// Simulate remote update to trigger conflict resolution
		err = simulateRemoteUpdate(remoteRepoPath, localRepoPath)
		require.NoError(t, err)

		// Try to push - should succeed with filtered events
		err = repo.TryPushCommits(ctx, events)
		assert.NoError(t, err)

		// Verify that only non-stale events were committed
		existingPodContent, err := os.ReadFile(fullPath)
		require.NoError(t, err)
		
		// Should have the newer version (600), not the stale one (300)
		assert.Contains(t, string(existingPodContent), "resourceVersion: \"600\"")
		assert.NotContains(t, string(existingPodContent), "resourceVersion: \"300\"")

		// New pod should exist
		newPodPath := filepath.Join(localRepoPath, "namespaces/default/Pod/new-pod.yaml")
		assert.FileExists(t, newPodPath)
	})

	t.Run("error_handling", func(t *testing.T) {
		// Test various error scenarios
		
		// Test with empty events
		err := repo.TryPushCommits(ctx, []eventqueue.Event{})
		assert.NoError(t, err, "Should handle empty events gracefully")

		// Test with invalid object (this should be handled gracefully)
		invalidEvent := eventqueue.Event{
			Object: &unstructured.Unstructured{}, // Empty object
			Request: admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: admissionv1.Create,
					UserInfo: authenticationv1.UserInfo{
						Username: "test-user",
					},
				},
			},
		}

		// This might fail, but shouldn't panic
		_ = repo.TryPushCommits(ctx, []eventqueue.Event{invalidEvent})
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
	err = os.WriteFile(readmePath, []byte("# GitOps Reverser Repository\n"), 0644)
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

func simulateRemoteUpdate(remoteRepoPath, localRepoPath string) error {
	// Clone the remote repository to a temporary location
	tempClonePath := remoteRepoPath + "-temp-clone"
	defer os.RemoveAll(tempClonePath)

	tempRepo, err := git.PlainClone(tempClonePath, false, &git.CloneOptions{
		URL: remoteRepoPath,
	})
	if err != nil {
		return err
	}

	// Create a conflicting change
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

	filePath := GetFilePath(conflictingService)
	fullPath := filepath.Join(tempClonePath, filePath)

	err = os.MkdirAll(filepath.Dir(fullPath), 0755)
	if err != nil {
		return err
	}

	content, err := yaml.Marshal(conflictingService.Object)
	if err != nil {
		return err
	}

	err = os.WriteFile(fullPath, content, 0644)
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

	return tempRepo.Push(&git.PushOptions{})
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

func resetToRemote(repo *Repo) error {
	return repo.hardResetToRemote(context.Background())
}

// TestRaceConditionEdgeCases tests edge cases in race condition handling
func TestRaceConditionEdgeCases(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "race-condition-edge-cases-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	repo := &Repo{
		path: tempDir,
	}

	ctx := context.Background()

	t.Run("corrupted_git_state", func(t *testing.T) {
		// Create a file that looks like a YAML but is corrupted
		corruptedPath := filepath.Join(tempDir, "namespaces/default/Pod/corrupted.yaml")
		err := os.MkdirAll(filepath.Dir(corruptedPath), 0755)
		require.NoError(t, err)

		err = os.WriteFile(corruptedPath, []byte("invalid: yaml: content: {{{"), 0644)
		require.NoError(t, err)

		event := eventqueue.Event{
			Object: createTestPod("corrupted", "default"),
		}

		// Should handle corrupted files gracefully
		valid, err := repo.isEventStillValid(ctx, event)
		assert.NoError(t, err)
		assert.True(t, valid, "Should consider event valid when existing file is corrupted")
	})

	t.Run("resource_version_parsing_edge_cases", func(t *testing.T) {
		testCases := []struct {
			name           string
			resourceVersion string
			expectError    bool
		}{
			{"empty_version", "", true},
			{"valid_number", "123", false},
			{"invalid_string", "abc", true},
			{"negative_number", "-1", false}, // Technically valid int64
			{"very_large_number", "9223372036854775807", false}, // Max int64
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := parseResourceVersion(tc.resourceVersion)
				if tc.expectError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			})
		}
	})

	t.Run("generation_comparison_edge_cases", func(t *testing.T) {
		// Test generation comparison with various metadata formats
		pod := createTestPod("gen-test", "default")
		
		// Test with generation as different types in metadata
		testCases := []struct {
			name       string
			generation interface{}
			expected   int64
		}{
			{"int64_generation", int64(5), 5},
			{"int_generation", int(3), 3},
			{"float64_generation", float64(7), 7},
			{"string_generation", "invalid", 0}, // Should default to 0
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Set generation in metadata manually
				metadata := pod.Object["metadata"].(map[string]interface{})
				metadata["generation"] = tc.generation

				filePath := GetFilePath(pod)
				fullPath := filepath.Join(tempDir, filePath)
				
				err := os.MkdirAll(filepath.Dir(fullPath), 0755)
				require.NoError(t, err)
				
				content, err := yaml.Marshal(pod.Object)
				require.NoError(t, err)
				
				err = os.WriteFile(fullPath, content, 0644)
				require.NoError(t, err)

				// Create event with lower generation
				eventPod := createTestPod("gen-test", "default")
				eventPod.SetGeneration(1)
				eventPod.SetResourceVersion("") // Force generation comparison

				event := eventqueue.Event{
					Object: eventPod,
				}

				valid, err := repo.isEventStillValid(ctx, event)
				assert.NoError(t, err)
				
				if tc.expected > 1 {
					assert.False(t, valid, "Event should be invalid when existing has higher generation")
				} else {
					assert.True(t, valid, "Event should be valid when existing has lower/equal generation")
				}
			})
		}
	})
}

// TestRaceConditionPerformance tests performance aspects of race condition handling
func TestRaceConditionPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	tempDir, err := os.MkdirTemp("", "race-condition-perf-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	repo := &Repo{
		path: tempDir,
	}

	ctx := context.Background()

	t.Run("large_event_batch_re_evaluation", func(t *testing.T) {
		// Create many existing files
		const numExistingFiles = 100
		for i := 0; i < numExistingFiles; i++ {
			pod := createTestPodWithResourceVersion(fmt.Sprintf("existing-pod-%d", i), "default", "100")
			filePath := GetFilePath(pod)
			fullPath := filepath.Join(tempDir, filePath)
			
			err := os.MkdirAll(filepath.Dir(fullPath), 0755)
			require.NoError(t, err)
			
			content, err := yaml.Marshal(pod.Object)
			require.NoError(t, err)
			
			err = os.WriteFile(fullPath, content, 0644)
			require.NoError(t, err)
		}

		// Create many events to re-evaluate
		const numEvents = 200
		events := make([]eventqueue.Event, numEvents)
		for i := 0; i < numEvents; i++ {
			var resourceVersion string
			if i < numExistingFiles {
				// Half will be stale (older version)
				if i%2 == 0 {
					resourceVersion = "50" // Stale
				} else {
					resourceVersion = "150" // Newer
				}
			} else {
				resourceVersion = "200" // New files
			}

			events[i] = eventqueue.Event{
				Object: createTestPodWithResourceVersion(fmt.Sprintf("existing-pod-%d", i), "default", resourceVersion),
			}
		}

		// Measure re-evaluation performance
		start := time.Now()
		validEvents, err := repo.reEvaluateEvents(ctx, events)
		duration := time.Since(start)

		assert.NoError(t, err)
		assert.Less(t, len(validEvents), numEvents, "Should filter out some stale events")
		assert.Less(t, duration, 5*time.Second, "Re-evaluation should complete within reasonable time")

		t.Logf("Re-evaluated %d events in %v, %d valid events remaining", 
			numEvents, duration, len(validEvents))
	})
}