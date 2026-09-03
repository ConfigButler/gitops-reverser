// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// These are the SILENT-FAILURE guards. Every site they cover produces a stale watch rather than a
// visible failure: nothing errors, nothing is unready, the operator just quietly mirrors the wrong
// namespaces. Without these tests the omissions are invisible until someone notices in production.

// watchRuleOwnNamespace is the control-plane namespace every rule in these tests lives in; the
// point of each case is to vary the SOURCE namespaces against it.
const watchRuleOwnNamespace = "tenant-acme"

// watchRuleWithSource is watchRuleForTarget with an explicit rules[0].sourceNamespace, so a test can
// vary the SOURCE namespace independently of the namespace the rule object lives in.
func watchRuleWithSource(name, gitTargetName, sourceNamespace string) configv1alpha3.WatchRule {
	rule := watchRuleForTarget(name, gitTargetName, watchRuleOwnNamespace)
	rule.Spec.Rules[0].SourceNamespace = sourceNamespace
	return rule
}

// addRule compiles one WatchRule with an explicit resolved scope — exactly what CompileWatchRule
// would hand the store — so the planning assertions run against the real shape.
func addRule(store *rulestore.RuleStore, rule configv1alpha3.WatchRule, scope [][]string) {
	store.AddOrUpdateWatchRule(
		rule, scope, rule.Spec.GitTargetRef.Name, "test-ns", "test-provider", "test-ns", "main", "test-path")
}

// makeStoreWithScope compiles one WatchRule with a given resolved scope into a fresh store, for the
// tests that assert on the compiled form rather than on the resolved watch plan.
func makeStoreWithScope(rule configv1alpha3.WatchRule, scope [][]string) *rulestore.RuleStore {
	store := rulestore.NewStore()
	addRule(store, rule, scope)
	return store
}

// The watched-type table is only re-projected when the rules fingerprint changes, so the
// fingerprint must describe what is actually WATCHED rather than what was requested. Two
// byte-identical WatchRules compiled to different resolved sets must fingerprint differently, or a
// re-reconcile finds the fingerprint unchanged, skips the rebuild, and leaves every stream running
// at its old width with nothing anywhere reporting a problem.
//
// Resolution no longer reads another cluster, so this can no longer diverge behind the operator's
// back the way it could when a wildcard resolved through a GitTarget policy and a Namespace label
// snapshot. It is still asserted here, because the store compiles from the resolved set and the
// planner reads the store: a fingerprint keyed on anything else would be describing a different
// object than the one the plan is built from.
func TestWatchRuleFingerprint_ChangesWithResolvedSourceScope(t *testing.T) {
	wildcard := watchRuleWithSource("rule", "target", configv1alpha3.SourceNamespaceWildcard)

	narrow := makeStoreWithScope(wildcard, itemScope("repo-config")).SnapshotWatchRules()[0]
	widened := makeStoreWithScope(wildcard, itemScope("repo-config", "team-payments")).SnapshotWatchRules()[0]
	tightened := makeStoreWithScope(wildcard, itemScope("team-payments")).SnapshotWatchRules()[0]

	assert.NotEqual(t, watchRuleFingerprint(narrow), watchRuleFingerprint(widened),
		"WIDENING a policy under an untouched rule object must change the fingerprint")
	assert.NotEqual(t, watchRuleFingerprint(narrow), watchRuleFingerprint(tightened),
		"TIGHTENING a policy under an untouched rule object must change the fingerprint too")
	assert.Equal(t, watchRuleFingerprint(narrow),
		watchRuleFingerprint(makeStoreWithScope(wildcard, itemScope("repo-config")).SnapshotWatchRules()[0]),
		"an unchanged resolved scope must be stable, or every reconcile rebuilds the table")
}

// TestWatchRuleFingerprint_DiffersBySourceNamespace guards the explicit-name half of the same step.
func TestWatchRuleFingerprint_DiffersBySourceNamespace(t *testing.T) {
	rule := watchRuleWithSource("rule", "target", "repo-config")
	withOverride := makeStoreWithScope(rule, itemScope("repo-config")).SnapshotWatchRules()[0]

	other := watchRuleWithSource("rule", "target", "other-namespace")
	withDifferentOverride := makeStoreWithScope(other, itemScope("other-namespace")).SnapshotWatchRules()[0]

	legacyRule := watchRuleWithSource("rule", "target", "")
	legacy := makeStoreWithScope(legacyRule, ownNamespaceScope(legacyRule)).SnapshotWatchRules()[0]

	assert.NotEqual(t, watchRuleFingerprint(withOverride), watchRuleFingerprint(withDifferentOverride),
		"two rules differing ONLY in sourceNamespace must fingerprint differently, or a change to "+
			"the field never re-projects the watched-type table")
	assert.NotEqual(t, watchRuleFingerprint(withOverride), watchRuleFingerprint(legacy),
		"adding an override must re-project the table")
}

// TestCollectWatchRuleSelections_UsesResolvedSourceNamespace guards the selection step: the watch
// scope must be the item's RESOLVED source namespace, not the namespace the WatchRule object lives
// in.
func TestCollectWatchRuleSelections_UsesResolvedSourceNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	addRule(store,
		watchRuleWithSource("override-rule", "src-target", "repo-config"),
		itemScope("repo-config"))

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("src-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"repo-config"}, table.Types[0].WatchScopes(),
		"the stream must watch the SOURCE namespace, not the WatchRule's own namespace")
}

// A wildcard is ONE cluster-wide scope, not one scope per namespace. That is the whole efficiency
// case for the redefinition: a "*" rule over a type in a hundred-namespace cluster used to be a
// hundred watch connections and a hundred list calls at warm-up, each with its own cursor and its
// own share of the apiserver watch cache.
func TestCollectWatchRuleSelections_WildcardIsOneClusterWideScope(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	addRule(store,
		watchRuleWithSource("wild", "wild-target", configv1alpha3.SourceNamespaceWildcard),
		itemScope(""))

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("wild-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{""}, table.Types[0].WatchScopes())
	assert.True(t, table.Types[0].ClusterWide(),
		"a wildcard compiles to the all-namespaces collection, read once")
}

// A cluster-wide cell is a PEER of a named-namespace cell on the same type, never a replacement.
// Each rule carries its own operations filter, and collapsing the two once widened the named rule's
// stream to every namespace its credential could read while discarding that filter (CellKey's own
// doc comment records the bug). Two streams over overlapping objects is the correct outcome here.
func TestCollectWatchRuleSelections_WildcardIsAPeerOfANamedNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	addRule(store,
		watchRuleWithSource("wild", "shared-target", configv1alpha3.SourceNamespaceWildcard),
		itemScope(""))
	addRule(store,
		watchRuleWithSource("named", "shared-target", "repo-config"),
		itemScope("repo-config"))

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("shared-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"", "repo-config"}, table.Types[0].WatchScopes(),
		"the named scope survives beside the cluster-wide one; collapsing them loses its filter")
}

// TestCollectWatchRuleSelections_LegacyRuleStillWatchesItsOwnNamespace is the upgrade guarantee at
// the planning layer: with sourceNamespace omitted, nothing about the resolved scope changes.
func TestCollectWatchRuleSelections_LegacyRuleStillWatchesItsOwnNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	legacy := watchRuleForTarget("legacy-rule", "legacy-target", "tenant-acme")
	addRule(store, legacy, ownNamespaceScope(legacy))

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("legacy-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"tenant-acme"}, table.Types[0].WatchScopes())
}

// The invalidation twin one level up from the fingerprint: the resident table must actually
// RE-PROJECT when the resolved scope moved, not merely have its reconcile re-run. Here the rule
// object is byte-identical across both compiles and only what it compiled to changed — which is
// what a rule going from a named namespace to the cluster-wide wildcard looks like to the planner.
func TestWatchedTypeTable_RebuildsWhenOnlyTheResolvedScopeChanged(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	rule := watchRuleWithSource("rule", "policy-target", configv1alpha3.SourceNamespaceWildcard)

	addRule(store, rule, itemScope("repo-config"))
	manager.refreshWatchedTypeTables()
	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("policy-target"))
	require.True(t, ok)
	require.Equal(t, []string{"repo-config"}, table.Types[0].WatchScopes())

	addRule(store, rule, itemScope(""))
	manager.refreshWatchedTypeTables()

	table, ok = manager.watchedTypeTableForGitDest(gitDestRef("policy-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{""}, table.Types[0].WatchScopes(),
		"a changed resolution must re-project the resident table, not just re-run the reconcile")
}

// TestRefreshWatchedTypeTables_SourceNamespaceChangeReProjects is the end of the same chain for an
// explicit name: an edit to rules[].sourceNamespace must actually move the resolved watch scope.
func TestRefreshWatchedTypeTables_SourceNamespaceChangeReProjects(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	addRule(store,
		watchRuleWithSource("rule", "reproj-target", "repo-config"),
		itemScope("repo-config"))
	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("reproj-target"))
	require.True(t, ok)
	require.Equal(t, []string{"repo-config"}, table.Types[0].WatchScopes())

	// Same rule name, different source namespace — the update path, not a new rule.
	addRule(store,
		watchRuleWithSource("rule", "reproj-target", "moved-namespace"),
		itemScope("moved-namespace"))
	manager.refreshWatchedTypeTables()

	table, ok = manager.watchedTypeTableForGitDest(gitDestRef("reproj-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"moved-namespace"}, table.Types[0].WatchScopes(),
		"changing sourceNamespace must rebuild the watched-type table, not leave a stale watch")
}

// TestCompiledRule_SourceNamespacesAreSeparateFromTheRuleObject pins the field's meaning: Source
// names the WatchRule OBJECT in the control plane and each item's SourceNamespaces name the
// namespaces being mirrored. Collapsing them is the mistake the split exists to prevent.
func TestCompiledRule_SourceNamespacesAreSeparateFromTheRuleObject(t *testing.T) {
	store := makeStoreWithScope(
		watchRuleWithSource("rule", "target", "repo-config"), itemScope("repo-config"))

	compiled := store.SnapshotWatchRules()[0]

	assert.Equal(t, "tenant-acme", compiled.Source.Namespace, "the WatchRule object's namespace")
	assert.Equal(t, []string{"repo-config"}, compiled.ResourceRules[0].SourceNamespaces,
		"the namespaces being mirrored")
}
