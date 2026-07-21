// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The two deletion paths are exercised separately here because spec.prune.mode is the only place
// in the product where they are controlled independently. `onEvent` — the effective default —
// differs from `always` on exactly one of them, so a test that only covered "does a delete
// happen" could not tell the two modes apart at all.

// resyncUnder folds desired over the worktree under one prune mode and returns the stats.
func resyncUnder(
	t *testing.T,
	worktree *gogit.Worktree,
	mode v1alpha3.PruneMode,
	desired ...manifestanalyzer.DesiredResource,
) ResyncStats {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	stats, _, err := w.applyResyncToWorktree(context.Background(), worktree, "", "", desired, nil, nil, mode)
	require.NoError(t, err)
	return stats
}

// deleteEventFor builds the object-less DELETE event the steady-state writer receives when a
// watched resource is removed from the source cluster.
func deleteEventFor(name string) Event {
	return Event{
		Operation: "DELETE",
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: "default", Name: name,
		},
	}
}

// deleteUnder folds one explicit DELETE event through the steady-state writer under one prune
// mode and reports whether anything changed.
func deleteUnder(t *testing.T, worktree *gogit.Worktree, mode v1alpha3.PruneMode, name string) bool {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	changed, err := w.flushEventsToWorktree(
		context.Background(), worktree, "", []Event{deleteEventFor(name)}, nil, mode)
	require.NoError(t, err)
	return changed
}

// TestPrune_OnEventRetainsWhenTheDesiredSetNarrowsToEmpty is the safety property PR 5 exists for.
//
// An empty desired set is exactly what a scope collapse, a source-cluster outage, or an older
// controller that does not understand a newer scope field produces. Under the default the mirror
// keeps its documents: nothing is deleted, nothing is counted as deleted, and the file is still
// on disk byte-for-byte.
func TestPrune_OnEventRetainsWhenTheDesiredSetNarrowsToEmpty(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seeded := cmManifest("orphan", "blue")
	full := seedPlacedManifest(t, worktree, "apps/orphan.yaml", seeded)

	stats := resyncUnder(t, worktree, v1alpha3.PruneOnEvent)

	assert.Zero(t, stats.Deleted, "onEvent must never infer a deletion from a desired snapshot")
	got, err := os.ReadFile(full)
	require.NoError(t, err, "the retained document must still exist")
	assert.Equal(t, seeded, string(got), "a retained document must be untouched, not rewritten")
}

// TestPrune_OnEventStillMirrorsAnExplicitDelete is the other half of the default: retention is
// about INFERENCE, not about deletion. Source-cluster evidence still removes the document, which
// is what keeps `onEvent` a usable default rather than a slow-growing archive.
func TestPrune_OnEventStillMirrorsAnExplicitDelete(t *testing.T) {
	worktree := newWorktreeForTest(t)
	full := seedPlacedManifest(t, worktree, "apps/app.yaml", cmManifest("app", "blue"))

	assert.True(t, deleteUnder(t, worktree, v1alpha3.PruneOnEvent, "app"),
		"an observed source DELETE must still be mirrored under onEvent")
	_, statErr := os.Stat(full)
	assert.True(t, os.IsNotExist(statErr), "the document must be removed from Git")
}

// TestPrune_NeverSuppressesBothPaths pins the archive mode. `never` is the only mode under which
// an explicit DELETE leaves the document in place, so this is what distinguishes it from the
// default — a test that only checked the sweep would pass for both.
func TestPrune_NeverSuppressesBothPaths(t *testing.T) {
	t.Run("explicit delete", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		seeded := cmManifest("app", "blue")
		full := seedPlacedManifest(t, worktree, "apps/app.yaml", seeded)

		assert.False(t, deleteUnder(t, worktree, v1alpha3.PruneNever, "app"),
			"never must not mirror a source DELETE, so the flush changes nothing")
		got, err := os.ReadFile(full)
		require.NoError(t, err)
		assert.Equal(t, seeded, string(got), "an archived document keeps its bytes")
	})

	t.Run("resync sweep", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		full := seedPlacedManifest(t, worktree, "apps/orphan.yaml", cmManifest("orphan", "blue"))

		stats := resyncUnder(t, worktree, v1alpha3.PruneNever)

		assert.Zero(t, stats.Deleted)
		_, statErr := os.Stat(full)
		assert.NoError(t, statErr, "never must not sweep either")
	})
}

// TestPrune_AlwaysReproducesMarkAndSweep proves `always` is the opt-in back to the pre-PR-5
// behaviour: the orphan is swept, and the resource the snapshot DOES name is still upserted in
// the same pass. Sweeping without upserting would be a different, much worse bug.
func TestPrune_AlwaysReproducesMarkAndSweep(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	orphan := seedPlacedManifest(t, worktree, "apps/orphan.yaml", cmManifest("orphan", "blue"))

	stats := resyncUnder(t, worktree, v1alpha3.PruneAlways, desiredCM("keep", "green"))

	assert.Equal(t, 1, stats.Deleted, "always restores the mark-and-sweep")
	assert.Equal(t, 1, stats.Created, "the desired resource is still created in the same pass")
	_, statErr := os.Stat(orphan)
	assert.True(t, os.IsNotExist(statErr), "the orphan is removed from Git")
	// Asserted by content rather than by path: where a new document lands is placement's
	// decision (here, sibling inference off the orphan's own directory), and this test is about
	// the sweep not swallowing the upsert — not about where the upsert went.
	assert.True(t, worktreeHoldsDocumentNamed(t, root, "keep"),
		"the resource present in the cluster is mirrored")
}

// worktreeHoldsDocumentNamed reports whether any YAML file under root names a metadata.name.
func worktreeHoldsDocumentNamed(t *testing.T, root, name string) bool {
	t.Helper()
	found := false
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(body), "name: "+name) {
			found = true
		}
		return nil
	}))
	return found
}

// TestPrune_RetentionIsIdenticalUnderEveryResyncShape closes the "no alternate sweep path" gap.
// A whole-target resync (nil scope, BuildPlan) and a namespace-scoped per-type resync (non-nil
// scope, BuildScopedPlan) reach the planner through two different functions; gating only one
// would leave a live deletion path uncontrolled. Production only ever issues the scoped shape,
// which is precisely why the unscoped one is the easy one to forget.
func TestPrune_RetentionIsIdenticalUnderEveryResyncShape(t *testing.T) {
	scope := &ResyncScope{GVR: configmapsGVRForScope, Namespace: "default"}

	for _, tc := range []struct {
		name  string
		scope *ResyncScope
	}{
		{"whole target", nil},
		{"namespace-scoped per-type", scope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := newWorktreeForTest(t)
			full := seedPlacedManifest(t, worktree, "apps/orphan.yaml", cmManifest("orphan", "blue"))
			w := &BranchWorker{
				contentWriter: newContentWriter(types.SensitiveResourcePolicy{}),
				mapper:        configMapMapper(),
			}

			stats, _, err := w.applyResyncToWorktree(
				context.Background(), worktree, "", "", nil, tc.scope, nil, v1alpha3.PruneOnEvent)
			require.NoError(t, err)

			assert.Zero(t, stats.Deleted)
			_, statErr := os.Stat(full)
			assert.NoError(t, statErr, "no resync shape may bypass the prune policy")
		})
	}
}

// TestPruneModeForBase_UnresolvableTargetFallsBackToOnEvent guards the lookup's failure mode. A
// base with no matching target (an event whose GitTarget metadata could not be resolved) must not
// pick up the zero value of the type: "" answers false to both predicates, so it would silently
// promote the target to `never` and stop mirroring deletes.
func TestPruneModeForBase_UnresolvableTargetFallsBackToOnEvent(t *testing.T) {
	targets := map[pendingTargetKey]ResolvedTargetMetadata{
		{Name: "known", Namespace: "default"}: {Path: "live", PruneMode: v1alpha3.PruneAlways},
		// Written by a path that predates the field, or by a struct literal in a test.
		{Name: "unset", Namespace: "default"}: {Path: "legacy"},
	}

	assert.Equal(t, v1alpha3.PruneAlways, pruneModeForBase(targets, "live"))
	assert.Equal(t, v1alpha3.PruneOnEvent, pruneModeForBase(targets, "legacy"),
		"a metadata entry with no mode is unset, which means onEvent")
	assert.Equal(t, v1alpha3.PruneOnEvent, pruneModeForBase(targets, "no-such-base"),
		"an unresolvable target must mirror deletes, not silently archive")
}

// TestShouldLogRetention_ThrottlesPerTarget pins the throttle. Retaining is a steady state and a
// resync fires per watched type and namespace, so an unthrottled default-verbosity line would
// scale with the reconcile rate — which is how a useful signal becomes noise nobody reads.
func TestShouldLogRetention_ThrottlesPerTarget(t *testing.T) {
	w := &BranchWorker{}

	assert.True(t, w.shouldLogRetention("tenant-a"), "the first retention for a target is reported")
	assert.False(t, w.shouldLogRetention("tenant-a"), "an immediate repeat is throttled")
	assert.True(t, w.shouldLogRetention("tenant-b"), "a different target is throttled independently")

	// Age the stamp past the interval: the next one reports again, so a long-lived retention does
	// not go permanently silent after its first line.
	w.retentionLoggedAt["tenant-a"] = w.retentionLoggedAt["tenant-a"].Add(-2 * retentionLogInterval)
	assert.True(t, w.shouldLogRetention("tenant-a"))
}
