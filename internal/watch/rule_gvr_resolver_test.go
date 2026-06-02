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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

func TestRuleGVRResolver_OmittedGroupResolvesDeployment(t *testing.T) {
	resolver := NewRuleGVRResolver(newCommonTestCatalog(t))

	gvrs, misses := resolver.Resolve(nil, nil, []string{"deployments"}, configv1alpha1.ResourceScopeNamespaced)

	require.Empty(t, misses)
	require.Len(t, gvrs, 1)
	assert.Equal(t, "apps", gvrs[0].Group)
	assert.Equal(t, "v1", gvrs[0].Version)
	assert.Equal(t, "deployments", gvrs[0].Resource)
}

func TestRuleGVRResolver_AmbiguousOmittedGroupReturnsMiss(t *testing.T) {
	disco := newCommonTestDiscovery()
	disco.groups = append(disco.groups, testAPIGroup("team.example.com", "v1"))
	disco.resources = append(disco.resources, &metav1.APIResourceList{
		GroupVersion: "team.example.com/v1",
		APIResources: []metav1.APIResource{{
			Name:       "deployments",
			Kind:       "Deployment",
			Namespaced: true,
			Verbs:      metav1.Verbs{"list", "watch"},
		}},
	})
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)

	gvrs, misses := NewRuleGVRResolver(catalog).Resolve(
		nil,
		nil,
		[]string{"deployments"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	assert.Empty(t, gvrs)
	require.Len(t, misses, 1)
	assert.Equal(t, ResolveMissAmbiguous, misses[0].Reason)
	assert.Contains(t, misses[0].Detail, `"apps"`)
	assert.Contains(t, misses[0].Detail, `"team.example.com"`)
}

func TestRuleGVRResolver_WildcardGroupResolvesNamedResource(t *testing.T) {
	resolver := NewRuleGVRResolver(newCommonTestCatalog(t))

	gvrs, misses := resolver.Resolve(
		[]string{"*"},
		nil,
		[]string{"deployments"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	require.Empty(t, misses)
	require.Len(t, gvrs, 1)
	assert.Equal(t, GVR{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
		Scope:    configv1alpha1.ResourceScopeNamespaced,
	}, gvrs[0])
}

func TestRuleGVRResolver_WildcardResourceExpandsAllowedNamespacedResources(t *testing.T) {
	resolver := NewRuleGVRResolver(newCommonTestCatalog(t))

	gvrs, misses := resolver.Resolve(
		[]string{"", "apps", "shop.example.com"},
		nil,
		[]string{"*"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	require.Empty(t, misses)
	assert.ElementsMatch(t, []GVR{
		{Group: "", Version: "v1", Resource: "configmaps", Scope: configv1alpha1.ResourceScopeNamespaced},
		{Group: "", Version: "v1", Resource: "secrets", Scope: configv1alpha1.ResourceScopeNamespaced},
		{Group: "", Version: "v1", Resource: "services", Scope: configv1alpha1.ResourceScopeNamespaced},
		{Group: "apps", Version: "v1", Resource: "deployments", Scope: configv1alpha1.ResourceScopeNamespaced},
		{
			Group:    "shop.example.com",
			Version:  "v1alpha1",
			Resource: "customresources",
			Scope:    configv1alpha1.ResourceScopeNamespaced,
		},
		{
			Group:    "shop.example.com",
			Version:  "v1alpha1",
			Resource: "icecreamorders",
			Scope:    configv1alpha1.ResourceScopeNamespaced,
		},
	}, gvrs)
}

func TestRuleGVRResolver_WildcardVersionKeepsAllServedVersions(t *testing.T) {
	disco := newCommonTestDiscovery()
	disco.groups = append(disco.groups, &metav1.APIGroup{
		Name: "multi.example.com",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "multi.example.com/v1", Version: "v1"},
			{GroupVersion: "multi.example.com/v1beta1", Version: "v1beta1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "multi.example.com/v1",
			Version:      "v1",
		},
	})
	for _, version := range []string{"v1", "v1beta1"} {
		disco.resources = append(disco.resources, &metav1.APIResourceList{
			GroupVersion: "multi.example.com/" + version,
			APIResources: []metav1.APIResource{{
				Name:       "widgets",
				Kind:       "Widget",
				Namespaced: true,
				Verbs:      metav1.Verbs{"list", "watch"},
			}},
		})
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)

	gvrs, misses := NewRuleGVRResolver(catalog).Resolve(
		[]string{"multi.example.com"},
		[]string{"*"},
		[]string{"widgets"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	require.Empty(t, misses)
	assert.ElementsMatch(t, []GVR{
		{Group: "multi.example.com", Version: "v1", Resource: "widgets", Scope: configv1alpha1.ResourceScopeNamespaced},
		{
			Group:    "multi.example.com",
			Version:  "v1beta1",
			Resource: "widgets",
			Scope:    configv1alpha1.ResourceScopeNamespaced,
		},
	}, gvrs)
}

func TestRuleGVRResolver_OmittedVersionKeepsPreferredVersion(t *testing.T) {
	disco := newCommonTestDiscovery()
	disco.groups = append(disco.groups, &metav1.APIGroup{
		Name: "multi.example.com",
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "multi.example.com/v1", Version: "v1"},
			{GroupVersion: "multi.example.com/v1beta1", Version: "v1beta1"},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "multi.example.com/v1",
			Version:      "v1",
		},
	})
	for _, version := range []string{"v1", "v1beta1"} {
		disco.resources = append(disco.resources, &metav1.APIResourceList{
			GroupVersion: "multi.example.com/" + version,
			APIResources: []metav1.APIResource{{
				Name:       "widgets",
				Kind:       "Widget",
				Namespaced: true,
				Verbs:      metav1.Verbs{"list", "watch"},
			}},
		})
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)

	gvrs, misses := NewRuleGVRResolver(catalog).Resolve(
		[]string{"multi.example.com"},
		nil,
		[]string{"widgets"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	require.Empty(t, misses)
	require.Len(t, gvrs, 1)
	assert.Equal(t, "v1", gvrs[0].Version)
}

func TestRuleGVRResolver_DisallowedResourceReturnsPolicyMiss(t *testing.T) {
	disco := staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("batch", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "batch/v1",
			APIResources: []metav1.APIResource{{
				Name:       "jobs",
				Kind:       "Job",
				Namespaced: true,
				Verbs:      metav1.Verbs{"list", "watch"},
			}},
		}},
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)

	gvrs, misses := NewRuleGVRResolver(catalog).Resolve(
		[]string{"batch"},
		[]string{"v1"},
		[]string{"jobs"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	assert.Empty(t, gvrs)
	require.Len(t, misses, 1)
	assert.Equal(t, ResolveMissDisallowed, misses[0].Reason)
	assert.Contains(t, misses[0].Detail, defaultResourceExclusionReason)
}

func TestRuleGVRResolver_CatalogUnavailableFailsClosed(t *testing.T) {
	gvrs, misses := NewRuleGVRResolver(NewAPIResourceCatalog()).Resolve(
		nil,
		nil,
		[]string{"deployments"},
		configv1alpha1.ResourceScopeNamespaced,
	)

	assert.Empty(t, gvrs)
	require.Len(t, misses, 1)
	assert.Equal(t, ResolveMissCatalogUnavailable, misses[0].Reason)
}

func TestManager_NamespacesFollowResolvedNonCoreWatchRule(t *testing.T) {
	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "deployment-rule", Namespace: "apps-ns"},
		Spec: configv1alpha1.WatchRuleSpec{Rules: []configv1alpha1.ResourceRule{{
			Resources: []string{"deployments"},
		}}},
	}, "target", "apps-ns", "provider", "apps-ns", "main", "live")
	manager := &Manager{RuleStore: store, resourceCatalog: newCommonTestCatalog(t)}

	requested := manager.ComputeRequestedGVRs()

	require.Len(t, requested, 1)
	assert.Equal(t, []string{"apps-ns"}, manager.getNamespacesForGVR(requested[0]))
}
