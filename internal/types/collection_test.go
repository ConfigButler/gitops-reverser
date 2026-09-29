// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The property the whole type exists for: the served version is not identity. Two collections of
// one logical resource that differ only in served version are ONE collection, because they are one
// sweep boundary — the discrepancy recorded in docs/design/target-watch-plan.md,
// "Diff the plan".
func TestCollectionKeyFor_DropsTheServedVersion(t *testing.T) {
	v1 := CollectionKeyFor(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "team-a")
	v1beta1 := CollectionKeyFor(
		schema.GroupVersionResource{Group: "apps", Version: "v1beta1", Resource: "deployments"}, "team-a")

	assert.Equal(t, v1, v1beta1, "one logical resource in one namespace is one collection")
	assert.Equal(t, v1.String(), v1beta1.String(), "and one map key, wherever the string is the key")
}

func TestCollectionKey_StringNamesTheTypeAndTheNamespace(t *testing.T) {
	clusterWide := CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	assert.Equal(t, "configmaps", clusterWide.String())

	grouped := CollectionKeyFor(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "")
	assert.Equal(t, "deployments.apps", grouped.String())

	scoped := CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "team-a")
	assert.Equal(t, "configmaps in team-a", scoped.String())
	assert.NotEqual(t, clusterWide.String(), scoped.String(),
		"a cluster-wide collection is a PEER of a named namespace, never the same key")
}

func TestCollectionKey_Matches(t *testing.T) {
	cmInTeamA := NewResourceIdentifier("", "v1", "configmaps", "team-a", "app")
	cmInTeamB := NewResourceIdentifier("", "v1", "configmaps", "team-b", "app")
	deployInTeamA := NewResourceIdentifier("apps", "v1", "deployments", "team-a", "app")

	clusterWide := CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	assert.True(t, clusterWide.Matches(cmInTeamA))
	assert.True(t, clusterWide.Matches(cmInTeamB), "an empty namespace is every namespace")
	assert.False(t, clusterWide.Matches(deployInTeamA))

	scoped := CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "team-a")
	assert.True(t, scoped.Matches(cmInTeamA))
	assert.False(t, scoped.Matches(cmInTeamB))

	// A resource observed at another served version is still inside the collection: the mirror
	// stores it at one versionless path, so a sweep that skipped it would leave it unmanaged.
	assert.True(t, scoped.Matches(NewResourceIdentifier("", "v2", "configmaps", "team-a", "app")))
}

func TestCollectionKey_OverlapsIsStructural(t *testing.T) {
	named := CollectionKey{Resource: "secrets", Namespace: "a", LabelSelector: "team in (a)"}
	for _, tc := range []struct {
		other CollectionKey
		want  bool
	}{
		{CollectionKey{Resource: "secrets", Namespace: "a"}, true},
		{CollectionKey{Resource: "secrets"}, true},
		{CollectionKey{Resource: "secrets", Namespace: "b"}, false},
		{CollectionKey{Resource: "configmaps", Namespace: "a"}, false},
		{CollectionKey{Group: "x", Resource: "secrets", Namespace: "a"}, false},
	} {
		if got := named.Overlaps(tc.other); got != tc.want {
			t.Errorf("%s overlaps %s = %v, want %v", named, tc.other, got, tc.want)
		}
	}
}
