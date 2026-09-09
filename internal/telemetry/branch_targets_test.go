// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const branchTargetsMetric = "gitopsreverser_git_branch_targets"

// branchTargetSeries reads the series for one GitTarget, and reports whether it exists at all —
// which is what the delete path is about.
func branchTargetSeries(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	namespace, name string,
) (int64, bool) {
	t.Helper()
	return CollectInt64Sum(reader, branchTargetsMetric, map[string]string{
		"gittarget_namespace": namespace,
		"gittarget_name":      name,
	})
}

// The whole point of the instrument: two GitTargets sharing one provider and branch each publish
// their own series, so a query keyed on {provider_namespace, provider_name, branch} fans out to
// both. One series per branch would answer the easy case and lose the case the ask was filed about.
func TestBranchTargets_TwoTargetsOnOneBranchEachPublish(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetBranchTarget("team-a", "mirror")
	defer ForgetBranchTarget("team-a", "docs")

	RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	RecordBranchTarget("team-a", "docs", "team-a", "acme", "main")

	for _, name := range []string{"mirror", "docs"} {
		value, ok := branchTargetSeries(t, reader, "team-a", name)
		require.True(t, ok, name)
		assert.Equal(t, int64(1), value, "an info gauge is always 1; the labels carry the mapping")
	}

	// The join keys have to be exactly the ones the Git-side instruments carry, or the vector
	// match silently returns nothing.
	value, ok := CollectInt64Sum(reader, branchTargetsMetric, map[string]string{
		"provider_namespace":  "team-a",
		"provider_name":       "acme",
		"branch":              "main",
		"gittarget_namespace": "team-a",
		"gittarget_name":      "mirror",
	})
	require.True(t, ok, "the destination labels must match providerAttrs' key names exactly")
	assert.Equal(t, int64(1), value)
}

// The delete path, and the half that has to be right. A join series that outlives its GitTarget
// keeps attributing a live branch's push failures to an object that no longer exists.
func TestBranchTargets_ForgetStopsTheSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)

	RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	_, ok := branchTargetSeries(t, reader, "team-a", "mirror")
	require.True(t, ok)

	ForgetBranchTarget("team-a", "mirror")
	_, ok = branchTargetSeries(t, reader, "team-a", "mirror")
	assert.False(t, ok, "a deleted GitTarget must leave no series, not a stale 1")
}

// Forgetting one target must not disturb another that shares its branch, which is the arrangement
// this instrument exists to describe.
func TestBranchTargets_ForgetIsScopedToOneTarget(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetBranchTarget("team-a", "docs")

	RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	RecordBranchTarget("team-a", "docs", "team-a", "acme", "main")

	ForgetBranchTarget("team-a", "mirror")

	_, ok := branchTargetSeries(t, reader, "team-a", "mirror")
	assert.False(t, ok)
	_, ok = branchTargetSeries(t, reader, "team-a", "docs")
	assert.True(t, ok, "a sibling on the same branch must be untouched")
}

// Recording is level rather than event: every reconcile publishes, and the series stays one.
// A counter here would climb with reconcile frequency and make the join meaningless.
func TestBranchTargets_RepeatedRecordStaysOneSeries(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetBranchTarget("team-a", "mirror")

	for range 5 {
		RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	}

	value, ok := branchTargetSeries(t, reader, "team-a", "mirror")
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}

// Two GitTargets with the same name in different namespaces are different objects writing to
// different providers. Keying on name alone would collapse them, and the mapping would name the
// wrong tenant's target for a branch failure.
func TestBranchTargets_NamespaceIsPartOfTheIdentity(t *testing.T) {
	reader, err := InitTestExporter()
	require.NoError(t, err)
	defer ForgetBranchTarget("team-a", "mirror")
	defer ForgetBranchTarget("team-b", "mirror")

	RecordBranchTarget("team-a", "mirror", "team-a", "acme", "main")
	RecordBranchTarget("team-b", "mirror", "team-b", "globex", "main")

	value, ok := CollectInt64Sum(reader, branchTargetsMetric, map[string]string{
		"gittarget_namespace": "team-b",
		"gittarget_name":      "mirror",
		"provider_name":       "globex",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), value)

	ForgetBranchTarget("team-a", "mirror")
	_, ok = branchTargetSeries(t, reader, "team-b", "mirror")
	assert.True(t, ok, "forgetting one namespace's target must not drop the other's")
}
