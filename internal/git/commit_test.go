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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestResolveCommitConfig_Defaults(t *testing.T) {
	config := ResolveCommitConfig(nil)

	assert.Equal(t, DefaultCommitterName, config.Committer.Name)
	assert.Equal(t, DefaultCommitterEmail, config.Committer.Email)
	assert.Equal(t, DefaultCommitMessageTemplate, config.Message.Template)
	assert.Equal(t, DefaultBatchCommitMessageTemplate, config.Message.BatchTemplate)
	assert.Equal(t, DefaultGroupCommitMessageTemplate, config.Message.GroupTemplate)
}

func TestResolveCommitConfig_CustomValues(t *testing.T) {
	config := ResolveCommitConfig(&v1alpha1.CommitSpec{
		Committer: &v1alpha1.CommitterSpec{
			Name:  "Audit Bot",
			Email: "audit@example.com",
		},
		Message: &v1alpha1.CommitMessageSpec{
			Template:      "audit: {{.Operation}} {{.Name}}",
			BatchTemplate: "snapshot: {{.Count}} {{.GitTarget}}",
			GroupTemplate: "grouped: {{.Author}} {{.Count}}",
		},
	})

	assert.Equal(t, "Audit Bot", config.Committer.Name)
	assert.Equal(t, "audit@example.com", config.Committer.Email)
	assert.Equal(t, "audit: {{.Operation}} {{.Name}}", config.Message.Template)
	assert.Equal(t, "snapshot: {{.Count}} {{.GitTarget}}", config.Message.BatchTemplate)
	assert.Equal(t, "grouped: {{.Author}} {{.Count}}", config.Message.GroupTemplate)
}

func TestValidateCommitConfig_InvalidTemplate(t *testing.T) {
	config := ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			Template: "{{.Operation",
		},
	})

	err := ValidateCommitConfig(config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse event commit template")
}

func TestValidateCommitConfig_InvalidGroupTemplate(t *testing.T) {
	config := ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			GroupTemplate: "{{.Author",
		},
	})

	err := ValidateCommitConfig(config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse group commit template")
}

func TestRenderEventCommitMessage_CustomTemplate(t *testing.T) {
	event := Event{
		Operation: "UPDATE",
		Identifier: types.ResourceIdentifier{
			Group:     "apps",
			Version:   "v1",
			Resource:  "deployments",
			Namespace: "prod",
			Name:      "api",
		},
		UserInfo:      UserInfo{Username: "alice"},
		GitTargetName: "platform",
	}

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			Template: "audit({{.GitTarget}}): {{.Username}} {{.Operation}} {{.Namespace}}/{{.Name}}",
		},
	}))
	require.NoError(t, err)
	assert.Equal(t, "audit(platform): alice UPDATE prod/api", message)
}

func TestRenderBatchCommitMessage_DefaultTemplate(t *testing.T) {
	message, err := renderBatchCommitMessage(
		[]Event{{Operation: "CREATE"}, {Operation: "DELETE"}},
		"",
		"demo",
		ResolveCommitConfig(nil),
	)
	require.NoError(t, err)
	assert.Equal(t, "reconcile: sync 2 resources", message)
}

func TestRenderGroupCommitMessage_CustomTemplate(t *testing.T) {
	events := []Event{{
		Operation: "UPDATE",
		Identifier: types.ResourceIdentifier{
			Group:     "apps",
			Version:   "v1",
			Resource:  "deployments",
			Namespace: "prod",
			Name:      "api",
		},
		UserInfo:           UserInfo{Username: "alice"},
		GitTargetName:      "platform",
		GitTargetNamespace: "default",
	}}

	groups := groupCommits(events)
	require.Len(t, groups, 1)

	message, err := renderGroupCommitMessage(CommitUnit{
		Events:      groups[0].orderedEvents(),
		GroupAuthor: groups[0].Author,
		Target: ResolvedTargetMetadata{
			Name: groups[0].GitTarget,
		},
	}, ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			GroupTemplate: "grouped({{.GitTarget}}): {{.Author}} changed {{.Count}} resource(s)",
		},
	}))
	require.NoError(t, err)
	assert.Equal(t, "grouped(platform): alice changed 1 resource(s)", message)
}

func TestRenderEventCommitMessage_CreateOperation(t *testing.T) {
	event := newCommitTestEvent("pods", "default", "test-pod", "CREATE", "john.doe@example.com")

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[CREATE] v1/pods/test-pod", message)
}

func TestRenderEventCommitMessage_UpdateOperation(t *testing.T) {
	event := newCommitTestEvent(
		"services",
		"production",
		"my-service",
		"UPDATE",
		"system:serviceaccount:kube-system:deployment-controller",
	)
	event.Path = "prod-repo"

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[UPDATE] v1/services/my-service", message)
}

func TestRenderEventCommitMessage_DeleteOperation(t *testing.T) {
	event := newCommitTestEvent("configmaps", "staging", "old-config", "DELETE", "admin")
	event.Path = "staging-repo"

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[DELETE] v1/configmaps/old-config", message)
}

func TestRenderEventCommitMessage_ClusterScopedResource(t *testing.T) {
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
		UserInfo:  UserInfo{Username: "cluster-admin"},
		Path:      "cluster-repo",
	}

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[CREATE] v1/namespaces/my-namespace", message)
}

func TestRenderEventCommitMessage_EmptyUsername(t *testing.T) {
	event := newCommitTestEvent("pods", "default", "test-pod", "CREATE", "")

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[CREATE] v1/pods/test-pod", message)
}

func TestRenderEventCommitMessage_SpecialCharactersInNames(t *testing.T) {
	event := newCommitTestEvent("pods", "test-ns_with_underscores", "test-pod.with.dots", "UPDATE", "user@domain.com")

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "[UPDATE] v1/pods/test-pod.with.dots", message)
}

func TestIntegration_FilePathAndCommitMessage(t *testing.T) {
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
		UserInfo:   UserInfo{Username: "integration-test-user"},
		Path:       "integration-repo",
	}

	filePath := identifier.ToGitPath()
	commitMessage, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)

	assert.Equal(t, "v1/pods/integration-test/integration-test-pod.yaml", filePath)
	assert.Equal(t, "[CREATE] v1/pods/integration-test-pod", commitMessage)
	assert.Contains(t, filePath, "integration-test-pod")
	assert.Contains(t, commitMessage, "integration-test-pod")
	assert.Contains(t, filePath, "integration-test")
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
				UserInfo:  UserInfo{Username: "test-user"},
			}

			message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
			require.NoError(t, err)
			assert.Equal(t, "["+op+"] v1/testkinds/test-resource", message)
		})
	}
}

func TestGenerateLocalCommits_DeleteOperation(t *testing.T) {
	event := newCommitTestEvent("configmaps", "default", "test-configmap", "DELETE", "admin")

	commitMessage, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Contains(t, commitMessage, "[DELETE]")
	assert.Contains(t, commitMessage, "configmaps/test-configmap")
}

func TestGenerateLocalCommits_CreateUpdateDeleteMixed(t *testing.T) {
	testCases := []struct {
		name      string
		operation string
		objName   string
		expected  string
	}{
		{name: "CREATE operation", operation: "CREATE", objName: "new-pod", expected: "[CREATE] v1/pods/new-pod"},
		{
			name:      "UPDATE operation",
			operation: "UPDATE",
			objName:   "existing-pod",
			expected:  "[UPDATE] v1/pods/existing-pod",
		},
		{name: "DELETE operation", operation: "DELETE", objName: "old-pod", expected: "[DELETE] v1/pods/old-pod"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			event := newCommitTestEvent("pods", "default", tc.objName, tc.operation, "test-user")
			message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
			require.NoError(t, err)
			assert.Contains(t, message, tc.expected)
		})
	}
}

func TestDeleteOperation_CommitMessageFormat(t *testing.T) {
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
			expected:  "[DELETE] v1/configmaps/app-config",
		},
		{
			name:      "db-secret",
			namespace: "production",
			resource:  "secrets",
			username:  "admin",
			expected:  "[DELETE] v1/secrets/db-secret",
		},
		{
			name:      "web-deployment",
			namespace: "default",
			resource:  "deployments",
			username:  "system:serviceaccount:kube-system:deployment-controller",
			expected:  "[DELETE] apps/v1/deployments/web-deployment",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			group := ""
			if tc.resource == "deployments" {
				group = "apps"
			}

			obj := &unstructured.Unstructured{}
			obj.SetName(tc.name)
			obj.SetNamespace(tc.namespace)

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
				UserInfo:  UserInfo{Username: tc.username},
			}

			message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
			require.NoError(t, err)
			assert.Equal(t, tc.expected, message)
		})
	}
}

func TestDeleteOperation_ClusterScoped(t *testing.T) {
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
		UserInfo:   UserInfo{Username: "cluster-admin"},
		Path:       "cluster-repo",
	}

	filePath := identifier.ToGitPath()
	commitMessage, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	require.NoError(t, err)

	assert.Equal(t, "v1/namespaces/test-namespace.yaml", filePath)
	assert.Equal(t, "[DELETE] v1/namespaces/test-namespace", commitMessage)
}

func TestBatchOperations_MultipleDeletes(t *testing.T) {
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
		events = append(events, newCommitTestEvent(res.plural, res.namespace, res.name, "DELETE", "batch-delete-user"))
	}

	for i, event := range events {
		message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
		require.NoError(t, err)
		assert.Contains(t, message, "[DELETE]")
		assert.Contains(t, message, resources[i].name)
	}

	assert.Len(t, events, 4)
}

func newCommitTestEvent(resource, namespace, name, operation, username string) Event {
	obj := &unstructured.Unstructured{}
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetKind("Pod")

	return Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  resource,
			Namespace: namespace,
			Name:      name,
		},
		Operation: operation,
		UserInfo:  UserInfo{Username: username},
	}
}
