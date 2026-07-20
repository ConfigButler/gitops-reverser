// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

var configmapsGVRForScope = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

// cmManifestIn renders a ConfigMap in an explicit namespace, so a test can seed the same
// type in two namespaces and prove a scoped sweep touches only one of them.
func cmManifestIn(name, namespace, color string) string {
	return "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: " + name + "\n  namespace: " + namespace + "\n" +
		"data:\n  color: " + color + "\n"
}

// desiredCMIn builds a desired ConfigMap snapshot entry in an explicit namespace.
func desiredCMIn(name, namespace, color string) manifestanalyzer.DesiredResource {
	return manifestanalyzer.DesiredResource{
		Resource: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: name,
		},
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
			"data":       map[string]interface{}{"color": color},
		}},
	}
}

func TestResyncScope_MatchesRespectsTypeAndNamespace(t *testing.T) {
	cmInTeamA := types.ResourceIdentifier{
		Group: "", Version: "v1", Resource: "configmaps", Namespace: "team-a", Name: "cfg",
	}
	cmInTeamB := types.ResourceIdentifier{
		Group: "", Version: "v1", Resource: "configmaps", Namespace: "team-b", Name: "cfg",
	}
	secretInTeamA := types.ResourceIdentifier{
		Group: "", Version: "v1", Resource: "secrets", Namespace: "team-a", Name: "sec",
	}

	cases := []struct {
		name    string
		scope   *ResyncScope
		id      types.ResourceIdentifier
		matches bool
	}{
		{
			name: "a nil scope is the whole-GitTarget resync and matches everything",
			// Guards the fallback: BuildPlan, not BuildScopedPlan, must stay reachable.
			scope: nil, id: cmInTeamA, matches: true,
		},
		{
			name:  "an empty namespace is an all-namespaces scope for the type",
			scope: &ResyncScope{GVR: configmapsGVRForScope}, id: cmInTeamB, matches: true,
		},
		{
			name:  "a named namespace matches its own namespace",
			scope: &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}, id: cmInTeamA, matches: true,
		},
		{
			name: "a named namespace does NOT match a sibling namespace",
			// This single row is the defect: before the namespace half existed, this
			// returned true and the sibling namespace's document was swept.
			scope: &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}, id: cmInTeamB, matches: false,
		},
		{
			name:  "a sibling type never matches, even in the scoped namespace",
			scope: &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}, id: secretInTeamA, matches: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.matches, tc.scope.Matches(tc.id))
		})
	}
}

func TestResyncScope_StringIsNilSafeAndNamesTheNamespace(t *testing.T) {
	var nilScope *ResyncScope
	assert.Empty(t, nilScope.String(), "a whole-GitTarget resync has no scope string")

	allNamespaces := &ResyncScope{GVR: configmapsGVRForScope}
	assert.Equal(t, configmapsGVRForScope.String(), allNamespaces.String())

	scoped := &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}
	assert.Contains(t, scoped.String(), "team-a",
		"the namespace must appear in the scope string: it keys deferred heals, so two "+
			"namespaces of one type sharing a key would silently drop one heal")
	assert.NotEqual(t, allNamespaces.String(), scoped.String())
}

// Deferred heals are keyed by (GitTarget, scope). Once one GitTarget can watch a type in
// several namespaces, the key must separate them — otherwise stashing team-b's heal
// replaces team-a's parked one and team-a's drift is never corrected.
func TestResyncHealKey_SeparatesNamespacesOfTheSameType(t *testing.T) {
	req := func(ns string) *ResyncRequest {
		return &ResyncRequest{
			GitTargetName:      "team-a-config",
			GitTargetNamespace: "default",
			Scope:              &ResyncScope{GVR: configmapsGVRForScope, Namespace: ns},
		}
	}
	assert.NotEqual(t, resyncHealKey(req("team-a")), resyncHealKey(req("team-b")),
		"two namespaces of one type must key separately")
	assert.Equal(t, healKey{
		name: "team-a-config", namespace: "default",
		scope: (&ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}).String(),
	}, resyncHealKey(req("team-a")),
		"the key is stable for the same scope, so a re-stashed heal replaces rather than duplicates")
}

// THE test for this change. A replay covers one namespace, so its desired set names only
// that namespace's objects. The sweep must therefore be confined to that namespace: a
// GVR-only sweep would find team-b's document absent from desired and delete it, silently
// removing a namespace's manifests from the tenant's repository.
func TestResync_NamespaceScopedSweepLeavesSiblingNamespacesAlone(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	teamAFull := seedPlacedManifest(t, worktree, "team-a/cm.yaml", cmManifestIn("cfg", "team-a", "blue"))
	teamBFull := seedPlacedManifest(t, worktree, "team-b/cm.yaml", cmManifestIn("cfg", "team-b", "green"))

	// team-a replays and finds nothing: its namespace is empty at the pinned revision.
	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	scope := &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}
	stats, changed, err := w.applyResyncToWorktree(context.Background(), worktree, "", "", nil, scope, nil)
	require.NoError(t, err)
	require.True(t, changed, "team-a's orphaned document is swept")
	assert.Equal(t, 1, stats.Deleted, "exactly team-a's document is swept, not team-b's")

	_, teamAErr := os.Stat(teamAFull)
	assert.True(t, os.IsNotExist(teamAErr), "the scoped namespace's orphan is deleted")
	_, teamBErr := os.Stat(teamBFull)
	require.NoError(t, teamBErr,
		"a sibling namespace's document must survive a namespace-scoped replay of the same type")
}

// The narrowing must not disable the sweep inside its own namespace: an orphan in the
// scoped namespace is still dropped. Without this, "fixing" the defect by never sweeping
// would pass the test above.
func TestResync_NamespaceScopedSweepStillDropsOrphansInItsOwnNamespace(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	keptFull := seedPlacedManifest(t, worktree, "team-a/kept.yaml", cmManifestIn("kept", "team-a", "blue"))
	goneFull := seedPlacedManifest(t, worktree, "team-a/gone.yaml", cmManifestIn("gone", "team-a", "green"))

	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	scope := &ResyncScope{GVR: configmapsGVRForScope, Namespace: "team-a"}
	stats, changed, err := w.applyResyncToWorktree(
		context.Background(), worktree, "", "", []manifestanalyzer.DesiredResource{
			desiredCMIn("kept", "team-a", "blue"),
		}, scope, nil)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, 1, stats.Deleted, "the orphan inside the scoped namespace is still swept")

	_, keptErr := os.Stat(keptFull)
	require.NoError(t, keptErr, "a desired document in the scoped namespace is retained")
	_, goneErr := os.Stat(goneFull)
	assert.True(t, os.IsNotExist(goneErr), "an orphan in the scoped namespace is dropped")
}

// A genuinely cluster-wide stream (a ClusterWatchRule following a type across all
// namespaces) gathers every namespace, so its scope names none and its sweep must still
// cover the whole type. This is the behaviour the namespace half must NOT change.
func TestResync_ClusterWideScopeStillSweepsEveryNamespace(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	teamAFull := seedPlacedManifest(t, worktree, "team-a/cm.yaml", cmManifestIn("cfg", "team-a", "blue"))
	teamBFull := seedPlacedManifest(t, worktree, "team-b/cm.yaml", cmManifestIn("cfg", "team-b", "green"))

	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	scope := &ResyncScope{GVR: configmapsGVRForScope} // no namespace: all-namespaces
	stats, changed, err := w.applyResyncToWorktree(context.Background(), worktree, "", "", nil, scope, nil)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, 2, stats.Deleted, "an all-namespaces scope sweeps the type in every namespace")

	_, teamAErr := os.Stat(teamAFull)
	assert.True(t, os.IsNotExist(teamAErr))
	_, teamBErr := os.Stat(teamBFull)
	assert.True(t, os.IsNotExist(teamBErr))
}
