/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const watchedTypesMetric = "gitopsreverser_watched_types"

func TestRefreshWatchedTypeTables_RecordsResolvedTypeGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	value, ok := telemetry.CollectInt64Sum(reader, watchedTypesMetric, labels)
	require.True(t, ok, "expected a watched_types gauge sample")
	assert.Equal(t, int64(1), value)
}
