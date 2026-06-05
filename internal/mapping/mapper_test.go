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

package mapping

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// gvr builds a v1 GroupVersionResource; every fixture resource here is served at
// v1, so the version is fixed.
func gvr(group, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}
}

// servedSnapshot is a small ready cluster fixture: apps/v1 Deployment and core v1
// ConfigMap are served and allowed; core v1 Secret is served but disallowed by
// policy.
func servedSnapshot() Snapshot {
	return Snapshot{
		Generation: 7,
		Entries: []Entry{
			{
				GVK:        gvk("apps", "v1", "Deployment"),
				GVR:        gvr("apps", "deployments"),
				Namespaced: true,
				Verbs:      []string{"watch", "get", "list"},
				Preferred:  true,
				Allowed:    true,
			},
			{
				GVK:        gvk("", "v1", "ConfigMap"),
				GVR:        gvr("", "configmaps"),
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        gvk("", "v1", "Secret"),
				GVR:        gvr("", "secrets"),
				Namespaced: true,
				Allowed:    false,
			},
		},
	}
}

func TestStaticSnapshotMapper_GVRForGVK_Resolved(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())

	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, MappingResolved, got.Status)
	assert.Equal(t, gvr("apps", "deployments"), got.GVR)
	assert.True(t, got.Namespaced)
	assert.True(t, got.Preferred)
	assert.True(t, got.Allowed)
	// Verbs are returned sorted for deterministic output.
	assert.Equal(t, []string{"get", "list", "watch"}, got.Verbs)
}

func TestStaticSnapshotMapper_GVRForGVK_Disallowed(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())

	got, err := mapper.GVRForGVK(context.Background(), gvk("", "v1", "Secret"))
	require.NoError(t, err)

	assert.Equal(t, MappingDisallowed, got.Status)
	// A disallowed result is policy, not "not served": GVR stays unset.
	assert.Empty(t, got.GVR.Resource)
}

func TestStaticSnapshotMapper_GVRForGVK_Unserved(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())

	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "StatefulSet"))
	require.NoError(t, err)

	assert.Equal(t, MappingUnserved, got.Status)
	assert.Equal(t, gvk("apps", "v1", "StatefulSet"), got.GVK)
}

func TestStaticSnapshotMapper_GVRForGVK_ExactVersionOnly(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())

	// The human relationship extensions/v1beta1 Deployment -> apps/v1 is never
	// guessed: an unserved apiVersion stays unserved.
	got, err := mapper.GVRForGVK(context.Background(), gvk("extensions", "v1beta1", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, MappingUnserved, got.Status)
}

func TestStaticSnapshotMapper_GVRForGVK_Ambiguous(t *testing.T) {
	snap := servedSnapshot()
	// Two served resources share the same kind; with nothing to narrow them this
	// is ambiguous, never a silent guess.
	snap.Entries = append(snap.Entries,
		Entry{GVK: gvk("a.example.com", "v1", "Widget"), GVR: gvr("a.example.com", "widgets"), Allowed: true},
		Entry{GVK: gvk("a.example.com", "v1", "Widget"), GVR: gvr("a.example.com", "widgetz"), Allowed: true},
	)
	mapper := NewStaticSnapshotMapper(snap)

	got, err := mapper.GVRForGVK(context.Background(), gvk("a.example.com", "v1", "Widget"))
	require.NoError(t, err)

	assert.Equal(t, MappingAmbiguous, got.Status)
}

func TestStaticSnapshotMapper_GVRForGVK_SubresourceOnly(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{{
			GVK:         gvk("apps", "v1", "Deployment"),
			GVR:         gvr("apps", "deployments/status"),
			Subresource: true,
			Allowed:     true,
		}},
	}
	mapper := NewStaticSnapshotMapper(snap)

	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, MappingSubresource, got.Status)
}

func TestStaticSnapshotMapper_GVRForGVK_RealWinsOverSubresource(t *testing.T) {
	snap := Snapshot{
		Entries: []Entry{
			{GVK: gvk("apps", "v1", "Deployment"), GVR: gvr("apps", "deployments"), Allowed: true},
			{
				GVK:         gvk("apps", "v1", "Deployment"),
				GVR:         gvr("apps", "deployments/status"),
				Subresource: true,
				Allowed:     true,
			},
		},
	}
	mapper := NewStaticSnapshotMapper(snap)

	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "Deployment"))
	require.NoError(t, err)

	assert.Equal(t, MappingResolved, got.Status)
	assert.Equal(t, "deployments", got.GVR.Resource)
}

func TestStaticSnapshotMapper_GVRForGVK_CatalogUnavailable(t *testing.T) {
	snap := servedSnapshot()
	snap.NotReady = true
	mapper := NewStaticSnapshotMapper(snap)

	got, err := mapper.GVRForGVK(context.Background(), gvk("example.com", "v1", "Thing"))
	require.NoError(t, err)

	assert.Equal(t, MappingCatalogUnavailable, got.Status)
}

func TestStaticSnapshotMapper_GVRForGVK_DiscoveryDegraded(t *testing.T) {
	snap := servedSnapshot()
	snap.DegradedGroupVersions = []schema.GroupVersion{{Group: "apps", Version: "v1"}}
	mapper := NewStaticSnapshotMapper(snap)

	// A kind absent from a degraded group/version is degraded, not unserved.
	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "StatefulSet"))
	require.NoError(t, err)
	assert.Equal(t, MappingDiscoveryDegraded, got.Status)

	// A healthy group/version still answers unserved.
	got, err = mapper.GVRForGVK(context.Background(), gvk("", "v1", "Pod"))
	require.NoError(t, err)
	assert.Equal(t, MappingUnserved, got.Status)

	assert.True(t, mapper.Ready().Degraded)
}

func TestStaticSnapshotMapper_ContextCancelled(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mapper.GVRForGVK(ctx, gvk("apps", "v1", "Deployment"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestStaticSnapshotMapper_SourceAndGeneration(t *testing.T) {
	mapper := NewStaticSnapshotMapper(servedSnapshot())

	assert.Equal(t, MapperSourceStaticSnapshot, mapper.Source())
	assert.Equal(t, uint64(7), mapper.Generation())
	ready := mapper.Ready()
	assert.True(t, ready.Ready)
	assert.False(t, ready.Degraded)
	assert.Equal(t, uint64(7), ready.Generation)
}

func TestStructureOnlyMapper_AlwaysStructureOnly(t *testing.T) {
	mapper := NewStructureOnlyMapper()

	assert.Equal(t, MapperSourceStructureOnly, mapper.Source())
	assert.Equal(t, uint64(0), mapper.Generation())
	assert.False(t, mapper.Ready().Ready)

	got, err := mapper.GVRForGVK(context.Background(), gvk("apps", "v1", "Deployment"))
	require.NoError(t, err)
	assert.Equal(t, MappingStructureOnly, got.Status)
	assert.Equal(t, gvk("apps", "v1", "Deployment"), got.GVK)
	assert.Empty(t, got.GVR.Resource)
}

func TestStructureOnlyMapper_ContextCancelled(t *testing.T) {
	mapper := NewStructureOnlyMapper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Cancellation is honored even though the mapper does no I/O, so every
	// ResourceMapper reacts to a cancelled context identically.
	_, err := mapper.GVRForGVK(ctx, gvk("apps", "v1", "Deployment"))
	require.ErrorIs(t, err, context.Canceled)
}

// TestStructureOnlyMapper_SatisfiesInterface is a compile-time guard kept as a
// runtime assertion for clarity.
func TestStructureOnlyMapper_SatisfiesInterface(_ *testing.T) {
	var _ ResourceMapper = NewStructureOnlyMapper()
	var _ ResourceMapper = NewStaticSnapshotMapper(Snapshot{})
}
