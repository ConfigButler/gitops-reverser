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
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	catalogRefreshMetric   = "gitopsreverser_api_catalog_refresh_total"
	catalogResourcesMetric = "gitopsreverser_api_catalog_resources"
)

// policyMixDiscovery serves resources that span the default watch policy:
// configmaps/services are allowed; pods/events/leases are excluded.
func policyMixDiscovery() staticCatalogDiscovery {
	listWatch := metav1.Verbs{"get", "list", "watch"}
	return staticCatalogDiscovery{
		groups: []*metav1.APIGroup{
			testAPIGroup("", "v1"),
			testAPIGroup("coordination.k8s.io", "v1"),
		},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{
					{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: listWatch},
					{Name: "services", Kind: "Service", Namespaced: true, Verbs: listWatch},
					{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: listWatch},
					{Name: "events", Kind: "Event", Namespaced: true, Verbs: listWatch},
				},
			},
			{
				GroupVersion: "coordination.k8s.io/v1",
				APIResources: []metav1.APIResource{
					{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: listWatch},
				},
			},
		},
	}
}

func TestRefreshAPIResourceCatalog_RefreshMetrics(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager := &Manager{
		Log: logr.Discard(),
		discoveryClient: func() (apiResourceDiscovery, error) {
			return policyMixDiscovery(), nil
		},
	}
	ctx := context.Background()

	// First refresh populates the empty catalog: it must report changed.
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	changed, ok := telemetry.CollectInt64Sum(reader, catalogRefreshMetric,
		map[string]string{"outcome": "changed"})
	require.True(t, ok, "expected a changed api_catalog_refresh_total sample")
	assert.Equal(t, int64(1), changed)

	// Second refresh of identical discovery data must report unchanged.
	require.NoError(t, manager.RefreshAPIResourceCatalog(ctx))
	unchanged, ok := telemetry.CollectInt64Sum(reader, catalogRefreshMetric,
		map[string]string{"outcome": "unchanged"})
	require.True(t, ok, "expected an unchanged api_catalog_refresh_total sample")
	assert.Equal(t, int64(1), unchanged)

	// The excluded gauge reflects the default-watch-policy set: pods, events, leases.
	excluded, ok := telemetry.CollectInt64Sum(reader, catalogResourcesMetric,
		map[string]string{"state": "excluded"})
	require.True(t, ok, "expected an excluded api_catalog_resources sample")
	assert.Equal(t, int64(3), excluded)

	allowed, ok := telemetry.CollectInt64Sum(reader, catalogResourcesMetric,
		map[string]string{"state": "allowed"})
	require.True(t, ok, "expected an allowed api_catalog_resources sample")
	assert.Equal(t, int64(2), allowed)
}
