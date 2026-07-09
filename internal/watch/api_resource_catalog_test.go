// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
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

// catalogServes reports whether the catalog's latest normalized scan holds the exact
// resource as a served entry.
func catalogServes(catalog *APIResourceCatalog, gvr schema.GroupVersionResource) bool {
	scan, ok := catalog.Scan(types.SensitiveResourcePolicy{})
	if !ok {
		return false
	}
	for _, e := range scan.Entries {
		if e.GVR == gvr {
			return true
		}
	}
	return false
}

// TestAPIResourceCatalog_PartialRefreshPreservesFailedGroupVersion verifies the
// retain-on-error pipeline post-relocation: the catalog reports the failed
// group/version as a per-scan fact (no entries for it, listed degraded), and the
// REGISTRY — which now owns all cross-scan judgement — keeps that group's last
// trusted records serving as retained/DiscoveryDegraded rather than dropping them.
// The pure leaf-level semantics are covered in typeset (TestUpdateFromScan_*).
func TestAPIResourceCatalog_PartialRefreshPreservesFailedGroupVersion(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}
	deployments := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	reg := registryFromCatalog(t, catalog, types.SensitiveResourcePolicy{})
	if rec, ok := reg.ByGVR(deployments); !ok || !rec.Followable() {
		t.Fatalf("seed: deployments should be followable, ok=%v", ok)
	}

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

	// The catalog's scan carries the failure as a fact: no apps entries, listed degraded.
	assert.False(t, catalogServes(catalog, deployments),
		"a failed group/version contributes no entries to the per-scan facts")
	assert.Equal(t, []schema.GroupVersion{appsGV}, catalog.DegradedGroupVersions())

	// The registry retains the last trusted record, refused only as DiscoveryDegraded.
	scan, ok := catalog.Scan(types.SensitiveResourcePolicy{})
	require.True(t, ok)
	reg.UpdateFromScan(scan)
	rec, ok := reg.ByGVR(deployments)
	require.True(t, ok, "a degraded group/version retains its last trusted records")
	assert.Equal(t, typeset.VerdictRetained, rec.Followability.Verdict)
	check, _ := rec.Followability.Check(typeset.RequirementTrusted)
	assert.Equal(t, typeset.ReasonDiscoveryDegraded, check.Reason)
}

// TestNotServedResourceProducesNoGVR verifies catalog-backed resolution does not
// request a concrete informer for a resource trusted discovery does not serve.
func TestNotServedResourceProducesNoGVR(t *testing.T) {
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-gvr-rule", Namespace: "default"},
		Spec: configv1alpha3.WatchRuleSpec{
			Rules: []configv1alpha3.ResourceRule{{
				APIGroups:   []string{"custom.example.com"},
				APIVersions: []string{"v1alpha1"},
				Resources:   []string{"customresources"},
			}},
		},
	}, rulestore.TargetBinding{
		GitTargetName:        "target",
		GitTargetNamespace:   "default",
		GitProviderName:      "provider",
		GitProviderNamespace: "default",
		Branch:               "main",
		Path:                 "live",
	})
	manager := &Manager{RuleStore: store, resourceCatalog: newCommonTestCatalog(t)}

	assert.Empty(t, manager.ComputeRequestedGVRs())
}
