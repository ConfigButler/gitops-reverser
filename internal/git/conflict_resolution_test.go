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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestTryPushCommits_Success(t *testing.T) {
	// This test focuses on the logic components since we can't easily test real git operations
	// We'll test the file path generation and event processing logic

	// Create test events
	events := []eventqueue.Event{
		{
			Object: createTestPod("test-pod", "default"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "test-pod",
			},
			Operation: "CREATE",
			UserInfo: eventqueue.UserInfo{
				Username: "test-user",
			},
			GitRepoConfigRef: "test-repo",
		},
	}

	// Test that we can generate the expected file paths
	for _, event := range events {
		filePath := event.Identifier.ToGitPath()
		expectedPath := "v1/pods/default/test-pod.yaml"
		assert.Equal(t, expectedPath, filePath)

		commitMessage := GetCommitMessage(event)
		expectedMessage := "[CREATE] v1/pods/test-pod by user/test-user"
		assert.Equal(t, expectedMessage, commitMessage)
	}
}

func TestIsNonFastForwardError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "non-fast-forward error",
			err:      assert.AnError,
			expected: false, // Our test error doesn't contain the keywords
		},
		{
			name:     "rejected error",
			err:      &mockError{msg: "updates were rejected because the remote contains work"},
			expected: true,
		},
		{
			name:     "non-fast-forward keyword",
			err:      &mockError{msg: "non-fast-forward"},
			expected: true,
		},
		{
			name:     "fetch first error",
			err:      &mockError{msg: "fetch first"},
			expected: true,
		},
		{
			name:     "other error",
			err:      &mockError{msg: "network error"},
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isNonFastForwardError(tc.err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestIsEventStillValid(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	repo := &Repo{
		path: tempDir,
	}

	ctx := context.Background()

	t.Run("file_does_not_exist", func(t *testing.T) {
		event := eventqueue.Event{
			Object: createTestPod("new-pod", "default"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "new-pod",
			},
			Operation: "CREATE",
		}

		valid := repo.isEventStillValid(ctx, event)
		assert.True(t, valid, "Event should be valid when file doesn't exist")
	})

	t.Run("file_exists_with_older_resource_version", func(t *testing.T) {
		// Create existing file with older resource version
		existingPod := createTestPod("existing-pod", "default")
		existingPod.SetResourceVersion("100")

		identifier := types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "existing-pod",
		}
		filePath := identifier.ToGitPath()
		fullPath := filepath.Join(tempDir, filePath)

		// Create directory and file
		err := os.MkdirAll(filepath.Dir(fullPath), 0750)
		require.NoError(t, err)

		content, err := yaml.Marshal(existingPod.Object)
		require.NoError(t, err)

		err = os.WriteFile(fullPath, content, 0600)
		require.NoError(t, err)

		// Create event with newer resource version
		newPod := createTestPod("existing-pod", "default")
		newPod.SetResourceVersion("200")

		event := eventqueue.Event{
			Object: newPod,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "existing-pod",
			},
			Operation: "UPDATE",
		}

		valid := repo.isEventStillValid(ctx, event)
		assert.True(t, valid, "Event should be valid when it has newer resource version")
	})

	t.Run("file_exists_with_newer_resource_version", func(t *testing.T) {
		// Create existing file with newer resource version
		existingPod := createTestPod("stale-pod", "default")
		existingPod.SetResourceVersion("300")

		identifier := types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "stale-pod",
		}
		filePath := identifier.ToGitPath()
		fullPath := filepath.Join(tempDir, filePath)

		// Create directory and file
		err := os.MkdirAll(filepath.Dir(fullPath), 0750)
		require.NoError(t, err)

		content, err := yaml.Marshal(existingPod.Object)
		require.NoError(t, err)

		err = os.WriteFile(fullPath, content, 0600)
		require.NoError(t, err)

		// Create event with older resource version
		oldPod := createTestPod("stale-pod", "default")
		oldPod.SetResourceVersion("200")

		event := eventqueue.Event{
			Object: oldPod,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "stale-pod",
			},
			Operation: "UPDATE",
		}

		valid := repo.isEventStillValid(ctx, event)
		assert.False(t, valid, "Event should be invalid when it has older resource version")
	})

	t.Run("file_exists_with_generation_comparison", func(t *testing.T) {
		// Create existing file with newer generation (no resource version to force generation comparison)
		existingPod := createTestPod("gen-pod", "default")
		existingPod.SetGeneration(5)
		// Explicitly clear resource version to ensure generation comparison is used
		existingPod.SetResourceVersion("")

		identifier := types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "gen-pod",
		}
		filePath := identifier.ToGitPath()
		fullPath := filepath.Join(tempDir, filePath)

		// Create directory and file
		err := os.MkdirAll(filepath.Dir(fullPath), 0750)
		require.NoError(t, err)

		content, err := yaml.Marshal(existingPod.Object)
		require.NoError(t, err)

		err = os.WriteFile(fullPath, content, 0600)
		require.NoError(t, err)

		// Create event with older generation (no resource version)
		oldPod := createTestPod("gen-pod", "default")
		oldPod.SetGeneration(3)
		oldPod.SetResourceVersion("") // Explicitly clear to force generation comparison

		event := eventqueue.Event{
			Object: oldPod,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "gen-pod",
			},
			Operation: "UPDATE",
		}

		valid := repo.isEventStillValid(ctx, event)
		assert.False(t, valid, "Event should be invalid when it has older generation")
	})

	t.Run("corrupted_existing_file", func(t *testing.T) {
		// Create corrupted file
		corruptedPath := filepath.Join(tempDir, "namespaces/default/Pod/corrupted-pod.yaml")
		err := os.MkdirAll(filepath.Dir(corruptedPath), 0750)
		require.NoError(t, err)

		err = os.WriteFile(corruptedPath, []byte("invalid yaml content {{{"), 0600)
		require.NoError(t, err)

		event := eventqueue.Event{
			Object: createTestPod("corrupted-pod", "default"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "corrupted-pod",
			},
			Operation: "CREATE",
		}

		valid := repo.isEventStillValid(ctx, event)
		assert.True(t, valid, "Event should be valid when existing file is corrupted")
	})
}

func TestReEvaluateEvents(t *testing.T) {
	// Create a temporary directory for the test
	tempDir := t.TempDir()

	repo := &Repo{
		path: tempDir,
	}

	ctx := context.Background()

	// Create some existing files to simulate Git state
	existingPod := createTestPod("existing-pod", "default")
	existingPod.SetResourceVersion("100")

	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "pods",
		Namespace: "default",
		Name:      "existing-pod",
	}
	filePath := identifier.ToGitPath()
	fullPath := filepath.Join(tempDir, filePath)

	err := os.MkdirAll(filepath.Dir(fullPath), 0750)
	require.NoError(t, err)

	content, err := yaml.Marshal(existingPod.Object)
	require.NoError(t, err)

	err = os.WriteFile(fullPath, content, 0600)
	require.NoError(t, err)

	// Create test events
	events := []eventqueue.Event{
		{
			Object: createTestPodWithResourceVersion("new-pod", "default", "200"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "new-pod",
			},
			Operation: "CREATE",
		},
		{
			Object: createTestPodWithResourceVersion("existing-pod", "default", "50"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "existing-pod",
			},
			Operation: "UPDATE",
		},
		{
			Object: createTestPodWithResourceVersion("existing-pod", "default", "150"),
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "existing-pod",
			},
			Operation: "UPDATE",
		},
	}

	validEvents := repo.reEvaluateEvents(ctx, events)
	assert.Len(t, validEvents, 2, "Should have 2 valid events")

	// Check that the stale event was filtered out
	names := make([]string, len(validEvents))
	for i, event := range validEvents {
		names[i] = event.Object.GetName()
	}
	assert.Contains(t, names, "new-pod")
	assert.Contains(t, names, "existing-pod") // The newer version should be kept
}

func TestConflictResolutionIntegration(t *testing.T) {
	// This test simulates the full conflict resolution workflow
	// Since we can't easily test with real Git operations, we test the logic components

	t.Run("conflict_resolution_workflow", func(t *testing.T) {
		tempDir := t.TempDir()

		repo := &Repo{
			path:       tempDir,
			branch:     "main",
			remoteName: "origin",
		}

		ctx := context.Background()

		// Simulate existing state in Git
		existingPod := createTestPodWithResourceVersion("conflict-pod", "default", "100")
		identifier := types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "conflict-pod",
		}
		filePath := identifier.ToGitPath()
		fullPath := filepath.Join(tempDir, filePath)

		err := os.MkdirAll(filepath.Dir(fullPath), 0750)
		require.NoError(t, err)

		content, err := yaml.Marshal(existingPod.Object)
		require.NoError(t, err)

		err = os.WriteFile(fullPath, content, 0600)
		require.NoError(t, err)

		// Create events that would conflict
		events := []eventqueue.Event{
			{
				Object: createTestPodWithResourceVersion("conflict-pod", "default", "50"),
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "default",
					Name:      "conflict-pod",
				},
				Operation: "UPDATE",
			},
			{
				Object: createTestPodWithResourceVersion("new-pod", "default", "200"),
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "default",
					Name:      "new-pod",
				},
				Operation: "CREATE",
			},
		}

		// Test re-evaluation (this is the core of conflict resolution)
		validEvents := repo.reEvaluateEvents(ctx, events)
		assert.Len(t, validEvents, 1, "Should filter out stale events")
		assert.Equal(t, "new-pod", validEvents[0].Object.GetName())
	})
}

// Helper functions

type mockError struct {
	msg string
}

func (e *mockError) Error() string {
	return e.msg
}

func TestErrNonFastForward(t *testing.T) {
	// Test that our custom error is properly defined
	require.Error(t, ErrNonFastForward)
	assert.Equal(t, "non-fast-forward push rejected", ErrNonFastForward.Error())
}

func TestToGitPath_ConsistencyWithConflictResolution(t *testing.T) {
	// Ensure ToGitPath works consistently for conflict resolution
	testCases := []struct {
		name           string
		namespace      string
		group          string
		resourcePlural string
		expected       string
	}{
		{
			name:           "test-pod",
			namespace:      "default",
			group:          "",
			resourcePlural: "pods",
			expected:       "v1/pods/default/test-pod.yaml",
		},
		{
			name:           "cluster-role",
			namespace:      "", // cluster-scoped
			group:          "rbac.authorization.k8s.io",
			resourcePlural: "clusterroles",
			expected:       "rbac.authorization.k8s.io/v1/clusterroles/cluster-role.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			identifier := types.ResourceIdentifier{
				Group:     tc.group,
				Version:   "v1",
				Resource:  tc.resourcePlural,
				Namespace: tc.namespace,
				Name:      tc.name,
			}
			path := identifier.ToGitPath()
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestCommitMessageGeneration(t *testing.T) {
	// Test commit message generation for conflict resolution scenarios
	pod := createTestPod("test-pod", "default")

	event := eventqueue.Event{
		Object: pod,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "UPDATE",
		UserInfo: eventqueue.UserInfo{
			Username: "conflict-resolver",
		},
		GitRepoConfigRef: "test-repo",
	}

	message := GetCommitMessage(event)
	expected := "[UPDATE] v1/pods/test-pod by user/conflict-resolver"
	assert.Equal(t, expected, message)
}
