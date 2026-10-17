// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestLastFieldManagers pins who counts as the last writer: the latest entry, status left out so a
// controller's status write cannot hide the apply before it, scale kept because it writes replicas,
// and every manager of a same-second tie.
func TestLastFieldManagers(t *testing.T) {
	at := func(seconds int) *metav1.Time {
		return &metav1.Time{Time: time.Date(2026, 10, 17, 12, 0, seconds, 0, time.UTC)}
	}
	entry := func(manager, subresource string, time *metav1.Time) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Subresource: subresource, Time: time}
	}
	cases := []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    []string
	}{
		{name: "none", want: nil},
		{name: "the latest entry", entries: []metav1.ManagedFieldsEntry{
			entry("kubectl-edit", "", at(1)), entry("kustomize-controller", "", at(2)),
		}, want: []string{"kustomize-controller"}},
		{name: "a later status write does not hide the apply", entries: []metav1.ManagedFieldsEntry{
			entry("kustomize-controller", "", at(2)), entry("kube-controller-manager", "status", at(3)),
		}, want: []string{"kustomize-controller"}},
		{name: "a later scale write is a write", entries: []metav1.ManagedFieldsEntry{
			entry("kustomize-controller", "", at(2)), entry("horizontal-pod-autoscaler", "scale", at(3)),
		}, want: []string{"horizontal-pod-autoscaler"}},
		{name: "a same-second tie names both", entries: []metav1.ManagedFieldsEntry{
			entry("kubectl-edit", "", at(2)), entry("kustomize-controller", "", at(2)), entry("old", "", at(1)),
		}, want: []string{"kubectl-edit", "kustomize-controller"}},
		{name: "an entry without a time says nothing", entries: []metav1.ManagedFieldsEntry{
			entry("kubectl-edit", "", at(1)), entry("before-v1.14", "", nil),
		}, want: []string{"kubectl-edit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "api", "namespace": "shop"},
			}}
			u.SetManagedFields(tc.entries)
			assert.Equal(t, tc.want, lastFieldManagers(u))

			event := targetWatchGitEvent(
				schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
				u,
				string(types.OperationUpdate),
			)
			assert.Equal(t, tc.want, event.LastFieldManagers)
			assert.Empty(t, event.Object.GetManagedFields(), "managedFields never reach Git")
		})
	}
}
