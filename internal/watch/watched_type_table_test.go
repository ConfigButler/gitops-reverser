// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

func testGitDest() types.ResourceReference {
	return types.NewResourceReference("git", "default")
}

// followableRecord builds a minimal followable typeset record for the given identity,
// the shape resolveWatchedTypeTables folds into the table.
func followableRecord(group, version, resource, kind string, scope typeset.Scope, preferred bool) typeset.TypeRecord {
	return typeset.TypeRecord{
		Identity: typeset.Identity{
			GVK:   schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
			GVR:   schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
			Scope: scope,
		},
		Preferred: preferred,
	}
}

func nsRecord(group, resource, kind string) typeset.TypeRecord {
	return followableRecord(group, "v1", resource, kind, typeset.ScopeNamespaced, true)
}

// namespaceRecord is the followable cluster-scoped Namespace record the matcher tests
// use to exercise scope filtering.
func namespaceRecord() typeset.TypeRecord {
	return followableRecord("", "v1", "namespaces", "Namespace", typeset.ScopeCluster, true)
}

func TestBuildWatchedTypeTable_NamespacedTypeCarriesRecordMetadata(t *testing.T) {
	selections := []watchSelection{
		{record: nsRecord("apps", "deployments", "Deployment"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 7, selections)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, wt.GVK)
	assert.Equal(t, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, wt.GVR)
	assert.True(t, wt.Namespaced)
	assert.Equal(t, configv1alpha3.ResourceScopeNamespaced, wt.Scope)
	assert.Equal(t, "v1", wt.ServedVersion)
	assert.True(t, wt.Preferred)
	assert.Equal(t, []string{"team-a"}, wt.SnapshotNamespaces())
	assert.False(t, wt.ClusterWide())
	assert.Equal(t, uint64(7), table.ResolvedAt)
}

func TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces(t *testing.T) {
	// The same record followed both in a specific namespace (WatchRule) and cluster-wide
	// (ClusterWatchRule) collapses to one cluster-wide stream for the snapshot, but both
	// namespace keys survive for the plan hash.
	cm := nsRecord("", "configmaps", "ConfigMap")
	selections := []watchSelection{
		{record: cm, namespace: "team-a"},
		{record: cm, namespace: ""},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.True(t, wt.ClusterWide())
	assert.Empty(t, wt.SnapshotNamespaces())
	assert.Contains(t, wt.NamespaceSelections, "")
	assert.Contains(t, wt.NamespaceSelections, "team-a")
}

func TestBuildWatchedTypeTable_OperationsUnionPerNamespace(t *testing.T) {
	cm := nsRecord("", "configmaps", "ConfigMap")
	selections := []watchSelection{
		{record: cm, namespace: "team-a", ops: []configv1alpha3.OperationType{configv1alpha3.OperationCreate}},
		{record: cm, namespace: "team-a", ops: []configv1alpha3.OperationType{configv1alpha3.OperationUpdate}},
		{record: cm, namespace: "team-b", ops: []configv1alpha3.OperationType{configv1alpha3.OperationAll}},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, []string{"CREATE", "UPDATE"}, wt.NamespaceOps("team-a").Sorted())
	assert.Equal(t, []string{"*"}, wt.NamespaceOps("team-b").Sorted())
}

func TestBuildWatchedTypeTable_EmptyOperationsAreAllOperations(t *testing.T) {
	selections := []watchSelection{
		{record: nsRecord("", "configmaps", "ConfigMap"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections)

	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"*"}, table.Types[0].NamespaceOps("team-a").Sorted())
}

func TestBuildWatchedTypeTable_ClusterScopedType(t *testing.T) {
	selections := []watchSelection{
		{record: namespaceRecord(), namespace: ""},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections)

	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, wt.GVK)
	assert.False(t, wt.Namespaced)
	assert.Equal(t, configv1alpha3.ResourceScopeCluster, wt.Scope)
	assert.True(t, wt.ClusterWide())
	assert.Empty(t, wt.SnapshotNamespaces())
}

func TestBuildWatchedTypeTable_SortsTypesByGVK(t *testing.T) {
	selections := []watchSelection{
		{record: nsRecord("", "services", "Service"), namespace: "team-a"},
		{record: nsRecord("apps", "deployments", "Deployment"), namespace: "team-a"},
		{record: nsRecord("", "configmaps", "ConfigMap"), namespace: "team-a"},
	}

	table := buildWatchedTypeTable(testGitDest(), 1, selections)

	require.Len(t, table.Types, 3)
	got := []string{table.Types[0].GVK.Kind, table.Types[1].GVK.Kind, table.Types[2].GVK.Kind}
	// Sorted by group|version|kind. The empty core group renders as a leading "|"
	// (ASCII 124), which sorts after named groups like "apps" (ASCII 97).
	assert.Equal(t, []string{"Deployment", "ConfigMap", "Service"}, got)
}

func TestMatchFollowableRecords_MatchesResourceGroupVersionScope(t *testing.T) {
	records := []typeset.TypeRecord{
		nsRecord("apps", "deployments", "Deployment"),
		nsRecord("", "configmaps", "ConfigMap"),
		namespaceRecord(),
	}

	matched := matchFollowableRecords(
		records, []string{"apps"}, []string{"v1"}, []string{"deployments"},
		configv1alpha3.ResourceScopeNamespaced)

	require.Len(t, matched, 1)
	assert.Equal(t, "Deployment", matched[0].Identity.GVK.Kind)
}

func TestMatchFollowableRecords_ScopeFiltersClusterFromNamespaced(t *testing.T) {
	records := []typeset.TypeRecord{namespaceRecord()}

	// A namespaced selector never matches a cluster-scoped record, and vice versa.
	assert.Empty(t, matchFollowableRecords(
		records, nil, nil, []string{"namespaces"}, configv1alpha3.ResourceScopeNamespaced))
	assert.Len(t, matchFollowableRecords(
		records, nil, nil, []string{"namespaces"}, configv1alpha3.ResourceScopeCluster), 1)
}

func TestMatchFollowableRecords_WildcardResourceExpandsWithinScope(t *testing.T) {
	records := []typeset.TypeRecord{
		nsRecord("", "configmaps", "ConfigMap"),
		nsRecord("", "secrets", "Secret"),
		namespaceRecord(),
	}

	matched := matchFollowableRecords(
		records, []string{""}, []string{"v1"}, []string{"*"}, configv1alpha3.ResourceScopeNamespaced)

	kinds := map[string]bool{}
	for _, rec := range matched {
		kinds[rec.Identity.GVK.Kind] = true
	}
	assert.True(t, kinds["ConfigMap"] && kinds["Secret"])
	assert.False(t, kinds["Namespace"], "a cluster-scoped record must not match a namespaced selector")
}

func TestMatchFollowableRecords_VersionlessSelectorCollapsesToPreferred(t *testing.T) {
	records := []typeset.TypeRecord{
		followableRecord("example.com", "v1", "widgets", "Widget", typeset.ScopeNamespaced, true),
		followableRecord("example.com", "v1beta1", "widgets", "Widget", typeset.ScopeNamespaced, false),
	}

	matched := matchFollowableRecords(
		records, []string{"example.com"}, nil, []string{"widgets"}, configv1alpha3.ResourceScopeNamespaced)

	require.Len(t, matched, 1, "a version-less selector must not watch the same object under two versions")
	assert.Equal(t, "v1", matched[0].Identity.GVR.Version, "the preferred version wins")
}

func TestMatchFollowableRecords_OmittedGroupMultiGroupResourceIsAmbiguous(t *testing.T) {
	// The same resource name served in two groups, selected without an apiGroups filter,
	// is ambiguous: it must be watched in no group, not silently expanded across both.
	records := []typeset.TypeRecord{
		followableRecord("a.example.com", "v1", "widgets", "Widget", typeset.ScopeNamespaced, true),
		followableRecord("b.example.com", "v1", "widgets", "Widget", typeset.ScopeNamespaced, true),
	}

	assert.Empty(t, matchFollowableRecords(
		records, nil, nil, []string{"widgets"}, configv1alpha3.ResourceScopeNamespaced),
		"an omitted apiGroups selector over a multi-group resource is ambiguous")

	// Naming the group disambiguates it.
	matched := matchFollowableRecords(
		records, []string{"a.example.com"}, nil, []string{"widgets"}, configv1alpha3.ResourceScopeNamespaced)
	require.Len(t, matched, 1)
	assert.Equal(t, "a.example.com", matched[0].Identity.GVR.Group)
}

func TestMatchFollowableRecords_WildcardVersionKeepsEveryVersion(t *testing.T) {
	records := []typeset.TypeRecord{
		followableRecord("example.com", "v1", "widgets", "Widget", typeset.ScopeNamespaced, true),
		followableRecord("example.com", "v1beta1", "widgets", "Widget", typeset.ScopeNamespaced, false),
	}

	matched := matchFollowableRecords(
		records, []string{"example.com"}, []string{"*"}, []string{"widgets"},
		configv1alpha3.ResourceScopeNamespaced)

	assert.Len(t, matched, 2, "an explicit version wildcard keeps every served version")
}
