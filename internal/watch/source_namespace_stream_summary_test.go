// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// This is the regression test for the e2e-only bug PR 4 shipped with a first cut: an authorized
// override compiled and streamed, but StreamSummaryForWatchRule looked its stream up by the rule's
// OWN namespace while the stream is keyed on the WATCHED (effective source) namespace. The two
// differ exactly when spec.sourceNamespace is set, so the rule reported StreamsRunning=False —
// and therefore Ready=False — forever, even though its stream was live. Mock WatchManagers hid it;
// only the real summary path exercises the key.

func srcnsSummaryManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		Log:             logr.Discard(),
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

func srcnsOverrideRule() configv1alpha3.WatchRule {
	return configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-config-rule", Namespace: "tenant-acme"},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef:       configv1alpha3.LocalTargetReference{Name: "acme"},
			SourceNamespace: "repo-config",
			Rules:           []configv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
		},
	}
}

// TestStreamSummaryForWatchRule_KeysOnEffectiveSourceNamespace is the fix asserted: a stream keyed
// on the WATCHED namespace is found, and the rule reports StreamsRunning.
func TestStreamSummaryForWatchRule_KeysOnEffectiveSourceNamespace(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule()
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	// The data plane keyed the live stream on the SOURCE namespace ("repo-config").
	m.seedStreamState(gitDest,
		targetWatchKey{GVR: configmaps, Namespace: "repo-config"},
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
	rule := srcnsOverrideRule()
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	// A stream keyed on the CONFIG-PLANE namespace is a different stream entirely.
	m.seedStreamState(gitDest,
		targetWatchKey{GVR: configmaps, Namespace: "tenant-acme"},
		targetStreamStatus{state: StreamStateStreaming})

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Total, "the rule still expects its one type")
	assert.Equal(t, 0, summary.Ready, "but nothing ready is keyed under the watched namespace")
	assert.False(t, summary.StreamsRunning())
}

// TestStreamSummaryForWatchRule_LegacyRuleUnchanged: with no override the watched namespace IS the
// rule's own, so the legacy path is unaffected.
func TestStreamSummaryForWatchRule_LegacyRuleUnchanged(t *testing.T) {
	m := srcnsSummaryManager(t)
	rule := srcnsOverrideRule()
	rule.Spec.SourceNamespace = "" // legacy: watches its own namespace
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	m.seedStreamState(gitDest,
		targetWatchKey{GVR: configmaps, Namespace: "tenant-acme"},
		targetStreamStatus{state: StreamStateStreaming})

	summary := m.StreamSummaryForWatchRule(rule)

	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning())
}
