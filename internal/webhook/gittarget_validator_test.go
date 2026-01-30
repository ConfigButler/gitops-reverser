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

func TestNormalizeRepoURL_Target(t *testing.T) {
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

func TestCreateTargetIdentifier(t *testing.T) {
	// Test that identical inputs produce identical identifiers
	id1 := createTargetIdentifier("https://github.com/org/repo", "main", "folder")
	id2 := createTargetIdentifier("https://github.com/org/repo", "main", "folder")
	assert.Equal(t, id1, id2, "identical inputs should produce identical identifiers")

	// Test that different inputs produce different identifiers
	id3 := createTargetIdentifier("https://github.com/org/repo", "main", "other-folder")
	assert.NotEqual(t, id1, id3, "different folders should produce different identifiers")

	id4 := createTargetIdentifier("https://github.com/org/repo", "dev", "folder")
	assert.NotEqual(t, id1, id4, "different branches should produce different identifiers")

	id5 := createTargetIdentifier("https://github.com/org/other-repo", "main", "folder")
	assert.NotEqual(t, id1, id5, "different repos should produce different identifiers")

	// Verify identifier is a valid hex string (SHA256 produces 64 hex chars)
	assert.Len(t, id1, 64, "SHA256 hash should be 64 hex characters")
}

func TestGitTargetValidator_ValidateCreate_AllowsUnique(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitProvider
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	// Create fake client with the provider
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Create a new GitTarget
	target := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), target)
	require.NoError(t, err, "should allow creation of unique target")
	assert.Nil(t, warnings)
}

func TestGitTargetValidator_ValidateCreate_RejectsDuplicate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitProvider
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	// Create an existing GitTarget
	existingTarget := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	// Create fake client with the provider and existing target
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, existingTarget).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Try to create a new GitTarget with same repo+branch+path
	newTarget := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod", // Same as existing
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), newTarget)
	require.Error(t, err, "should reject creation of duplicate target")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "GitTarget conflict detected")
	assert.Contains(t, err.Error(), "existing-target")
}

func TestGitTargetValidator_ValidateCreate_AllowsDifferentPath(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitProvider
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	// Create an existing GitTarget
	existingTarget := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	// Create fake client
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, existingTarget).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Create a new GitTarget with different path
	newTarget := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "new-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/staging", // Different path
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), newTarget)
	require.NoError(t, err, "should allow creation with different path")
	assert.Nil(t, warnings)
}

func TestGitTargetValidator_ValidateUpdate_AllowsNonConflicting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitProvider
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	// Create an existing GitTarget
	existingTarget := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, existingTarget).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Update to a different branch
	updatedTarget := existingTarget.DeepCopy()
	updatedTarget.Spec.Branch = "dev"

	warnings, err := validator.ValidateUpdate(context.Background(), existingTarget, updatedTarget)
	require.NoError(t, err, "should allow update to different branch")
	assert.Nil(t, warnings)
}

func TestGitTargetValidator_ValidateUpdate_RejectsConflicting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create a GitProvider
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	// Create two existing GitTargets
	target1 := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target-1",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	target2 := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target-2",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/staging",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, target1, target2).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Try to update target-2 to conflict with target-1
	updatedTarget2 := target2.DeepCopy()
	updatedTarget2.Spec.Path = "clusters/prod" // Conflicts with target-1

	warnings, err := validator.ValidateUpdate(context.Background(), target2, updatedTarget2)
	require.Error(t, err, "should reject update that creates conflict")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "GitTarget conflict detected")
}

func TestGitTargetValidator_ValidateDelete_AlwaysAllows(t *testing.T) {
	validator := &GitTargetValidator{
		Client: fake.NewClientBuilder().Build(),
	}

	target := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-target",
			Namespace: "default",
		},
	}

	warnings, err := validator.ValidateDelete(context.Background(), target)
	require.NoError(t, err, "deletion should always be allowed")
	assert.Nil(t, warnings)
}

func TestGitTargetValidator_ValidateCreate_MissingProvider(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	// Create fake client WITHOUT the provider
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	validator := &GitTargetValidator{
		Client: fakeClient,
	}

	// Create a GitTarget referencing non-existent provider
	target := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "missing-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), target)
	require.Error(t, err, "should fail when GitProvider not found")
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "failed to resolve provider")
}

func TestGitTargetValidator_NilObject(t *testing.T) {
	validator := &GitTargetValidator{
		Client: fake.NewClientBuilder().Build(),
	}

	t.Run("ValidateCreate", func(t *testing.T) {
		warnings, err := validator.ValidateCreate(context.Background(), nil)
		require.Error(t, err)
		assert.Nil(t, warnings)
	})

	t.Run("ValidateUpdate", func(t *testing.T) {
		warnings, err := validator.ValidateUpdate(context.Background(), nil, nil)
		require.Error(t, err)
		assert.Nil(t, warnings)
	})

	t.Run("ValidateDelete", func(t *testing.T) {
		warnings, err := validator.ValidateDelete(context.Background(), nil)
		require.Error(t, err)
		assert.Nil(t, warnings)
	})
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

func TestGitTargetValidator_ListError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = configbutleraiv1alpha1.AddToScheme(scheme)

	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			URL: "https://github.com/org/repo",
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider).
		Build()

	// Wrap with error client that fails on List
	mockClient := &errorClient{
		Client:    baseClient,
		listError: assert.AnError,
	}

	validator := &GitTargetValidator{
		Client: mockClient,
	}

	target := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-target",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{
				Name: "test-provider",
				Kind: "GitProvider",
			},
			Branch: "main",
			Path:   "clusters/prod",
		},
	}

	warnings, err := validator.ValidateCreate(context.Background(), target)
	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "failed to list GitTargets")
}
