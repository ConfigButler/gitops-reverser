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
)

// TestCatalogLookupGVK_Resolved verifies an exact GVK lookup returns the single
// served entry, and that the answer carries readiness and generation.
func TestCatalogLookupGVK_Resolved(t *testing.T) {
	catalog := newCommonTestCatalog(t)

	got := catalog.LookupGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})

	require.Len(t, got.Entries, 1)
	assert.Equal(t, "deployments", got.Entries[0].GVR.Resource)
	assert.True(t, got.Entries[0].Namespaced)
	assert.True(t, got.Ready)
	assert.False(t, got.Degraded)
	assert.Equal(t, catalog.Generation(), got.Generation)
}

// TestCatalogLookupGVK_ExactMatchOnly proves the lookup never bridges API
// versions: a kind served only at apps/v1 does not answer for extensions/v1beta1.
func TestCatalogLookupGVK_ExactMatchOnly(t *testing.T) {
	catalog := newCommonTestCatalog(t)

	got := catalog.LookupGVK(schema.GroupVersionKind{Group: "extensions", Version: "v1beta1", Kind: "Deployment"})

	assert.Empty(t, got.Entries)
	assert.True(t, got.Ready)
	assert.False(t, got.Degraded)
}

// TestCatalogLookupGVK_NotReady distinguishes an empty answer from an unready
// catalog: before any trusted discovery, Ready is false so callers treat the
// miss as "catalog unavailable", not "unserved".
func TestCatalogLookupGVK_NotReady(t *testing.T) {
	catalog := NewAPIResourceCatalog()

	got := catalog.LookupGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})

	assert.Empty(t, got.Entries)
	assert.False(t, got.Ready)
	assert.False(t, got.Degraded)
}

// TestCatalogLookupGVK_Degraded verifies a lookup against a group/version whose
// discovery currently fails reports Degraded, so absence is not trusted.
func TestCatalogLookupGVK_Degraded(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}

	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("apps", "v1")},
		err: &discovery.ErrGroupDiscoveryFailed{
			Groups: map[schema.GroupVersion]error{appsGV: errors.New("aggregated discovery failed")},
		},
	})
	require.NoError(t, err)

	got := catalog.LookupGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"})
	assert.Empty(t, got.Entries)
	assert.True(t, got.Degraded)

	// A healthy group/version is unaffected.
	core := catalog.LookupGVK(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.Len(t, core.Entries, 1)
	assert.False(t, core.Degraded)
}

// TestCatalogLookupGVK_Subresource verifies a resource and its kind-sharing
// subresource both appear under the same GVK so the mapper can detect a
// subresource-only situation.
func TestCatalogLookupGVK_Subresource(t *testing.T) {
	catalog := NewAPIResourceCatalog()
	listWatch := metav1.Verbs{"get", "list", "watch"}
	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("apps", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: listWatch},
				{Name: "deployments/status", Kind: "Deployment", Namespaced: true, Verbs: listWatch},
			},
		}},
	})
	require.NoError(t, err)

	got := catalog.LookupGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.Len(t, got.Entries, 2)

	var served, sub int
	for _, e := range got.Entries {
		if e.Subresource {
			sub++
		} else {
			served++
		}
	}
	assert.Equal(t, 1, served)
	assert.Equal(t, 1, sub)
}

// TestCatalogLookupGVR_Resolved verifies an exact GVR lookup returns the served
// entry and its kind for reverse mapping.
func TestCatalogLookupGVR_Resolved(t *testing.T) {
	catalog := newCommonTestCatalog(t)

	got := catalog.LookupGVR(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"})

	require.Len(t, got.Entries, 1)
	assert.Equal(t, "Deployment", got.Entries[0].GVK.Kind)
	assert.True(t, got.Ready)
}

// TestCatalogLookupGVR_Unserved verifies a ready catalog with no matching GVR
// returns an empty, non-degraded answer ("unserved").
func TestCatalogLookupGVR_Unserved(t *testing.T) {
	catalog := newCommonTestCatalog(t)

	got := catalog.LookupGVR(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"})

	assert.Empty(t, got.Entries)
	assert.True(t, got.Ready)
	assert.False(t, got.Degraded)
}
