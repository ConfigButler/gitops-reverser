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

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// snapshotDeliveryTestDiscovery returns a trusted-but-minimal discovery that
// serves only an unrelated resource. The snapshot-delivery tests register
// configmaps rules purely to exercise rule-set-change snapshot emission; with
// configmaps absent from trusted discovery, rule resolution requests no informer
// GVRs, so those tests never plan real informers. This replaces the old
// discoveryFilter stub that returned no GVRs.
func snapshotDeliveryTestDiscovery() staticCatalogDiscovery {
	return staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "nodes", Kind: "Node", Verbs: metav1.Verbs{"get", "list", "watch"}},
			},
		}},
	}
}

func newSnapshotDeliveryTestCatalog(t *testing.T) *APIResourceCatalog {
	t.Helper()
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(snapshotDeliveryTestDiscovery())
	require.NoError(t, err)
	return catalog
}

func snapshotDeliveryTestDiscoveryClient() func() (apiResourceDiscovery, error) {
	return func() (apiResourceDiscovery, error) {
		return snapshotDeliveryTestDiscovery(), nil
	}
}
