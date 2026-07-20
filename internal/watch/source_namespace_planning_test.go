// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// These are the SILENT-FAILURE guards. Both sites they cover produce a stale watch rather than a
// visible failure: nothing errors, nothing is unready, the operator just quietly mirrors the wrong
// namespace. Without these tests the omissions are invisible until someone notices in production.

// watchRuleWithSource is watchRuleForTarget plus an explicit spec.sourceNamespace, so a test can
// vary the SOURCE namespace independently of the namespace the rule object lives in.
func watchRuleWithSource(name, gitTargetName, sourceNamespace string) configv1alpha3.WatchRule {
	rule := watchRuleForTarget(name, gitTargetName, watchRuleOwnNamespace)
	rule.Spec.SourceNamespace = sourceNamespace
	return rule
}

// watchRuleOwnNamespace is the control-plane namespace every rule in these tests lives in; the
// point of each case is to vary the SOURCE namespace against it.
const watchRuleOwnNamespace = "tenant-acme"

// makeStoreWithRule compiles one WatchRule into a fresh store, for the tests that assert on the
// compiled form rather than on the resolved watch plan.
func makeStoreWithRule(t *testing.T, rule configv1alpha3.WatchRule) *rulestore.RuleStore {
	t.Helper()
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(rule, "target", "test-ns", "test-provider", "test-ns", "main", "test-path")
	return store
}

// TestWatchRuleFingerprint_DiffersBySourceNamespace guards the fingerprint step. The watched-type
// table is only re-projected when the rules fingerprint changes, so if the fingerprint hashed the
// rule OBJECT's namespace instead of its SOURCE namespace, editing spec.sourceNamespace would
// leave the old watch running forever.
func TestWatchRuleFingerprint_DiffersBySourceNamespace(t *testing.T) {
	store := makeStoreWithRule(t, watchRuleWithSource("rule", "target", "repo-config"))
	withOverride := store.SnapshotWatchRules()[0]

	store = makeStoreWithRule(t, watchRuleWithSource("rule", "target", "other-namespace"))
	withDifferentOverride := store.SnapshotWatchRules()[0]

	store = makeStoreWithRule(t, watchRuleWithSource("rule", "target", ""))
	legacy := store.SnapshotWatchRules()[0]

	assert.NotEqual(t, watchRuleFingerprint(withOverride), watchRuleFingerprint(withDifferentOverride),
		"two rules differing ONLY in sourceNamespace must fingerprint differently, or a change to "+
			"the field never re-projects the watched-type table")
	assert.NotEqual(t, watchRuleFingerprint(withOverride), watchRuleFingerprint(legacy),
		"adding an override must re-project the table")
}

// TestCollectWatchRuleSelections_UsesEffectiveSourceNamespace guards the selection step: the watch
// scope must be the rule's SOURCE namespace, not the namespace the WatchRule object lives in.
func TestCollectWatchRuleSelections_UsesEffectiveSourceNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateWatchRule(
		watchRuleWithSource("override-rule", "src-target", "repo-config"),
		"src-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("src-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"repo-config"}, table.Types[0].WatchScopes(),
		"the stream must watch the SOURCE namespace, not the WatchRule's own namespace")
}

// TestCollectWatchRuleSelections_LegacyRuleStillWatchesItsOwnNamespace is the upgrade guarantee at
// the planning layer: with sourceNamespace omitted, nothing about the resolved scope changes.
func TestCollectWatchRuleSelections_LegacyRuleStillWatchesItsOwnNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("legacy-rule", "legacy-target", "tenant-acme"),
		"legacy-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("legacy-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"tenant-acme"}, table.Types[0].WatchScopes())
}

// TestRefreshWatchedTypeTables_SourceNamespaceChangeReProjects is the end of the same chain: an
// edit to spec.sourceNamespace must actually move the resolved watch scope. This is what the
// fingerprint guard above exists to make possible, asserted through the real re-projection path.
func TestRefreshWatchedTypeTables_SourceNamespaceChangeReProjects(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateWatchRule(
		watchRuleWithSource("rule", "reproj-target", "repo-config"),
		"reproj-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("reproj-target"))
	require.True(t, ok)
	require.Equal(t, []string{"repo-config"}, table.Types[0].WatchScopes())

	// Same rule name, different source namespace — the update path, not a new rule.
	store.AddOrUpdateWatchRule(
		watchRuleWithSource("rule", "reproj-target", "moved-namespace"),
		"reproj-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	table, ok = manager.watchedTypeTableForGitDest(gitDestRef("reproj-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"moved-namespace"}, table.Types[0].WatchScopes(),
		"changing sourceNamespace must rebuild the watched-type table, not leave a stale watch")
}

// TestCompiledRule_SourceNamespaceIsSeparateFromTheRuleObject pins the field's meaning: Source
// names the WatchRule OBJECT in the control plane and SourceNamespace names the namespace being
// mirrored. Collapsing them is the mistake this field exists to prevent.
func TestCompiledRule_SourceNamespaceIsSeparateFromTheRuleObject(t *testing.T) {
	store := makeStoreWithRule(t, watchRuleWithSource("rule", "target", "repo-config"))

	compiled := store.SnapshotWatchRules()[0]

	assert.Equal(t, "tenant-acme", compiled.Source.Namespace, "the WatchRule object's namespace")
	assert.Equal(t, "repo-config", compiled.SourceNamespace, "the namespace being mirrored")
}
