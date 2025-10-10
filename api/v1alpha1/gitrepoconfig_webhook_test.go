/*
Copyright 2025.

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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGitRepoConfig_ValidateCreate(t *testing.T) {
	tests := []struct {
		name        string
		config      *GitRepoConfig
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid - no access policy",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
				},
			},
			expectError: false,
		},
		{
			name: "valid - AllNamespaces mode",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
					AccessPolicy: &AccessPolicy{
						NamespacedRules: &NamespacedRulesPolicy{
							Mode: AccessPolicyModeAllNamespaces,
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid - FromSelector with selector",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
					AccessPolicy: &AccessPolicy{
						NamespacedRules: &NamespacedRulesPolicy{
							Mode: AccessPolicyModeFromSelector,
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"team": "platform",
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid - selector without FromSelector mode",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
					AccessPolicy: &AccessPolicy{
						NamespacedRules: &NamespacedRulesPolicy{
							Mode: AccessPolicyModeAllNamespaces,
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"team": "platform",
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "namespaceSelector can only be set when mode is 'FromSelector'",
		},
		{
			name: "invalid - FromSelector without selector",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
					AccessPolicy: &AccessPolicy{
						NamespacedRules: &NamespacedRulesPolicy{
							Mode: AccessPolicyModeFromSelector,
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "namespaceSelector is required when mode is 'FromSelector'",
		},
		{
			name: "invalid - malformed label selector",
			config: &GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: GitRepoConfigSpec{
					RepoURL: "git@github.com:test/repo.git",
					Branch:  "main",
					AccessPolicy: &AccessPolicy{
						NamespacedRules: &NamespacedRulesPolicy{
							Mode: AccessPolicyModeFromSelector,
							NamespaceSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "invalid-key!@#",
										Operator: "InvalidOperator",
									},
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "invalid namespaceSelector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.config.ValidateCreate()
			if tt.expectError {
				require.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGitRepoConfig_ValidateUpdate(t *testing.T) {
	config := &GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: GitRepoConfigSpec{
			RepoURL: "git@github.com:test/repo.git",
			Branch:  "main",
			AccessPolicy: &AccessPolicy{
				NamespacedRules: &NamespacedRulesPolicy{
					Mode: AccessPolicyModeFromSelector,
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"team": "platform",
						},
					},
				},
			},
		},
	}

	_, err := config.ValidateUpdate(nil)
	assert.NoError(t, err)
}

func TestGitRepoConfig_ValidateDelete(t *testing.T) {
	config := &GitRepoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: GitRepoConfigSpec{
			RepoURL: "git@github.com:test/repo.git",
			Branch:  "main",
		},
	}

	_, err := config.ValidateDelete()
	assert.NoError(t, err)
}
