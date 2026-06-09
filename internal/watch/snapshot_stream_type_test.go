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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// configmapsGVR is a second served namespaced type, used to prove the per-type stream
// gathers only its own type.
var configmapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

// The M12 per-type stream gathers only the named type's objects, scoped to the namespaces the
// resident table watches it under — never a sibling type's objects.
func TestStreamSnapshotForType_StreamsOnlyTheType(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	addWatchRule(store, "wr-configmaps", "ns-a", "configmaps")

	m := streamingManager(t, gitTargetFixture(), store, map[schema.GroupVersionResource][]*unstructured.Unstructured{
		secretsGVR:    {uns("Secret", "ns-a", "secret-a"), uns("Secret", "ns-b", "secret-b")},
		configmapsGVR: {uns("ConfigMap", "ns-a", "cm-a")},
	})

	snap, err := m.StreamSnapshotForType(context.Background(), myTargetRef(), secretsGVR)
	require.NoError(t, err)
	assert.Equal(t, []string{"secret-a"}, desiredNames(snap.Desired), "only ns-a secrets, no configmaps, no ns-b leak")
	for _, d := range snap.Desired {
		assert.Equal(t, "secrets", d.Resource.Resource, "the per-type stream gathers only the named type")
	}
}

// A type the GitTarget does not watch yields an empty snapshot and no error, so the caller
// no-ops rather than gathering or sweeping.
func TestStreamSnapshotForType_UnwatchedTypeIsEmpty(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")

	m := streamingManager(t, gitTargetFixture(), store, nil)

	snap, err := m.StreamSnapshotForType(context.Background(), myTargetRef(), configmapsGVR)
	require.NoError(t, err)
	assert.Empty(t, snap.Desired, "an unwatched type produces nothing to reconcile")
}

// resolveSnapshotGVRForType fails closed when the API surface has not been observed yet — the
// per-type expression of the never-reconcile-a-partial-view invariant.
func TestResolveSnapshotGVRForType_FailsClosedWhenRegistryNotReady(t *testing.T) {
	store := rulestore.NewStore()
	addWatchRule(store, "wr-secrets", "ns-a", "secrets")
	empty := apiResourceDiscovery(staticCatalogDiscovery{})
	m := &Manager{
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: NewAPIResourceCatalog(),
		discoveryClient: func() (apiResourceDiscovery, error) { return empty, nil },
	}

	_, _, err := m.resolveSnapshotGVRForType(context.Background(), myTargetRef(), secretsGVR)
	require.Error(t, err, "an unobserved API surface must abort the per-type gather")
	assert.Contains(t, err.Error(), "has not been observed yet")
}

// tableWatchesGVR reports membership of a type in a GitTarget's resident table.
func TestTableWatchesGVR(t *testing.T) {
	table := WatchedTypeTable{Types: []WatchedType{{GVR: secretsGVR}}}
	assert.True(t, tableWatchesGVR(table, secretsGVR), "a watched type is reported present")
	assert.False(t, tableWatchesGVR(table, configmapsGVR), "an unwatched type is reported absent")
}
