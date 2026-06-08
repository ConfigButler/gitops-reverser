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
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// followableDiscovery serves a realistic mix the funnel must judge: a followable
// built-in (deployments, with a /scale subresource), a followable CRD, a
// policy-denied built-in (pods), and a built-in missing the watch verb (nodes).
func followableDiscovery() staticCatalogDiscovery {
	full := metav1.Verbs{"get", "list", "watch", "patch", "create", "delete"}
	// getList lacks watch, so a type with these verbs cannot be followed.
	getList := metav1.Verbs{"get", "list"}
	return staticCatalogDiscovery{
		groups: []*metav1.APIGroup{
			testAPIGroup("", "v1"),
			testAPIGroup("apps", "v1"),
			testAPIGroup("shop.example.com", "v1alpha1"),
		},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{
					{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: full},
					{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: full},
					{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: full},
					{Name: "nodes", Kind: "Node", Verbs: getList},
				},
			},
			{
				GroupVersion: "apps/v1",
				APIResources: []metav1.APIResource{
					{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: full},
					{Name: "deployments/scale", Kind: "Scale", Namespaced: true, Verbs: metav1.Verbs{"get", "patch"}},
				},
			},
			{
				GroupVersion: "shop.example.com/v1alpha1",
				APIResources: []metav1.APIResource{
					{Name: "icecreamorders", Kind: "IceCreamOrder", Namespaced: true, Verbs: full},
				},
			},
		},
	}
}

func recordByGVR(t *testing.T, r *typeset.Registry, group, version, resource string) typeset.TypeRecord {
	t.Helper()
	rec, ok := r.ByGVR(schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
	require.True(t, ok, "registry should know %s/%s/%s", group, version, resource)
	return rec
}

func TestObservations_FollowabilityFromCatalog(t *testing.T) {
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(followableDiscovery())
	require.NoError(t, err)

	reg := typeset.NewRegistry()
	reg.Update(catalog.Observations(types.SensitiveResourcePolicy{}), catalog.Generation())

	// Deployment: followable built-in with a usable scale binding folded in.
	dep := recordByGVR(t, reg, "apps", "v1", "deployments")
	assert.True(t, dep.Followable(), "deployment should be followable: %s", dep.Followability.Summary)
	assert.Equal(t, typeset.OriginBuiltin, dep.Origin.Kind)
	assert.True(t, dep.Subresources.Scale.Enabled, "deployment exposes /scale")
	assert.True(t, dep.Subresources.Scale.Usable, "built-in scale binding is usable")
	assert.Equal(t, ".spec.replicas", dep.Subresources.Scale.SpecReplicasPath)

	// CRD: followable, classified crd by group shape.
	order := recordByGVR(t, reg, "shop.example.com", "v1alpha1", "icecreamorders")
	assert.True(t, order.Followable())
	assert.Equal(t, typeset.OriginCRD, order.Origin.Kind)

	// Pod: served and fully verbed but denied by default watch policy.
	pod := recordByGVR(t, reg, "", "v1", "pods")
	assert.False(t, pod.Followable())
	policy, _ := pod.Followability.Check(typeset.RequirementPolicy)
	assert.Equal(t, typeset.ReasonDeniedByPolicy, policy.Reason)

	// Node: built-in lacking watch -> refused for the missing verb (cannot follow it).
	node := recordByGVR(t, reg, "", "v1", "nodes")
	assert.False(t, node.Followable())
	verbs, _ := node.Followability.Check(typeset.RequirementVerbs)
	assert.Equal(t, typeset.ReasonMissingVerb, verbs.Reason)
	assert.Equal(t, "watch", verbs.Detail)

	// Secret is sensitive but supported, so it stays followable.
	secret := recordByGVR(t, reg, "", "v1", "secrets")
	assert.True(t, secret.Sensitive)
	assert.True(t, secret.Followable())

	// The scale subresource itself is folded into the parent, never its own record.
	_, ok := reg.ByGVR(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments/scale"})
	assert.False(t, ok, "subresources must not enter the registry as their own types")
}

func TestObservations_AppliesConfiguredSensitivePolicy(t *testing.T) {
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(followableDiscovery())
	require.NoError(t, err)

	// The operator additionally marks configmaps sensitive; core secrets stay sensitive
	// by default. The registry record must reflect the configured policy, not just the
	// built-in core-secret rule.
	policy, err := types.ParseSensitiveResourcePolicy("configmaps")
	require.NoError(t, err)

	reg := typeset.NewRegistry()
	reg.Update(catalog.Observations(policy), catalog.Generation())

	cm := recordByGVR(t, reg, "", "v1", "configmaps")
	assert.True(t, cm.Sensitive, "an operator-configured sensitive type must be marked sensitive")
	secret := recordByGVR(t, reg, "", "v1", "secrets")
	assert.True(t, secret.Sensitive, "core secrets stay sensitive")
	dep := recordByGVR(t, reg, "apps", "v1", "deployments")
	assert.False(t, dep.Sensitive, "an unlisted type is not sensitive")
}

func TestObservations_AmbiguousGVKMarkedNonUnique(t *testing.T) {
	full := metav1.Verbs{"get", "list", "watch", "patch"}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("shop.example.com", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "shop.example.com/v1",
			APIResources: []metav1.APIResource{
				// Two resources serving the same kind: a pathological cluster.
				{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: full},
				{Name: "widgetz", Kind: "Widget", Namespaced: true, Verbs: full},
			},
		}},
	})
	require.NoError(t, err)

	reg := typeset.NewRegistry()
	reg.Update(catalog.Observations(types.SensitiveResourcePolicy{}), catalog.Generation())

	rec, ok := reg.ByGVK(schema.GroupVersionKind{Group: "shop.example.com", Version: "v1", Kind: "Widget"})
	require.True(t, ok)
	assert.False(t, rec.Followable(), "an ambiguous kind must be refused")
	id, _ := rec.Followability.Check(typeset.RequirementIdentity)
	assert.Equal(t, typeset.ReasonGVKNotUnique, id.Reason)
	assert.Equal(t, "widgets, widgetz", id.Detail)
}

func TestManager_RefreshPopulatesTypeRegistry(t *testing.T) {
	m := &Manager{Log: logr.Discard(), discoveryClient: func() (apiResourceDiscovery, error) {
		return followableDiscovery(), nil
	}}
	require.NoError(t, m.RefreshAPIResourceCatalog(context.Background()))

	followable := m.FollowableTypeRecords()
	all := m.TypeRecords()
	assert.NotEmpty(t, followable, "deployments/configmaps/secrets/icecreamorders should be followable")
	assert.Greater(t, len(all), len(followable), "All() must also include refused types (pods, nodes)")

	// The registry tracks the catalog generation.
	assert.Equal(t, m.apiResourceCatalog().Generation(), m.typeRegistryInstance().Generation())

	gvks := map[string]bool{}
	for _, rec := range followable {
		gvks[rec.Identity.GVK.Kind] = true
	}
	assert.True(t, gvks["Deployment"], "Deployment should be followable")
	assert.True(t, gvks["IceCreamOrder"], "the CRD should be followable")
	assert.False(t, gvks["Pod"], "Pod is denied by policy")
	assert.False(t, gvks["Node"], "Node is missing the watch verb")
}
