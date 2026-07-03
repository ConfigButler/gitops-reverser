// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

type staticCatalogDiscovery struct {
	groups    []*metav1.APIGroup
	resources []*metav1.APIResourceList
	err       error
}

func (d staticCatalogDiscovery) ServerGroupsAndResources() (
	[]*metav1.APIGroup,
	[]*metav1.APIResourceList,
	error,
) {
	return d.groups, d.resources, d.err
}

func newCommonTestDiscovery() staticCatalogDiscovery {
	listWatch := metav1.Verbs{"get", "list", "watch"}
	return staticCatalogDiscovery{
		groups: []*metav1.APIGroup{
			testAPIGroup("", "v1"),
			testAPIGroup("apps", "v1"),
			testAPIGroup("networking.k8s.io", "v1"),
			testAPIGroup("apiextensions.k8s.io", "v1"),
			testAPIGroup("shop.example.com", "v1alpha1"),
		},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{
					{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: listWatch},
					{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: listWatch},
					{Name: "services", Kind: "Service", Namespaced: true, Verbs: listWatch},
					{Name: "namespaces", Kind: "Namespace", Verbs: listWatch},
					{Name: "nodes", Kind: "Node", Verbs: listWatch},
				},
			},
			{
				GroupVersion: "apps/v1",
				APIResources: []metav1.APIResource{
					{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: listWatch},
				},
			},
			{
				GroupVersion: "networking.k8s.io/v1",
				APIResources: []metav1.APIResource{
					{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: listWatch},
				},
			},
			{
				GroupVersion: "apiextensions.k8s.io/v1",
				APIResources: []metav1.APIResource{
					{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition", Verbs: listWatch},
				},
			},
			{
				GroupVersion: "shop.example.com/v1alpha1",
				APIResources: []metav1.APIResource{
					{Name: "customresources", Kind: "CustomResource", Namespaced: true, Verbs: listWatch},
					{Name: "icecreamorders", Kind: "IceCreamOrder", Namespaced: true, Verbs: listWatch},
				},
			},
		},
	}
}

func testAPIGroup(group, preferredVersion string) *metav1.APIGroup {
	groupVersion := preferredVersion
	if group != "" {
		groupVersion = group + "/" + preferredVersion
	}
	return &metav1.APIGroup{
		Name: group,
		Versions: []metav1.GroupVersionForDiscovery{{
			GroupVersion: groupVersion,
			Version:      preferredVersion,
		}},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: groupVersion,
			Version:      preferredVersion,
		},
	}
}

func newCommonTestCatalog(t *testing.T) *APIResourceCatalog {
	t.Helper()
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	return catalog
}

func commonTestDiscoveryClient() func() (apiResourceDiscovery, error) {
	return func() (apiResourceDiscovery, error) {
		return newCommonTestDiscovery(), nil
	}
}

// registryFromCatalog builds a registry from the catalog's latest scan under the given
// sensitive policy — the test-side equivalent of refreshTypeRegistry.
func registryFromCatalog(
	t *testing.T,
	catalog *APIResourceCatalog,
	sensitive types.SensitiveResourcePolicy,
) *typeset.Registry {
	t.Helper()
	scan, ok := catalog.Scan(sensitive)
	require.True(t, ok, "catalog should hold a trusted scan")
	reg := typeset.NewRegistry()
	reg.UpdateFromScan(scan)
	return reg
}
