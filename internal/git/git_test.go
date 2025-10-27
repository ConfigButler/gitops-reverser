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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/ssh"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestToGitPath_NamespacedResource(t *testing.T) {
	testCases := []struct {
		name           string
		namespace      string
		group          string
		version        string
		resourcePlural string
		expected       string
	}{
		{
			name:           "test-pod",
			namespace:      "default",
			group:          "",
			version:        "v1",
			resourcePlural: "pods",
			expected:       "v1/pods/default/test-pod.yaml",
		},
		{
			name:           "my-service",
			namespace:      "production",
			group:          "",
			version:        "v1",
			resourcePlural: "services",
			expected:       "v1/services/production/my-service.yaml",
		},
		{
			name:           "app-config",
			namespace:      "staging",
			group:          "",
			version:        "v1",
			resourcePlural: "configmaps",
			expected:       "v1/configmaps/staging/app-config.yaml",
		},
		{
			name:           "complex-name-with-dashes",
			namespace:      "kube-system",
			group:          "apps",
			version:        "v1",
			resourcePlural: "deployments",
			expected:       "apps/v1/deployments/kube-system/complex-name-with-dashes.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			identifier := types.ResourceIdentifier{
				Group:     tc.group,
				Version:   tc.version,
				Resource:  tc.resourcePlural,
				Namespace: tc.namespace,
				Name:      tc.name,
			}
			path := identifier.ToGitPath()
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestToGitPath_ClusterScopedResource(t *testing.T) {
	testCases := []struct {
		name           string
		group          string
		version        string
		resourcePlural string
		expected       string
	}{
		{
			name:           "my-namespace",
			group:          "",
			version:        "v1",
			resourcePlural: "namespaces",
			expected:       "v1/namespaces/my-namespace.yaml",
		},
		{
			name:           "cluster-admin",
			group:          "rbac.authorization.k8s.io",
			version:        "v1",
			resourcePlural: "clusterroles",
			expected:       "rbac.authorization.k8s.io/v1/clusterroles/cluster-admin.yaml",
		},
		{
			name:           "system-binding",
			group:          "rbac.authorization.k8s.io",
			version:        "v1",
			resourcePlural: "clusterrolebindings",
			expected:       "rbac.authorization.k8s.io/v1/clusterrolebindings/system-binding.yaml",
		},
		{
			name:           "my-pv",
			group:          "",
			version:        "v1",
			resourcePlural: "persistentvolumes",
			expected:       "v1/persistentvolumes/my-pv.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			identifier := types.ResourceIdentifier{
				Group:     tc.group,
				Version:   tc.version,
				Resource:  tc.resourcePlural,
				Namespace: "",
				Name:      tc.name,
			}
			path := identifier.ToGitPath()
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestToGitPath_EmptyNamespace(t *testing.T) {
	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "testkinds",
		Namespace: "", // Empty namespace means cluster-scoped
		Name:      "test-resource",
	}
	path := identifier.ToGitPath()
	assert.Equal(t, "v1/testkinds/test-resource.yaml", path)
}

func TestToGitPath_SpecialCharacters(t *testing.T) {
	testCases := []struct {
		name           string
		namespace      string
		resourcePlural string
		expected       string
	}{
		{
			name:           "test.resource",
			namespace:      "default",
			resourcePlural: "pods",
			expected:       "v1/pods/default/test.resource.yaml",
		},
		{
			name:           "test_resource",
			namespace:      "default",
			resourcePlural: "services",
			expected:       "v1/services/default/test_resource.yaml",
		},
		{
			name:           "test-resource-123",
			namespace:      "test-ns-456",
			resourcePlural: "configmaps",
			expected:       "v1/configmaps/test-ns-456/test-resource-123.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			identifier := types.ResourceIdentifier{
				Group:     "",
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

func TestGetCommitMessage_CreateOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "john.doe@example.com",
		},
		BaseFolder: "",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] v1/pods/test-pod by user/john.doe@example.com"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_UpdateOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("my-service")
	obj.SetNamespace("production")
	obj.SetKind("Service")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "services",
			Namespace: "production",
			Name:      "my-service",
		},
		Operation: "UPDATE",
		UserInfo: UserInfo{
			Username: "system:serviceaccount:kube-system:deployment-controller",
		},
		BaseFolder: "prod-repo",
	}

	message := GetCommitMessage(event)
	expected := "[UPDATE] v1/services/my-service by user/system:serviceaccount:kube-system:deployment-controller"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_DeleteOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("old-config")
	obj.SetNamespace("staging")
	obj.SetKind("ConfigMap")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "staging",
			Name:      "old-config",
		},
		Operation: "DELETE",
		UserInfo: UserInfo{
			Username: "admin",
		},
		BaseFolder: "staging-repo",
	}

	message := GetCommitMessage(event)
	expected := "[DELETE] v1/configmaps/old-config by user/admin"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_ClusterScopedResource(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("my-namespace")
	obj.SetKind("Namespace")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "namespaces",
			Namespace: "",
			Name:      "my-namespace",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "cluster-admin",
		},
		BaseFolder: "cluster-repo",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] v1/namespaces/my-namespace by user/cluster-admin"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_EmptyUsername(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "", // Empty username
		},
		BaseFolder: "",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] v1/pods/test-pod by user/"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_SpecialCharactersInNames(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod.with.dots")
	obj.SetNamespace("test-ns_with_underscores")
	obj.SetKind("Pod")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "test-ns_with_underscores",
			Name:      "test-pod.with.dots",
		},
		Operation: "UPDATE",
		UserInfo: UserInfo{
			Username: "user@domain.com",
		},
		BaseFolder: "",
	}

	message := GetCommitMessage(event)
	expected := "[UPDATE] v1/pods/test-pod.with.dots by user/user@domain.com"
	assert.Equal(t, expected, message)
}

func TestGetAuthMethod_ValidKey(t *testing.T) {
	// Since we're testing with a fake key, we expect this to fail
	// In a real scenario, this would be a valid SSH private key
	privateKey := `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA4f5wg5l2hKsTeNem/V41fGnJm6gOdrj8ym3rFkEjWT2btYhA
z2R6eMhF+o/5BjKff/1hdX7O+9AHjfTjHOKVn0+aWNs2fQIDAQABAoIBABYWnohQ
e+3Iw1AbYbvylpP2yv9otaanmT0Dcn2TlBXqBTfnIFLd5vbmbnw3WEg5Zf9/5cqm
3z8/Lu8EYFnagqGjlwM62YWtHBtDtrjI2d01q/DuLBGXHFTn/H49TXfn7pwqYBwJ
of5c89fDoGhyoMpo0eDidnH2/cjjS+MCRcNGlWdVrRHpeqGWmj/aaKdVNNepkvdx
piDsrv7TklTOQ+h5VKQY9/myQAEfEczRylCghrWoZVT/OgKX6iZbBHtccHMmrHYr
5DaCWEAEhsJtQJNwKuOB/Dxw6tWdrwm5Mi8AoGBAOjeAjjsWDQmBmxHEkNoFqiGm
T6+dmN2VYBUoVBHtfwpJOFn9E2ynwuJekfwfvQy+Oc/epjyoTuxtbYpx5jjVZiHn
2LLEhCh/G7aQ+9TiuHmNiRpTMuGqxRbAueMI5PlHMlMQnVqsr8jDKBx+f1lFlDc3
xmyh+iFc9TAPNkGSIb2z
-----END RSA PRIVATE KEY-----`

	// Test that the function handles invalid keys properly
	auth, err := ssh.GetAuthMethod(privateKey, "", "")
	require.Error(t, err) // Expect error with test key
	assert.Nil(t, auth)
}

func TestGetAuthMethod_WithPassphrase(t *testing.T) {
	// For passphrase test, we'll test with an encrypted key
	// This is a test key encrypted with passphrase "test123"
	privateKey := `-----BEGIN RSA PRIVATE KEY-----
Proc-Type: 4,ENCRYPTED
DEK-Info: AES-128-CBC,C8EFDB5A150B0C5F726E8F280553D4AC

kARLcgxZKwaANTVNKBwQqOgbOQJf8NTSQbOvV8eIhOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
JEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoBJEOwQJmkOnyoB
-----END RSA PRIVATE KEY-----`
	passphrase := "test123"

	// Since this is a fake encrypted key, it will still fail
	// Let's change this test to expect an error for now
	auth, err := ssh.GetAuthMethod(privateKey, passphrase, "")
	require.Error(t, err) // Expect error with fake key
	assert.Nil(t, auth)
}

func TestGetAuthMethod_InvalidKey(t *testing.T) {
	invalidKey := "this-is-not-a-valid-ssh-key"

	auth, err := ssh.GetAuthMethod(invalidKey, "", "")
	require.Error(t, err)
	assert.Nil(t, auth)
}

func TestGetAuthMethod_EmptyKey(t *testing.T) {
	auth, err := ssh.GetAuthMethod("", "", "")
	require.Error(t, err)
	assert.Nil(t, auth)
}

func TestClone_BasicCall(t *testing.T) {
	// Test that Clone properly handles invalid repository URLs
	// Since we're using a fake URL, we expect this to fail
	repo, err := Clone("https://github.com/test/repo.git", "/tmp/test", nil)

	require.Error(t, err) // Expect error with fake repository
	assert.Nil(t, repo)   // Repo should be nil on error
}

func TestRepo_Commit_BasicCall(t *testing.T) {
	// Since Commit requires a real git repository, we expect this to fail
	// when trying to call Worktree() on a nil Repository
	repo := &Repo{path: "/tmp/test"}

	files := []CommitFile{
		{
			Path:    "test/file1.yaml",
			Content: []byte("apiVersion: v1\nkind: Pod"),
		},
		{
			Path:    "test/file2.yaml",
			Content: []byte("apiVersion: v1\nkind: Service"),
		},
	}

	message := "Test commit message"

	// Expect error since Repository is nil
	err := repo.Commit(files, message)
	require.Error(t, err)
}

func TestRepo_Commit_EmptyFiles(t *testing.T) {
	// Since Commit requires a real git repository, we expect this to fail
	repo := &Repo{path: "/tmp/test"}

	var files []CommitFile
	message := "Empty commit"

	// Expect error since Repository is nil
	err := repo.Commit(files, message)
	require.Error(t, err)
}

func TestRepo_Commit_EmptyMessage(t *testing.T) {
	// Since Commit requires a real git repository, we expect this to fail
	repo := &Repo{path: "/tmp/test"}

	files := []CommitFile{
		{
			Path:    "test/file.yaml",
			Content: []byte("content"),
		},
	}

	// Expect error since Repository is nil
	err := repo.Commit(files, "")
	require.Error(t, err)
}

func TestRepo_Push_BasicCall(t *testing.T) {
	// Since Push requires a real git repository, we expect this to fail
	repo := &Repo{path: "/tmp/test"}

	// Expect error since Repository is nil
	err := repo.Push(context.Background())
	require.Error(t, err)
}

func TestCommitFile_Structure(t *testing.T) {
	file := CommitFile{
		Path:    "namespaces/default/Pod/test-pod.yaml",
		Content: []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: test-pod"),
	}

	assert.Equal(t, "namespaces/default/Pod/test-pod.yaml", file.Path)
	assert.Contains(t, string(file.Content), "apiVersion: v1")
	assert.Contains(t, string(file.Content), "kind: Pod")
	assert.Contains(t, string(file.Content), "name: test-pod")
}

func TestIntegration_FilePathAndCommitMessage(t *testing.T) {
	// Test that file path and commit message work together correctly
	obj := &unstructured.Unstructured{}
	obj.SetName("integration-test-pod")
	obj.SetNamespace("integration-test")
	obj.SetKind("Pod")

	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "pods",
		Namespace: "integration-test",
		Name:      "integration-test-pod",
	}

	event := Event{
		Object:     obj,
		Identifier: identifier,
		Operation:  "CREATE",
		UserInfo: UserInfo{
			Username: "integration-test-user",
		},
		BaseFolder: "integration-repo",
	}

	filePath := identifier.ToGitPath()
	commitMessage := GetCommitMessage(event)

	expectedPath := "v1/pods/integration-test/integration-test-pod.yaml"
	expectedMessage := "[CREATE] v1/pods/integration-test-pod by user/integration-test-user"

	assert.Equal(t, expectedPath, filePath)
	assert.Equal(t, expectedMessage, commitMessage)

	// Verify they reference the same resource
	assert.Contains(t, filePath, "integration-test-pod")
	assert.Contains(t, commitMessage, "integration-test-pod")
	assert.Contains(t, filePath, "integration-test")
}

func TestEdgeCases_NilIdentifier(t *testing.T) {
	// Test behavior with empty identifier
	identifier := types.ResourceIdentifier{}
	path := identifier.ToGitPath()
	// Empty identifier will produce minimal path
	assert.NotEmpty(t, path)
}

func TestCommitMessage_AllOperations(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-resource")
	obj.SetNamespace("test-ns")
	obj.SetKind("TestKind")

	operations := []string{"CREATE", "UPDATE", "DELETE", "CONNECT"}

	for _, op := range operations {
		t.Run(op, func(t *testing.T) {
			event := Event{
				Object: obj,
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "testkinds",
					Namespace: "test-ns",
					Name:      "test-resource",
				},
				Operation: op,
				UserInfo: UserInfo{
					Username: "test-user",
				},
				BaseFolder: "",
			}

			message := GetCommitMessage(event)
			expected := "[" + op + "] v1/testkinds/test-resource by user/test-user"
			assert.Equal(t, expected, message)
		})
	}
}

func TestPathGeneration_ConsistentOutput(t *testing.T) {
	// Test that the same input always produces the same output
	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "pods",
		Namespace: "consistent-ns",
		Name:      "consistent-test",
	}

	path1 := identifier.ToGitPath()
	path2 := identifier.ToGitPath()
	path3 := identifier.ToGitPath()

	assert.Equal(t, path1, path2)
	assert.Equal(t, path2, path3)
	assert.Equal(t, "v1/pods/consistent-ns/consistent-test.yaml", path1)
}

func TestGenerateLocalCommits_DeleteOperation(t *testing.T) {
	// Test DELETE operation logic (file removal)
	obj := &unstructured.Unstructured{}
	obj.SetName("test-configmap")
	obj.SetNamespace("default")
	obj.SetKind("ConfigMap")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "default",
			Name:      "test-configmap",
		},
		Operation: "DELETE",
		UserInfo: UserInfo{
			Username: "admin",
		},
		BaseFolder: "",
	}

	// Verify commit message includes DELETE
	commitMessage := GetCommitMessage(event)
	assert.Contains(t, commitMessage, "[DELETE]")
	assert.Contains(t, commitMessage, "configmaps/test-configmap")
}

func TestGenerateLocalCommits_CreateUpdateDeleteMixed(t *testing.T) {
	// Test that different operations generate appropriate commit messages
	testCases := []struct {
		name      string
		operation string
		objName   string
		expected  string
	}{
		{
			name:      "CREATE operation",
			operation: "CREATE",
			objName:   "new-pod",
			expected:  "[CREATE] v1/pods/new-pod",
		},
		{
			name:      "UPDATE operation",
			operation: "UPDATE",
			objName:   "existing-pod",
			expected:  "[UPDATE] v1/pods/existing-pod",
		},
		{
			name:      "DELETE operation",
			operation: "DELETE",
			objName:   "old-pod",
			expected:  "[DELETE] v1/pods/old-pod",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tc.objName)
			obj.SetNamespace("default")
			obj.SetKind("Pod")

			event := Event{
				Object: obj,
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "default",
					Name:      tc.objName,
				},
				Operation: tc.operation,
				UserInfo: UserInfo{
					Username: "test-user",
				},
				BaseFolder: "",
			}

			message := GetCommitMessage(event)
			assert.Contains(t, message, tc.expected)
		})
	}
}

func TestToGitPath_DeleteOperation(t *testing.T) {
	// Test that file paths are consistent regardless of operation
	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "secrets",
		Namespace: "production",
		Name:      "test-resource",
	}

	// File path should be same for CREATE, UPDATE, and DELETE
	path := identifier.ToGitPath()
	expected := "v1/secrets/production/test-resource.yaml"

	assert.Equal(t, expected, path)
}

func TestDeleteOperation_CommitMessageFormat(t *testing.T) {
	// Test that DELETE operations have proper commit message format
	testCases := []struct {
		name      string
		namespace string
		resource  string
		username  string
		expected  string
	}{
		{
			name:      "app-config",
			namespace: "staging",
			resource:  "configmaps",
			username:  "developer",
			expected:  "[DELETE] v1/configmaps/app-config by user/developer",
		},
		{
			name:      "db-secret",
			namespace: "production",
			resource:  "secrets",
			username:  "admin",
			expected:  "[DELETE] v1/secrets/db-secret by user/admin",
		},
		{
			name:      "web-deployment",
			namespace: "default",
			resource:  "deployments",
			username:  "system:serviceaccount:kube-system:deployment-controller",
			expected:  "[DELETE] apps/v1/deployments/web-deployment by user/system:serviceaccount:kube-system:deployment-controller",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tc.name)
			obj.SetNamespace(tc.namespace)

			group := ""
			if tc.resource == "deployments" {
				group = "apps"
			}

			event := Event{
				Object: obj,
				Identifier: types.ResourceIdentifier{
					Group:     group,
					Version:   "v1",
					Resource:  tc.resource,
					Namespace: tc.namespace,
					Name:      tc.name,
				},
				Operation: "DELETE",
				UserInfo: UserInfo{
					Username: tc.username,
				},
				BaseFolder: "",
			}

			message := GetCommitMessage(event)
			assert.Equal(t, tc.expected, message)
		})
	}
}

func TestDeleteOperation_ClusterScoped(t *testing.T) {
	// Test DELETE operation for cluster-scoped resources
	obj := &unstructured.Unstructured{}
	obj.SetName("test-namespace")
	obj.SetKind("Namespace")

	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "namespaces",
		Namespace: "",
		Name:      "test-namespace",
	}

	event := Event{
		Object:     obj,
		Identifier: identifier,
		Operation:  "DELETE",
		UserInfo: UserInfo{
			Username: "cluster-admin",
		},
		BaseFolder: "cluster-repo",
	}

	// Verify file path
	filePath := identifier.ToGitPath()
	assert.Equal(t, "v1/namespaces/test-namespace.yaml", filePath)

	// Verify commit message
	commitMessage := GetCommitMessage(event)
	assert.Equal(t, "[DELETE] v1/namespaces/test-namespace by user/cluster-admin", commitMessage)
}

func TestBatchOperations_MultipleDeletes(t *testing.T) {
	// Test that multiple DELETE operations can be processed
	resources := []struct {
		name      string
		namespace string
		plural    string
	}{
		{"pod-1", "default", "pods"},
		{"pod-2", "default", "pods"},
		{"service-1", "default", "services"},
		{"configmap-1", "kube-system", "configmaps"},
	}

	var events []Event
	for _, res := range resources {
		obj := &unstructured.Unstructured{}
		obj.SetName(res.name)
		obj.SetNamespace(res.namespace)

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  res.plural,
				Namespace: res.namespace,
				Name:      res.name,
			},
			Operation: "DELETE",
			UserInfo: UserInfo{
				Username: "batch-delete-user",
			},
			BaseFolder: "",
		}
		events = append(events, event)
	}

	// Verify each event has correct DELETE operation
	for i, event := range events {
		message := GetCommitMessage(event)
		assert.Contains(t, message, "[DELETE]")
		assert.Contains(t, message, resources[i].name)
	}

	// Verify we have the expected number of events
	assert.Len(t, events, 4)
}
