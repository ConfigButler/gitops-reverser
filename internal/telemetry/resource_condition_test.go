// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const resourceConditionMetric = "gitopsreverser_resource_condition"

// conditionSeries reads the one series for a (kind, object, condition type, status), and reports
// whether it exists at all — which is the property the delete path is about.
func conditionSeries(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	name, conditionType, status string,
) (int64, bool) {
	t.Helper()
	return CollectInt64Sum(reader, resourceConditionMetric, map[string]string{
		"kind":               "GitTarget",
		"resource_namespace": "team-a",
		"resource_name":      name,
		"type":               conditionType,
		"status":             status,
	})
}

// One condition publishes THREE series, one per possible status, of which exactly one is 1. That
// geometry is what makes `{status="False"} == 1` a selector and `count by (status)` a rollup; a
// single series whose value encoded the status would support neither.
func TestResourceCondition_PublishesOneSeriesPerStatus(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetResourceConditions("GitTarget", "team-a", "mirror")

	RecordResourceConditions("GitTarget", "team-a", "mirror", []ResourceConditionState{
		{Type: "Ready", Status: "False", Reason: "ValidationFailed"},
	})

	value, ok := conditionSeries(t, reader, "mirror", "Ready", "False")
	require.True(t, ok)
	assert.Equal(t, int64(1), value, "the status the object is in must be 1")

	for _, status := range []string{"True", "Unknown"} {
		value, ok := conditionSeries(t, reader, "mirror", "Ready", status)
		require.True(t, ok, "every possible status must publish a series, not only the current one")
		assert.Equal(t, int64(0), value, status)
	}

	value, ok = CollectInt64Sum(reader, resourceConditionMetric, map[string]string{
		"resource_name": "mirror",
		"type":          "Ready",
		"status":        "False",
		"reason":        "ValidationFailed",
	})
	require.True(t, ok, "the reason must be on the series; it is what turns a count into a diagnosis")
	assert.Equal(t, int64(1), value)
}

// A deleted object must stop publishing. Flux shipped this metric without a delete path and a
// deleted object reported Ready=False for two years' worth of releases; the alert never cleared,
// which is worse than no metric at all.
func TestResourceCondition_ForgetStopsTheSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	RecordResourceConditions("GitTarget", "team-a", "doomed", []ResourceConditionState{
		{Type: "Ready", Status: "False", Reason: "ValidationFailed"},
		{Type: "Stalled", Status: "True", Reason: "ValidationFailed"},
	})
	_, ok := conditionSeries(t, reader, "doomed", "Ready", "False")
	require.True(t, ok)

	ForgetResourceConditions("GitTarget", "team-a", "doomed")

	_, ok = conditionSeries(t, reader, "doomed", "Ready", "False")
	assert.False(t, ok, "a deleted object must publish nothing, not a latched last value")
	_, ok = conditionSeries(t, reader, "doomed", "Stalled", "True")
	assert.False(t, ok, "every condition of the object goes, not only the one named last")
}

// Forgetting one object leaves its neighbours alone. The delete is keyed on the whole identity, so
// two objects sharing a name across namespaces are not each other's delete.
func TestResourceCondition_ForgetIsScopedToOneObject(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetResourceConditions("GitTarget", "team-b", "mirror")

	for _, namespace := range []string{"team-a", "team-b"} {
		RecordResourceConditions("GitTarget", namespace, "mirror", []ResourceConditionState{
			{Type: "Ready", Status: "True", Reason: "Succeeded"},
		})
	}

	ForgetResourceConditions("GitTarget", "team-a", "mirror")

	_, ok := conditionSeries(t, reader, "mirror", "Ready", "True")
	assert.False(t, ok, "team-a's object was deleted")

	value, ok := CollectInt64Sum(reader, resourceConditionMetric, map[string]string{
		"resource_namespace": "team-b",
		"resource_name":      "mirror",
		"type":               "Ready",
		"status":             "True",
	})
	require.True(t, ok, "team-b's object was not")
	assert.Equal(t, int64(1), value)
}

// A reason that changes must not leave the old one behind. This is the one cardinality trap the
// label carries, and the whole-object replace is what defuses it: the map holds one entry per
// (object, condition), so the previous reason is gone before the next scrape reads it.
func TestResourceCondition_ChangedReasonLeavesNoStaleSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetResourceConditions("GitTarget", "team-a", "moving")

	RecordResourceConditions("GitTarget", "team-a", "moving", []ResourceConditionState{
		{Type: "Ready", Status: "False", Reason: "ProviderNotFound"},
	})
	RecordResourceConditions("GitTarget", "team-a", "moving", []ResourceConditionState{
		{Type: "Ready", Status: "True", Reason: "Succeeded"},
	})

	_, ok := CollectInt64Sum(reader, resourceConditionMetric, map[string]string{
		"resource_name": "moving",
		"reason":        "ProviderNotFound",
	})
	assert.False(t, ok, "the previous reason must not still be published")

	value, ok := CollectInt64Sum(reader, resourceConditionMetric, map[string]string{
		"resource_name": "moving",
		"type":          "Ready",
		"status":        "True",
		"reason":        "Succeeded",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}

// Publishing nothing is not a deletion. A caller that passes an empty slice has computed no
// conditions, which must not silently erase the object the way a real delete does.
func TestResourceCondition_EmptyUpdateIsNotADelete(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetResourceConditions("GitTarget", "team-a", "kept")

	RecordResourceConditions("GitTarget", "team-a", "kept", []ResourceConditionState{
		{Type: "Ready", Status: "True", Reason: "Succeeded"},
	})
	RecordResourceConditions("GitTarget", "team-a", "kept", nil)

	value, ok := conditionSeries(t, reader, "kept", "Ready", "True")
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}
