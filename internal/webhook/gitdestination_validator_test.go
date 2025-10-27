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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "removes .git suffix",
			input:    "https://github.com/org/repo.git",
			expected: "https://github.com/org/repo",
		},
		{
			name:     "removes trailing slash",
			input:    "https://github.com/org/repo/",
			expected: "https://github.com/org/repo",
		},
		{
			name:     "removes .git and trailing slash",
			input:    "https://github.com/org/repo.git/",
			expected: "https://github.com/org/repo",
		},
		{
			name:     "normalizes to lowercase",
			input:    "https://GitHub.com/Org/Repo",
			expected: "https://github.com/org/repo",
		},
		{
			name:     "handles SSH URLs",
			input:    "git@github.com:org/repo.git",
			expected: "git@github.com:org/repo",
		},
		{
			name:     "handles already normalized URLs",
			input:    "https://github.com/org/repo",
			expected: "https://github.com/org/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeRepoURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCreateDestinationIdentifier(t *testing.T) {
	// Test that identical inputs produce identical identifiers
	id1 := createDestinationIdentifier("https://github.com/org/repo", "main", "folder")
	id2 := createDestinationIdentifier("https://github.com/org/repo", "main", "folder")
	assert.Equal(t, id1, id2, "identical inputs should produce identical identifiers")

	// Test that different inputs produce different identifiers
	id3 := createDestinationIdentifier("https://github.com/org/repo", "main", "other-folder")
	assert.NotEqual(t, id1, id3, "different folders should produce different identifiers")

	id4 := createDestinationIdentifier("https://github.com/org/repo", "dev", "folder")
	assert.NotEqual(t, id1, id4, "different branches should produce different identifiers")

	id5 := createDestinationIdentifier("https://github.com/org/other-repo", "main", "folder")
	assert.NotEqual(t, id1, id5, "different repos should produce different identifiers")

	// Verify identifier is a valid hex string (SHA256 produces 64 hex chars)
	assert.Len(t, id1, 64, "SHA256 hash should be 64 hex characters")
}

func TestGitDestinationValidator_ValidateCreate_AllowsUnique(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitRepoConfig
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	// Create fake client with the repo config
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Create a new GitDestination
	dest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), dest)
	require.NoError(t, err, "should allow creation of unique destination")
	assert.Nil(t, warnings)
}

func TestGitDestinationValidator_ValidateCreate_RejectsDuplicate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitRepoConfig
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	// Create an existing GitDestination
	existingDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	// Create fake client with the repo config and existing destination
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig, existingDest).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Try to create a new GitDestination with same repo+branch+folder
	newDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod", // Same as existing
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), newDest)
	require.Error(t, err, "should reject creation of duplicate destination")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "GitDestination conflict detected")
	assert.Contains(t, err.Error(), "existing-dest")
}

func TestGitDestinationValidator_ValidateCreate_AllowsDifferentFolder(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitRepoConfig
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	// Create an existing GitDestination
	existingDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	// Create fake client
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig, existingDest).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Create a new GitDestination with different folder
	newDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/staging", // Different folder
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), newDest)
	require.NoError(t, err, "should allow creation with different folder")
	assert.Nil(t, warnings)
}

func TestGitDestinationValidator_ValidateUpdate_AllowsNonConflicting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitRepoConfig
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main", "dev"},
		},
	}

	// Create an existing GitDestination
	existingDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig, existingDest).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Update to a different branch
	updatedDest := existingDest.DeepCopy()
	updatedDest.Spec.Branch = "dev"

	warnings, err := validator.ValidateUpdate(context.Background(), existingDest, updatedDest)
	require.NoError(t, err, "should allow update to different branch")
	assert.Nil(t, warnings)
}

func TestGitDestinationValidator_ValidateUpdate_RejectsConflicting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitRepoConfig
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	// Create two existing GitDestinations
	dest1 := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dest-1",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	dest2 := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dest-2",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/staging",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig, dest1, dest2).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Try to update dest-2 to conflict with dest-1
	updatedDest2 := dest2.DeepCopy()
	updatedDest2.Spec.BaseFolder = "clusters/prod" // Conflicts with dest-1

	warnings, err := validator.ValidateUpdate(context.Background(), dest2, updatedDest2)
	require.Error(t, err, "should reject update that creates conflict")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "GitDestination conflict detected")
}

func TestGitDestinationValidator_ValidateDelete_AlwaysAllows(t *testing.T) {
	validator := &GitDestinationValidator{
		Client: fake.NewClientBuilder().Build(),
	}

	dest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dest",
			Namespace: "default",
		},
	}

	warnings, err := validator.ValidateDelete(context.Background(), dest)
	require.NoError(t, err, "deletion should always be allowed")
	assert.Nil(t, warnings)
}

func TestGitDestinationValidator_ValidateCreate_MissingGitRepoConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create fake client WITHOUT the repo config
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Create a GitDestination referencing non-existent repo
	dest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "missing-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), dest)
	require.Error(t, err, "should fail when GitRepoConfig not found")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "failed to resolve GitRepoConfig")
}

func TestGitDestinationValidator_CrossNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create GitRepoConfig in namespace-a
	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-repo",
			Namespace: "namespace-a",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	// Create existing destination in namespace-a
	existingDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dest-a",
			Namespace: "namespace-a",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name:      "shared-repo",
				Namespace: "namespace-a",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig, existingDest).
		Build()

	validator := &GitDestinationValidator{
		Client: fakeClient,
	}

	// Try to create destination in namespace-b that conflicts
	newDest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dest-b",
			Namespace: "namespace-b",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name:      "shared-repo",
				Namespace: "namespace-a", // Cross-namespace reference
			},
			Branch:     "main",
			BaseFolder: "clusters/prod", // Same as existing
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), newDest)
	require.Error(t, err, "should detect conflict across namespaces")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "GitDestination conflict detected")
}

func TestGitDestinationValidator_InvalidObject(t *testing.T) {
	validator := &GitDestinationValidator{
		Client: fake.NewClientBuilder().Build(),
	}

	// Pass wrong type of object
	invalidObj := &configbutleraiv1alpha1.GitRepoConfig{}

	warnings, err := validator.ValidateCreate(context.Background(), invalidObj)
	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "expected GitDestination")
}

func TestResolveNamespace(t *testing.T) {
	validator := &GitDestinationValidator{}

	tests := []struct {
		name              string
		refNamespace      string
		defaultNamespace  string
		expectedNamespace string
	}{
		{
			name:              "uses ref namespace when specified",
			refNamespace:      "custom-ns",
			defaultNamespace:  "default",
			expectedNamespace: "custom-ns",
		},
		{
			name:              "uses default when ref namespace is empty",
			refNamespace:      "",
			defaultNamespace:  "default",
			expectedNamespace: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validator.resolveNamespace(tt.refNamespace, tt.defaultNamespace)
			assert.Equal(t, tt.expectedNamespace, result)
		})
	}
}

// Mock client that returns errors for testing error paths.
type errorClient struct {
	client.Client

	getError  error
	listError error
}

func (c *errorClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if c.getError != nil {
		return c.getError
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *errorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.listError != nil {
		return c.listError
	}
	return c.Client.List(ctx, list, opts...)
}

func TestGitDestinationValidator_ListError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	repoConfig := &configbutleraiv1alpha1.GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-repo",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/org/repo",
			AllowedBranches: []string{"main"},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repoConfig).
		Build()

	// Wrap with error client that fails on List
	mockClient := &errorClient{
		Client:    baseClient,
		listError: assert.AnError,
	}

	validator := &GitDestinationValidator{
		Client: mockClient,
	}

	dest := &configbutleraiv1alpha1.GitDestination{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dest",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitDestinationSpec{
			RepoRef: configbutleraiv1alpha1.NamespacedName{
				Name: "test-repo",
			},
			Branch:     "main",
			BaseFolder: "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), dest)
	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "failed to list GitDestinations")
}
