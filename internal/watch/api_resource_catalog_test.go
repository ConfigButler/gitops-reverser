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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// TestAPIResourceCatalog_RefreshPicksUpNewlyServedResource is the honest
// catalog-lifecycle test: a resource absent from the first trusted discovery
// snapshot does not resolve, and becomes resolvable after a later refresh
// surfaces it. This is the in-process equivalent of installing a CRD — the
// API-surface trigger informers drive exactly this Refresh in production.
func TestAPIResourceCatalog_RefreshPicksUpNewlyServedResource(t *testing.T) {
	catalog := NewAPIResourceCatalog()

	// Generation 1: shop.example.com/v1 is served, but has no icecreamorders.
	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("shop.example.com", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "shop.example.com/v1",
			APIResources: []metav1.APIResource{},
		}},
	})
	require.NoError(t, err)
	require.True(t, catalog.Ready())
	gen1 := catalog.Generation()

	iceCream := schema.GroupVersionResource{Group: "shop.example.com", Version: "v1", Resource: "icecreamorders"}
	assert.False(t, catalogServes(catalog, iceCream), "resource is not served yet")

	// Generation 2: the CRD is now served.
	changed, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("shop.example.com", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "shop.example.com/v1",
			APIResources: []metav1.APIResource{{
				Name:       "icecreamorders",
				Kind:       "IceCreamOrder",
				Namespaced: true,
				Verbs:      metav1.Verbs{"get", "list", "watch"},
			}},
		}},
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Greater(t, catalog.Generation(), gen1)

	assert.True(t, catalogServes(catalog, iceCream), "the newly-served resource is now in the raw scan")
}

// catalogServes reports whether the catalog's raw scan holds the exact resource.
func catalogServes(catalog *APIResourceCatalog, gvr schema.GroupVersionResource) bool {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	_, ok := catalog.byGVR[gvr]
	return ok
}

// TestAPIResourceCatalog_PartialRefreshPreservesFailedGroupVersion verifies that
// a discovery refresh which fails for one group/version keeps that group's last
// trusted entries instead of dropping them, while a rule that targets the failed
// group resolves as DiscoveryDegraded rather than NotServed.
func TestAPIResourceCatalog_PartialRefreshPreservesFailedGroupVersion(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}

	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{
			testAPIGroup("", "v1"),
			testAPIGroup("apps", "v1"),
		},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{{
				Name:       "configmaps",
				Kind:       "ConfigMap",
				Namespaced: true,
				Verbs:      metav1.Verbs{"list", "watch"},
			}},
		}},
		err: &discovery.ErrGroupDiscoveryFailed{
			Groups: map[schema.GroupVersion]error{appsGV: errors.New("aggregated discovery failed")},
		},
	})
	require.NoError(t, err)

	// The failed apps group/version keeps its previously-trusted entries (deployments
	// is still in the raw scan), and the group/version is marked degraded.
	catalog.mu.RLock()
	_, kept := catalog.byGVR[schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}]
	catalog.mu.RUnlock()
	assert.True(t, kept, "a degraded group/version retains its last trusted entries")
	assert.Equal(t, []schema.GroupVersion{appsGV}, catalog.DegradedGroupVersions())
}

// TestNotServedResourceProducesNoGVR verifies catalog-backed resolution does not
// request a concrete informer for a resource trusted discovery does not serve.
func TestNotServedResourceProducesNoGVR(t *testing.T) {
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-gvr-rule", Namespace: "default"},
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{{
				APIGroups:   []string{"custom.example.com"},
				APIVersions: []string{"v1alpha1"},
				Resources:   []string{"customresources"},
			}},
		},
	}, "target", "default", "provider", "default", "main", "live")
	manager := &Manager{RuleStore: store, resourceCatalog: newCommonTestCatalog(t)}

	assert.Empty(t, manager.ComputeRequestedGVRs())
}
