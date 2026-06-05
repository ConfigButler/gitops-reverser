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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	watchedTypesMetric         = "gitopsreverser_watched_types"
	watchedTypeConflictsMetric = "gitopsreverser_watched_type_conflicts"
)

func TestRefreshWatchedTypeTables_RecordsResolvedTypeGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	value, ok := telemetry.CollectInt64Sum(reader, watchedTypesMetric, labels)
	require.True(t, ok, "expected a watched_types gauge sample")
	assert.Equal(t, int64(1), value)

	conflicts, ok := telemetry.CollectInt64Sum(reader, watchedTypeConflictsMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(0), conflicts)
}

func TestRefreshWatchedTypeTables_RecordsConflictGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	store := rulestore.NewStore()
	manager := &Manager{
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: newWidgetConflictCatalog(t),
	}
	// A wildcard resource rule over the conflicting group resolves both widgets and
	// widgetslegacy, which share the Widget kind — the 1:1 violation.
	store.AddOrUpdateClusterWatchRule(
		configv1alpha1.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "rule-widgets"},
			Spec: configv1alpha1.ClusterWatchRuleSpec{
				TargetRef: configv1alpha1.NamespacedTargetReference{Name: "test-target", Namespace: "test-ns"},
				Rules: []configv1alpha1.ClusterResourceRule{{
					APIGroups:   []string{"example.com"},
					APIVersions: []string{"v1"},
					Resources:   []string{"*"},
					Scope:       configv1alpha1.ResourceScopeNamespaced,
				}},
			},
		},
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	labels := map[string]string{"gittarget_namespace": "test-ns", "gittarget_name": "test-target"}
	conflicts, ok := telemetry.CollectInt64Sum(reader, watchedTypeConflictsMetric, labels)
	require.True(t, ok, "expected a watched_type_conflicts gauge sample")
	assert.Equal(t, int64(1), conflicts)

	resolved, ok := telemetry.CollectInt64Sum(reader, watchedTypesMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(0), resolved, "a conflicting GVK is refused, not watched")
}
