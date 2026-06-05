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

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func testGitDest() types.ResourceReference {
	return types.NewResourceReference("git", "default")
}

// nsGVR / clGVR build watch GVRs for the v1 resources the common test catalog
// serves; the version is fixed because every served test resource is v1.
func nsGVR(group, resource string) GVR {
	return GVR{Group: group, Version: "v1", Resource: resource, Scope: configv1alpha1.ResourceScopeNamespaced}
}

func clGVR(group, resource string) GVR {
	return GVR{Group: group, Version: "v1", Resource: resource, Scope: configv1alpha1.ResourceScopeCluster}
}

func TestBuildWatchedTypeTable_NamespacedTypeCarriesServedMetadata(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("apps", "deployments"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 7, selections, catalog)

	require.Empty(t, table.Conflicts)
	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, wt.GVK)
	assert.Equal(t, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, wt.GVR)
	assert.True(t, wt.Namespaced)
	assert.Equal(t, configv1alpha1.ResourceScopeNamespaced, wt.Scope)
	assert.Equal(t, "v1", wt.ServedVersion)
	assert.True(t, wt.Preferred)
	assert.Equal(t, []string{"team-a"}, wt.SnapshotNamespaces())
	assert.False(t, wt.ClusterWide())
	assert.Equal(t, uint64(7), table.ResolvedAt)
}

func TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	// The same GVR followed both in a specific namespace (WatchRule) and cluster-wide
	// (ClusterWatchRule with Namespaced scope) collapses to one cluster-wide stream
	// for the snapshot, but both namespace keys survive for the plan hash.
	selections := []resolvedSelection{
		{gvr: nsGVR("", "configmaps"), namespace: "team-a"},
		{gvr: nsGVR("", "configmaps"), namespace: ""},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.True(t, wt.ClusterWide())
	assert.Empty(t, wt.SnapshotNamespaces())
	assert.Contains(t, wt.NamespaceOps, "")
	assert.Contains(t, wt.NamespaceOps, "team-a")
}

func TestBuildWatchedTypeTable_OperationsUnionPerNamespace(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("", "configmaps"), namespace: "team-a", ops: []configv1alpha1.OperationType{
			configv1alpha1.OperationCreate,
		}},
		{gvr: nsGVR("", "configmaps"), namespace: "team-a", ops: []configv1alpha1.OperationType{
			configv1alpha1.OperationUpdate,
		}},
		{gvr: nsGVR("", "configmaps"), namespace: "team-b", ops: []configv1alpha1.OperationType{
			configv1alpha1.OperationAll,
		}},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, []string{"CREATE", "UPDATE"}, wt.NamespaceOps["team-a"].Sorted())
	assert.Equal(t, []string{"*"}, wt.NamespaceOps["team-b"].Sorted())
}

func TestBuildWatchedTypeTable_EmptyOperationsAreAllOperations(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("", "configmaps"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"*"}, table.Types[0].NamespaceOps["team-a"].Sorted())
}

func TestBuildWatchedTypeTable_ClusterScopedType(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: clGVR("", "namespaces"), namespace: ""},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, wt.GVK)
	assert.False(t, wt.Namespaced)
	assert.Equal(t, configv1alpha1.ResourceScopeCluster, wt.Scope)
	assert.True(t, wt.ClusterWide())
	assert.Empty(t, wt.SnapshotNamespaces())
}

func TestBuildWatchedTypeTable_RefusesGVKServedByMultipleResources(t *testing.T) {
	catalog := newWidgetConflictCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("example.com", "widgets"), namespace: "team-a"},
		{gvr: nsGVR("example.com", "widgetslegacy"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	assert.Empty(t, table.Types, "a GVK served by >1 resource must not be watched")
	require.Len(t, table.Conflicts, 1)
	conflict := table.Conflicts[0]
	assert.Equal(t, schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}, conflict.GVK)
	assert.Equal(t, []schema.GroupVersionResource{
		{Group: "example.com", Version: "v1", Resource: "widgets"},
		{Group: "example.com", Version: "v1", Resource: "widgetslegacy"},
	}, conflict.GVRs)
}

func TestBuildWatchedTypeTable_SkipsGVRNotServedByCatalog(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("nonexistent.example.com", "ghosts"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	assert.Empty(t, table.Types)
	assert.Empty(t, table.Conflicts)
}

func TestBuildWatchedTypeTable_SortsTypesByGVK(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("", "services"), namespace: "team-a"},
		{gvr: nsGVR("apps", "deployments"), namespace: "team-a"},
		{gvr: nsGVR("", "configmaps"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Types, 3)
	got := []string{
		table.Types[0].GVK.Kind,
		table.Types[1].GVK.Kind,
		table.Types[2].GVK.Kind,
	}
	// Sorted by group|version|kind. The empty core group renders as a leading
	// "|" (ASCII 124), which sorts after named groups like "apps" (ASCII 97) —
	// the same convention as sortCatalogEntries.
	assert.Equal(t, []string{"Deployment", "ConfigMap", "Service"}, got)
}

func TestBuildWatchedTypeTable_NilCatalogWatchesNothing(t *testing.T) {
	selections := []resolvedSelection{
		{gvr: nsGVR("apps", "deployments"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, nil)

	assert.Empty(t, table.Types, "without a catalog no GVR can be mapped to a GVK, so nothing is watched")
	assert.Empty(t, table.Conflicts)
}

func TestBuildWatchedTypeTable_SortsMultipleConflictsByGVK(t *testing.T) {
	catalog := newTwoConflictCatalog(t)
	selections := []resolvedSelection{
		{gvr: nsGVR("example.com", "gadgets"), namespace: "team-a"},
		{gvr: nsGVR("example.com", "gadgetslegacy"), namespace: "team-a"},
		{gvr: nsGVR("example.com", "widgets"), namespace: "team-a"},
		{gvr: nsGVR("example.com", "widgetslegacy"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections, catalog)

	require.Len(t, table.Conflicts, 2)
	assert.Equal(t, "Gadget", table.Conflicts[0].GVK.Kind, "conflicts sort by GVK: Gadget before Widget")
	assert.Equal(t, "Widget", table.Conflicts[1].GVK.Kind)
}

// newTwoConflictCatalog serves two kinds, each from two resources, so a wildcard
// selection produces two GVK<->GVR conflicts to exercise conflict sorting.
func newTwoConflictCatalog(t *testing.T) *APIResourceCatalog {
	t.Helper()
	listWatch := metav1.Verbs{"get", "list", "watch"}
	disco := staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("example.com", "v1")},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{
					{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: listWatch},
					{Name: "widgetslegacy", Kind: "Widget", Namespaced: true, Verbs: listWatch},
					{Name: "gadgets", Kind: "Gadget", Namespaced: true, Verbs: listWatch},
					{Name: "gadgetslegacy", Kind: "Gadget", Namespaced: true, Verbs: listWatch},
				},
			},
		},
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)
	return catalog
}

// newWidgetConflictCatalog builds a pathological catalog where one group/version
// serves the same kind from two distinct resources, violating the GVK<->GVR 1:1
// assumption the watched-type table enforces.
func newWidgetConflictCatalog(t *testing.T) *APIResourceCatalog {
	t.Helper()
	listWatch := metav1.Verbs{"get", "list", "watch"}
	disco := staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("example.com", "v1")},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{
					{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: listWatch},
					{Name: "widgetslegacy", Kind: "Widget", Namespaced: true, Verbs: listWatch},
				},
			},
		},
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)
	return catalog
}
