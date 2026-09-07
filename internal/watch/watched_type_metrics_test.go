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

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	value, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, labels)
	require.True(t, ok, "expected a watch_types gauge sample")
	assert.Equal(t, int64(1), value)
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

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	_, ok := telemetry.CollectInt64Sum(reader, watchTypesMetric, labels)
	require.True(t, ok)

	store.DeleteClusterWatchRule(k8stypes.NamespacedName{Name: "rule-1"})
	manager.refreshWatchedTypeTables()

	_, ok = telemetry.CollectInt64Sum(reader, watchTypesMetric, labels)
	assert.False(t, ok, "a GitTarget with no resolved types must publish no series")
}
