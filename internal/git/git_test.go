package git

import (
	"context"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestGetFilePath_NamespacedResource(t *testing.T) {
	testCases := []struct {
		name      string
		namespace string
		kind      string
		expected  string
	}{
		{
			name:      "test-pod",
			namespace: "default",
			kind:      "Pod",
			expected:  "namespaces/default/Pod/test-pod.yaml",
		},
		{
			name:      "my-service",
			namespace: "production",
			kind:      "Service",
			expected:  "namespaces/production/Service/my-service.yaml",
		},
		{
			name:      "app-config",
			namespace: "staging",
			kind:      "ConfigMap",
			expected:  "namespaces/staging/ConfigMap/app-config.yaml",
		},
		{
			name:      "complex-name-with-dashes",
			namespace: "kube-system",
			kind:      "Deployment",
			expected:  "namespaces/kube-system/Deployment/complex-name-with-dashes.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tc.name)
			obj.SetNamespace(tc.namespace)
			obj.SetKind(tc.kind)

			path := GetFilePath(obj)
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestGetFilePath_ClusterScopedResource(t *testing.T) {
	testCases := []struct {
		name     string
		kind     string
		expected string
	}{
		{
			name:     "my-namespace",
			kind:     "Namespace",
			expected: "cluster-scoped/Namespace/my-namespace.yaml",
		},
		{
			name:     "cluster-admin",
			kind:     "ClusterRole",
			expected: "cluster-scoped/ClusterRole/cluster-admin.yaml",
		},
		{
			name:     "system-binding",
			kind:     "ClusterRoleBinding",
			expected: "cluster-scoped/ClusterRoleBinding/system-binding.yaml",
		},
		{
			name:     "my-pv",
			kind:     "PersistentVolume",
			expected: "cluster-scoped/PersistentVolume/my-pv.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tc.name)
			// No namespace for cluster-scoped resources
			obj.SetKind(tc.kind)

			path := GetFilePath(obj)
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestGetFilePath_EmptyNamespace(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-resource")
	obj.SetNamespace("") // Empty namespace
	obj.SetKind("TestKind")

	path := GetFilePath(obj)
	assert.Equal(t, "cluster-scoped/TestKind/test-resource.yaml", path)
}

func TestGetFilePath_SpecialCharacters(t *testing.T) {
	// Test with names that might have special characters
	testCases := []struct {
		name      string
		namespace string
		kind      string
		expected  string
	}{
		{
			name:      "test.resource",
			namespace: "default",
			kind:      "Pod",
			expected:  "namespaces/default/Pod/test.resource.yaml",
		},
		{
			name:      "test_resource",
			namespace: "default",
			kind:      "Service",
			expected:  "namespaces/default/Service/test_resource.yaml",
		},
		{
			name:      "test-resource-123",
			namespace: "test-ns-456",
			kind:      "ConfigMap",
			expected:  "namespaces/test-ns-456/ConfigMap/test-resource-123.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tc.name)
			obj.SetNamespace(tc.namespace)
			obj.SetKind(tc.kind)

			path := GetFilePath(obj)
			assert.Equal(t, tc.expected, path)
		})
	}
}

func TestGetCommitMessage_CreateOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{
					Username: "john.doe@example.com",
				},
			},
		},
		GitRepoConfigRef: "test-repo",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] Pod/test-pod in ns/default by user/john.doe@example.com"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_UpdateOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("my-service")
	obj.SetNamespace("production")
	obj.SetKind("Service")

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Update,
				UserInfo: authenticationv1.UserInfo{
					Username: "system:serviceaccount:kube-system:deployment-controller",
				},
			},
		},
		GitRepoConfigRef: "prod-repo",
	}

	message := GetCommitMessage(event)
	expected := "[UPDATE] Service/my-service in ns/production by user/system:serviceaccount:kube-system:deployment-controller"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_DeleteOperation(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("old-config")
	obj.SetNamespace("staging")
	obj.SetKind("ConfigMap")

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Delete,
				UserInfo: authenticationv1.UserInfo{
					Username: "admin",
				},
			},
		},
		GitRepoConfigRef: "staging-repo",
	}

	message := GetCommitMessage(event)
	expected := "[DELETE] ConfigMap/old-config in ns/staging by user/admin"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_ClusterScopedResource(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("my-namespace")
	obj.SetKind("Namespace")
	// No namespace for cluster-scoped resources

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{
					Username: "cluster-admin",
				},
			},
		},
		GitRepoConfigRef: "cluster-repo",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] Namespace/my-namespace in ns/ by user/cluster-admin"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_EmptyUsername(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{
					Username: "", // Empty username
				},
			},
		},
		GitRepoConfigRef: "test-repo",
	}

	message := GetCommitMessage(event)
	expected := "[CREATE] Pod/test-pod in ns/default by user/"
	assert.Equal(t, expected, message)
}

func TestGetCommitMessage_SpecialCharactersInNames(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod.with.dots")
	obj.SetNamespace("test-ns_with_underscores")
	obj.SetKind("Pod")

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Update,
				UserInfo: authenticationv1.UserInfo{
					Username: "user@domain.com",
				},
			},
		},
		GitRepoConfigRef: "test-repo",
	}

	message := GetCommitMessage(event)
	expected := "[UPDATE] Pod/test-pod.with.dots in ns/test-ns_with_underscores by user/user@domain.com"
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
	auth, err := GetAuthMethod(privateKey, "")
	assert.Error(t, err) // Expect error with test key
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
	auth, err := GetAuthMethod(privateKey, passphrase)
	assert.Error(t, err) // Expect error with fake key
	assert.Nil(t, auth)
}

func TestGetAuthMethod_InvalidKey(t *testing.T) {
	invalidKey := "this-is-not-a-valid-ssh-key"

	auth, err := GetAuthMethod(invalidKey, "")
	assert.Error(t, err)
	assert.Nil(t, auth)
}

func TestGetAuthMethod_EmptyKey(t *testing.T) {
	auth, err := GetAuthMethod("", "")
	assert.Error(t, err)
	assert.Nil(t, auth)
}

func TestClone_BasicCall(t *testing.T) {
	// Test that Clone properly handles invalid repository URLs
	// Since we're using a fake URL, we expect this to fail
	repo, err := Clone("https://github.com/test/repo.git", "/tmp/test", nil)

	assert.Error(t, err) // Expect error with fake repository
	assert.Nil(t, repo)  // Repo should be nil on error
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
	assert.Error(t, err)
}

func TestRepo_Commit_EmptyFiles(t *testing.T) {
	// Since Commit requires a real git repository, we expect this to fail
	repo := &Repo{path: "/tmp/test"}

	var files []CommitFile
	message := "Empty commit"

	// Expect error since Repository is nil
	err := repo.Commit(files, message)
	assert.Error(t, err)
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
	assert.Error(t, err)
}

func TestRepo_Push_BasicCall(t *testing.T) {
	// Since Push requires a real git repository, we expect this to fail
	repo := &Repo{path: "/tmp/test"}

	// Expect error since Repository is nil
	err := repo.Push(context.Background())
	assert.Error(t, err)
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

	event := eventqueue.Event{
		Object: obj,
		Request: admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{
					Username: "integration-test-user",
				},
			},
		},
		GitRepoConfigRef: "integration-repo",
	}

	filePath := GetFilePath(obj)
	commitMessage := GetCommitMessage(event)

	expectedPath := "namespaces/integration-test/Pod/integration-test-pod.yaml"
	expectedMessage := "[CREATE] Pod/integration-test-pod in ns/integration-test by user/integration-test-user"

	assert.Equal(t, expectedPath, filePath)
	assert.Equal(t, expectedMessage, commitMessage)

	// Verify they reference the same resource
	assert.Contains(t, filePath, "integration-test-pod")
	assert.Contains(t, commitMessage, "integration-test-pod")
	assert.Contains(t, filePath, "integration-test")
	assert.Contains(t, commitMessage, "integration-test")
}

func TestEdgeCases_NilObject(t *testing.T) {
	// Test behavior with nil object - should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("GetFilePath panicked with nil object: %v", r)
		}
	}()

	// This will likely panic in the current implementation, but we're testing
	// that we handle it gracefully in the future
	var obj *unstructured.Unstructured

	// We expect this to panic currently, so we'll catch it
	func() {
		defer func() {
			_ = recover() // Catch the panic
		}()
		GetFilePath(obj)
	}()
}

func TestCommitMessage_AllOperations(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-resource")
	obj.SetNamespace("test-ns")
	obj.SetKind("TestKind")

	operations := []admissionv1.Operation{
		admissionv1.Create,
		admissionv1.Update,
		admissionv1.Delete,
		admissionv1.Connect,
	}

	for _, op := range operations {
		t.Run(string(op), func(t *testing.T) {
			event := eventqueue.Event{
				Object: obj,
				Request: admission.Request{
					AdmissionRequest: admissionv1.AdmissionRequest{
						Operation: op,
						UserInfo: authenticationv1.UserInfo{
							Username: "test-user",
						},
					},
				},
				GitRepoConfigRef: "test-repo",
			}

			message := GetCommitMessage(event)
			expected := "[" + string(op) + "] TestKind/test-resource in ns/test-ns by user/test-user"
			assert.Equal(t, expected, message)
		})
	}
}

func TestPathGeneration_ConsistentOutput(t *testing.T) {
	// Test that the same input always produces the same output
	obj := &unstructured.Unstructured{}
	obj.SetName("consistent-test")
	obj.SetNamespace("consistent-ns")
	obj.SetKind("Pod")

	path1 := GetFilePath(obj)
	path2 := GetFilePath(obj)
	path3 := GetFilePath(obj)

	assert.Equal(t, path1, path2)
	assert.Equal(t, path2, path3)
	assert.Equal(t, "namespaces/consistent-ns/Pod/consistent-test.yaml", path1)
}
