// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const watchTypesMetric = "gitopsreverser_watch_types"

// Summing the gauge gives the resolved-type count the old `watched_types` published: the
// CONFIGURATION view, "how many types does this GitTarget claim".
func TestWatchTypeSamples_SumIsTheResolvedTypeCount(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	manager.installWatchTypeGaugeSource()
	defer manager.clearWatchTypeGaugeSource()

	// Asserted per state, because each state is its own series: a resolved type that is not yet
	// streaming is `replaying`, and the sum across states is the resolved-type count.
	replaying, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("replaying"))
	require.True(t, ok, "expected a watch_types gauge sample")
	assert.Equal(t, int64(1), replaying, "the resolved type has no running stream yet")

	streaming, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("streaming"))
	require.True(t, ok)
	assert.Equal(t, int64(0), streaming)
}

// typeStateLabels selects one state's series for the test GitTarget.
func typeStateLabels(state string) map[string]string {
	return map[string]string{
		"gittarget_namespace": "test-ns", "gittarget_name": "test-target", "state": state,
	}
}

// A GitTarget whose declarations are gone must stop publishing, rather than latching its last
// count forever. The gauge it replaces was pushed, so a departed target had to be zeroed by hand;
// a callback has nothing to latch.
func TestWatchTypeSamples_DepartedTargetStopsPublishing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	manager.installWatchTypeGaugeSource()
	defer manager.clearWatchTypeGaugeSource()

	_, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("replaying"))
	require.True(t, ok)

	store.DeleteClusterWatchRule(k8stypes.NamespacedName{Name: "rule-1"})
	manager.refreshWatchedTypeTables()

	_, ok = telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("replaying"))
	assert.False(t, ok, "a GitTarget with no resolved types must publish no series")
}

// The scrape path must never trigger a re-resolution.
//
// This is a regression pin for a real e2e failure, not a hypothetical. The first version of this
// source called StreamSummaryForGitTarget, which calls watchedTypeTableForGitDest, which calls
// refreshWatchedTypeTables -- a discovery-backed rebuild of every cluster's type registry, run on
// the Prometheus scrape goroutine once per target per scrape. Against a wildcard rule resolving 58
// types it starved the replaying streams and the GitTarget sat at 0/58 running until the spec timed
// out.
//
// The property is asserted the only way it can be from outside: add a rule WITHOUT refreshing, and
// require the scrape to still report the previous resolution. A source that refreshes would pick
// the new rule up and fail here.
func TestWatchTypeSamples_DoesNotResolveOnTheScrapePath(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	manager.installWatchTypeGaugeSource()
	defer manager.clearWatchTypeGaugeSource()

	before, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("replaying"))
	require.True(t, ok)

	// A second type is declared, and nothing refreshes the tables.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-2", "nodes"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	after, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, typeStateLabels("replaying"))
	require.True(t, ok)
	assert.Equal(t, before, after,
		"the scrape must report the last published resolution, never resolve one of its own")
}
