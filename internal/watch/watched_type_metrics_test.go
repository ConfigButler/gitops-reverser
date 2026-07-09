// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const watchedTypesMetric = "gitopsreverser_watched_types"

func TestRefreshWatchedTypeTables_RecordsResolvedTypeGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(clusterRuleForResource("rule-1", "configmaps"), rulestore.TargetBinding{
		GitTargetName:        "test-target",
		GitTargetNamespace:   "test-ns",
		GitProviderName:      "test-provider",
		GitProviderNamespace: "test-ns",
		Branch:               "main",
		Path:                 "test-path",
	})

	manager.refreshWatchedTypeTables()

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	value, ok := telemetry.CollectInt64Sum(reader, watchedTypesMetric, labels)
	require.True(t, ok, "expected a watched_types gauge sample")
	assert.Equal(t, int64(1), value)
}
