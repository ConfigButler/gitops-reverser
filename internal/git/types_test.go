// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestBranchKeyFor_OneKeyPerRepositoryBranch: a branch worker is keyed by repository and branch, so
// every spelling of one repository's URL is one key, and another branch or repository is another.
func TestBranchKeyFor_OneKeyPerRepositoryBranch(t *testing.T) {
	https := BranchKeyFor("https://github.com/acme/platform.git", "main")
	assert.Equal(t, BranchKey{Repository: "github.com/acme/platform", Branch: "main"}, https)
	assert.Equal(t, https, BranchKeyFor("git@github.com:acme/platform", "main"), "ssh and https name one repository")
	assert.Equal(t, https, BranchKeyFor("https://GitHub.com/Acme/Platform", "main"), "case is not identity")
	assert.NotEqual(t, https, BranchKeyFor("https://github.com/acme/platform", "develop"))
	assert.NotEqual(t, https, BranchKeyFor("https://github.com/acme/other", "main"))
	assert.Equal(t, BranchKey{}, BranchKeyFor("", "main"), "no URL names no repository")
	assert.Equal(t, "github.com/acme/platform@main", https.String())
}

// TestWritesThrough_IsTheGitBranch: the GitTargets a worker serves are those that reference its
// GitBranch, from whichever namespace.
func TestWritesThrough_IsTheGitBranch(t *testing.T) {
	shared := k8stypes.NamespacedName{Namespace: "platform", Name: "main"}
	target := &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "apps"},
		Spec: v1alpha3.GitTargetSpec{
			GitBranchRef: meta.NamespacedObjectReference{Name: "main", Namespace: "platform"},
		},
	}
	assert.True(t, WritesThrough(target, shared))
	assert.False(t, WritesThrough(target, k8stypes.NamespacedName{Namespace: "team-a", Name: "main"}),
		"a GitBranch of the same name in the GitTarget's own namespace is another one")

	target.Spec.GitBranchRef.Namespace = ""
	assert.True(t, WritesThrough(target, k8stypes.NamespacedName{Namespace: "team-a", Name: "main"}),
		"no namespace means the GitTarget's own")
	target.Spec.GitBranchRef = meta.NamespacedObjectReference{}
	assert.False(t, WritesThrough(target, k8stypes.NamespacedName{}), "no reference writes through nothing")
}

// TestEventWithObject verifies event creation with objects.
func TestEventWithObject(t *testing.T) {
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
			Username: "admin",
			UID:      "12345",
		},
		Path: "clusters/prod",
	}

	if event.Object == nil {
		t.Error("Expected Object to be set")
	}
	if event.Object.GetName() != "test-pod" {
		t.Errorf("Object name = %q, want 'test-pod'", event.Object.GetName())
	}
	if event.Operation != "CREATE" {
		t.Errorf("Operation = %q, want 'CREATE'", event.Operation)
	}
	if event.Path != "clusters/prod" {
		t.Errorf("Path = %q, want 'clusters/prod'", event.Path)
	}
}
