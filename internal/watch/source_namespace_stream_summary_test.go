// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// This is the regression test for the e2e-only bug the first cut of this work shipped with: an
// authorized override compiled and streamed, but StreamSummaryForWatchRule looked its stream up by
// the rule's OWN namespace while the stream is keyed on the WATCHED namespace. The two differ
// exactly when a source namespace is set, so the rule reported StreamsRunning=False — and therefore
// Ready=False — forever, even though its stream was live. Mock WatchManagers hid it; only the real
// summary path exercises the key.
//
// PR 4 raises the stakes: a `sourceNamespace: "*"` item's namespace set does not exist in the spec
// AT ALL, so a summary rebuilt from the spec cannot even guess the keys. The roll-up therefore reads
// the COMPILED rule.

func srcnsSummaryManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		Log:             logr.Discard(),
		RuleStore:       rulestore.NewStore(),
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}
}

// seedStreamState records one target's per-key stream status, the surface the summary reads.
func (m *Manager) seedStreamState(gitDest types.ResourceReference, key targetWatchKey, status targetStreamStatus) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetStreamStates == nil {
		m.targetStreamStates = map[string]map[targetWatchKey]targetStreamStatus{}
	}
	states := m.targetStreamStates[gitDest.Key()]
	if states == nil {
		states = map[targetWatchKey]targetStreamStatus{}
		m.targetStreamStates[gitDest.Key()] = states
	}
	states[key] = status
}

func srcnsOverrideRule(sourceNamespace string) configv1alpha3.WatchRule {
	return configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-config-rule", Namespace: "tenant-acme"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: "acme"},
			Rules: []configv1alpha3.ResourceRule{{
				Resources: []string{"configmaps"}, SourceNamespace: sourceNamespace,
			}},
		},
	}
}

// compileForSummary seeds the store the way CompileWatchRule would, with an explicit resolved scope.
func compileForSummary(m *Manager, rule configv1alpha3.WatchRule, scope [][]string) {
	m.RuleStore.AddOrUpdateWatchRule(
		rule, scope, "acme", "tenant-acme", "git", "tenant-acme", "main", "tenants/acme")
}

func srcnsGitDest() types.ResourceReference {
	return types.NewResourceReference("acme", "tenant-acme")
}

func srcnsConfigMaps() schema.GroupVersionResource {
	return schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
}

// TestStreamSummaryForWatchRule_KeysOnResolvedSourceNamespace is the fix asserted: a stream keyed
// on the WATCHED namespace is found, and the rule reports StreamsRunning.
func TestStreamSummaryForWatchRule_KeysOnResolvedSourceNamespace(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule("repo-config")
	compileForSummary(m, rule, itemScope("repo-config"))

	// The data plane keyed the live stream on the SOURCE namespace ("repo-config").
	m.seedStreamState(srcnsGitDest(),
		targetWatchKey{GVR: srcnsConfigMaps(), Namespace: "repo-config"},
		targetStreamStatus{state: StreamStateStreaming})

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Total)
	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning(),
		"an override's readiness must be looked up by the WATCHED namespace, not the rule's own")
}

// TestStreamSummaryForWatchRule_WrongNamespaceKeyMisses is the negative proof: a stream keyed on
// the rule's OWN namespace (the bug's behavior) is NOT this rule's stream, so the summary correctly
// finds nothing ready. This pins the distinction so a future refactor cannot silently reintroduce
// the config-plane-namespace lookup.
func TestStreamSummaryForWatchRule_WrongNamespaceKeyMisses(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule("repo-config")
	compileForSummary(m, rule, itemScope("repo-config"))

	// A stream keyed on the CONFIG-PLANE namespace is a different stream entirely.
	m.seedStreamState(srcnsGitDest(),
		targetWatchKey{GVR: srcnsConfigMaps(), Namespace: "tenant-acme"},
		targetStreamStatus{state: StreamStateStreaming})

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Total, "the rule still expects its one type")
	assert.Equal(t, 0, summary.Ready, "but nothing ready is keyed under the watched namespace")
	assert.False(t, summary.StreamsRunning())
}

// TestStreamSummaryForWatchRule_WildcardReadsTheCompiledRule is the §5 hazard. A wildcard's resolved
// namespaces exist ONLY in the compiled rule, so a summary rebuilt from the spec would look for
// streams under keys that were never opened and report a perfectly healthy rule as permanently
// not-ready.
func TestStreamSummaryForWatchRule_WildcardReadsTheCompiledRule(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule(configv1alpha3.SourceNamespaceWildcard)
	compileForSummary(m, rule, itemScope("repo-config", "team-payments"))

	for _, ns := range []string{"repo-config", "team-payments"} {
		m.seedStreamState(srcnsGitDest(),
			targetWatchKey{GVR: srcnsConfigMaps(), Namespace: ns},
			targetStreamStatus{state: StreamStateStreaming})
	}

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Total, "the roll-up counts TYPES, not (type × namespace) streams")
	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning(),
		"a wildcard rule whose streams are running must report ready")
}

// TestStreamSummaryForWatchRule_WildcardWithOnePendingNamespaceIsNotReady: the roll-up folds every
// resolved namespace of a type, so one namespace still replaying holds the type back.
func TestStreamSummaryForWatchRule_WildcardWithOnePendingNamespaceIsNotReady(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule(configv1alpha3.SourceNamespaceWildcard)
	compileForSummary(m, rule, itemScope("repo-config", "team-payments"))

	m.seedStreamState(srcnsGitDest(),
		targetWatchKey{GVR: srcnsConfigMaps(), Namespace: "repo-config"},
		targetStreamStatus{state: StreamStateStreaming})
	// team-payments has no recorded state at all, which the roll-up reads as still replaying.

	summary := m.StreamSummaryForWatchRule(rule)

	assert.False(t, summary.StreamsRunning(),
		"one namespace of a wildcard still converging must hold the type back")
}

// TestStreamSummaryForWatchRule_UncompiledRuleExpectsNoStreams: a rule the gate refused (or one the
// store has not seeded yet) expects nothing, rather than inventing keys from its spec.
func TestStreamSummaryForWatchRule_UncompiledRuleExpectsNoStreams(t *testing.T) {
	m := srcnsSummaryManager(t)

	summary := m.StreamSummaryForWatchRule(srcnsOverrideRule("repo-config"))

	assert.Equal(t, 0, summary.Total)
	assert.False(t, summary.StreamsRunning())
}

// TestStreamSummaryForWatchRule_LegacyRuleUnchanged: with no override the watched namespace IS the
// rule's own, so the legacy path is unaffected.
func TestStreamSummaryForWatchRule_LegacyRuleUnchanged(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule("") // legacy: watches its own namespace
	compileForSummary(m, rule, ownNamespaceScope(rule))

	m.seedStreamState(srcnsGitDest(),
		targetWatchKey{GVR: srcnsConfigMaps(), Namespace: "tenant-acme"},
		targetStreamStatus{state: StreamStateStreaming})

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning())
}
