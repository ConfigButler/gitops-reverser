// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// configmapsGVR is a second served namespaced type, used to prove per-type scope resolution
// is membership-exact.
var configmapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

// resolveSnapshotGVRForType fails closed when the API surface has not been observed yet — the
// per-type expression of the never-reconcile-a-partial-view invariant.
func TestResolveSnapshotGVRForType_FailsClosedWhenRegistryNotReady(t *testing.T) {
	store := rulestore.NewStore()
	addSecretsWatchRule(store)
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
