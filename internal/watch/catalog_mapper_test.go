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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/ConfigButler/gitops-reverser/internal/mapping"
)

// mapperGVK builds v1 identities; every resource these tests exercise is served at v1,
// so the version is fixed.
func mapperGVK(group, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind}
}

// TestCatalogMapper_Resolved verifies the live-catalog mapper resolves a served
// kind to its GVR end to end through the real catalog.
func TestCatalogMapper_Resolved(t *testing.T) {
	mapper := NewCatalogMapper(newCommonTestCatalog(t))

	got, err := mapper.GVRForGVK(context.Background(), mapperGVK("apps", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, mapping.MappingResolved, got.Status)
	assert.Equal(t, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, got.GVR)
	assert.True(t, got.Namespaced)
	assert.Equal(t, mapping.MapperSourceLiveCatalog, mapper.Source())
	assert.True(t, mapper.Ready().Ready)
}

// TestCatalogMapper_Unserved verifies a ready catalog reports a missing kind as
// unserved, not as an error.
func TestCatalogMapper_Unserved(t *testing.T) {
	mapper := NewCatalogMapper(newCommonTestCatalog(t))

	got, err := mapper.GVRForGVK(context.Background(), mapperGVK("apps", "StatefulSet"))
	require.NoError(t, err)

	assert.Equal(t, mapping.MappingUnserved, got.Status)
}

// TestCatalogMapper_CatalogUnavailable verifies an unready catalog reports
// CatalogUnavailable so callers fail closed rather than treating it as absence.
func TestCatalogMapper_CatalogUnavailable(t *testing.T) {
	mapper := NewCatalogMapper(NewAPIResourceCatalog())

	got, err := mapper.GVRForGVK(context.Background(), mapperGVK("apps", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, mapping.MappingCatalogUnavailable, got.Status)
	assert.False(t, mapper.Ready().Ready)
}

// TestCatalogMapper_Disallowed verifies a served-but-excluded resource surfaces as
// policy (Disallowed), distinct from unserved.
func TestCatalogMapper_Disallowed(t *testing.T) {
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("", "v1")},
		resources: []*metav1.APIResourceList{{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
			},
		}},
	})
	require.NoError(t, err)
	mapper := NewCatalogMapper(catalog)

	byGVK, err := mapper.GVRForGVK(context.Background(), mapperGVK("", "Pod"))
	require.NoError(t, err)
	assert.Equal(t, mapping.MappingDisallowed, byGVK.Status)
}

// TestCatalogMapper_DiscoveryDegraded verifies a lookup against a degraded
// group/version reports DiscoveryDegraded and surfaces degraded readiness, so an
// absence under partial discovery is never trusted as a delete signal.
func TestCatalogMapper_DiscoveryDegraded(t *testing.T) {
	catalog := newCommonTestCatalog(t)
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}
	_, err := catalog.Refresh(staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("apps", "v1")},
		err: &discovery.ErrGroupDiscoveryFailed{
			Groups: map[schema.GroupVersion]error{appsGV: errors.New("aggregated discovery failed")},
		},
	})
	require.NoError(t, err)
	mapper := NewCatalogMapper(catalog)

	got, err := mapper.GVRForGVK(context.Background(), mapperGVK("apps", "StatefulSet"))
	require.NoError(t, err)
	assert.Equal(t, mapping.MappingDiscoveryDegraded, got.Status)
	assert.True(t, mapper.Ready().Degraded)
}

// TestCatalogMapper_ContextCancelled verifies a cancelled context is an error,
// not a silent miss.
func TestCatalogMapper_ContextCancelled(t *testing.T) {
	mapper := NewCatalogMapper(newCommonTestCatalog(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mapper.GVRForGVK(ctx, mapperGVK("apps", "Deployment"))
	require.ErrorIs(t, err, context.Canceled)
}

// TestCatalogMapper_NilCatalog verifies the mapper degrades safely with no
// catalog wired in rather than panicking.
func TestCatalogMapper_NilCatalog(t *testing.T) {
	mapper := NewCatalogMapper(nil)

	got, err := mapper.GVRForGVK(context.Background(), mapperGVK("apps", "Deployment"))
	require.NoError(t, err)
	assert.Equal(t, mapping.MappingCatalogUnavailable, got.Status)
	assert.False(t, mapper.Ready().Ready)
	assert.Equal(t, uint64(0), mapper.Generation())
}
