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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// The persistent-absence tests exercise the watched-type store's "trusted, persistent
// absence" policy: a still-selected type the catalog momentarily stops serving is held
// (retained, snapshot blocked) under a grace timer rather than swept, which is the fix
// for the resources: 7 -> 0 -> 7 discovery wobble.

// fakeClock is an injectable, deterministic clock for the removal grace timer.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// shopGV is the group/version of the test CRD that the absence tests make appear and
// disappear from discovery.
var shopGV = schema.GroupVersion{Group: "shop.example.com", Version: "v1alpha1"}

// crGVR is the watched GVR of the test CRD (shop.example.com/v1alpha1 customresources),
// gathered cluster-wide because the rule selects it with a ClusterWatchRule.
func crGVR() GVR {
	return GVR{
		Group:    "shop.example.com",
		Version:  "v1alpha1",
		Resource: "customresources",
		Scope:    configv1alpha1.ResourceScopeNamespaced,
	}
}

// clusterRuleForGroupResource builds a ClusterWatchRule selecting one group/version/resource
// cluster-wide (Namespaced scope), so the resolved type is gathered as a single cluster-wide
// stream.
func clusterRuleForGroupResource(name, target, group, version, resource string) configv1alpha1.ClusterWatchRule {
	return configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			TargetRef: configv1alpha1.NamespacedTargetReference{Name: target, Namespace: "test-ns"},
			Rules: []configv1alpha1.ClusterResourceRule{{
				APIGroups:   []string{group},
				APIVersions: []string{version},
				Resources:   []string{resource},
				Scope:       configv1alpha1.ResourceScopeNamespaced,
			}},
		},
	}
}

// discoveryWithoutShop is the common discovery surface with the shop.example.com CRD group
// cleanly absent — a healthy catalog that simply no longer serves the type (NotServed),
// the trigger for a grace-held removal.
func discoveryWithoutShop() staticCatalogDiscovery {
	base := newCommonTestDiscovery()
	out := staticCatalogDiscovery{}
	for _, g := range base.groups {
		if g.Name == shopGV.Group {
			continue
		}
		out.groups = append(out.groups, g)
	}
	for _, rl := range base.resources {
		if rl.GroupVersion == shopGV.String() {
			continue
		}
		out.resources = append(out.resources, rl)
	}
	return out
}

// makeAbsenceManager builds a Manager whose catalog the test drives directly (via
// catalog.Refresh) and whose removal-grace clock is the injectable fakeClock. It starts
// with the common discovery, so the shop CRD is initially served.
func makeAbsenceManager(t *testing.T) (*Manager, *rulestore.RuleStore, *APIResourceCatalog, *fakeClock) {
	t.Helper()
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)

	store := rulestore.NewStore()
	clock := newFakeClock()
	m := &Manager{Log: logr.Discard(), RuleStore: store, resourceCatalog: catalog}
	m.ensureWatchedTypeStore()
	m.watchedTypes.now = clock.now
	m.watchedTypes.removalGrace = 60 * time.Second
	return m, store, catalog, clock
}

func addShopRule(store *rulestore.RuleStore) {
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForGroupResource("cr-rule", "tgt", shopGV.Group, shopGV.Version, "customresources"),
		"tgt", "test-ns", "test-provider", "test-ns", "main", "path",
	)
}

func mustAbsenceTable(t *testing.T, m *Manager) WatchedTypeTable {
	t.Helper()
	table, ok := m.residentWatchedTypeTable(gitDestRef("tgt"))
	require.True(t, ok, "GitTarget table must be resident")
	return table
}

func TestPersistentAbsence_WithinGraceRetainsTypeAndKeepsInformer(t *testing.T) {
	m, store, catalog, _ := makeAbsenceManager(t)
	addShopRule(store)
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).Types, 1, "type initially served and watched")

	// The CRD disappears from a healthy catalog (clean discovery, no degradation).
	_, err := catalog.Refresh(discoveryWithoutShop())
	require.NoError(t, err)
	m.refreshWatchedTypeTables()

	held := mustAbsenceTable(t, m)
	require.Len(t, held.Types, 1, "still-selected type is retained during the grace window")
	require.Len(t, held.PendingRemovals, 1)
	assert.Equal(t, "CustomResource", held.PendingRemovals[0].Type.GVK.Kind)

	// Informers stay desired because the retained type is still in Types.
	assert.Equal(t, map[string]struct{}{"": {}}, m.desiredInformerScope()[crGVR()],
		"the retained type's cluster-wide informer must remain desired")
}

func TestPersistentAbsence_ReappearsBeforeGraceClearsPending(t *testing.T) {
	m, store, catalog, clock := makeAbsenceManager(t)
	addShopRule(store)
	m.refreshWatchedTypeTables()

	_, err := catalog.Refresh(discoveryWithoutShop())
	require.NoError(t, err)
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).PendingRemovals, 1, "absence is held pending first")

	// The CRD reappears within the grace window.
	clock.advance(30 * time.Second)
	_, err = catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)
	m.refreshWatchedTypeTables()

	table := mustAbsenceTable(t, m)
	require.Len(t, table.Types, 1)
	assert.Empty(t, table.PendingRemovals, "reappearance clears the pending removal")
	assert.False(t, m.hasPendingRemovals())
}

func TestPersistentAbsence_PastGraceRemovesTypeAndInformer(t *testing.T) {
	m, store, catalog, clock := makeAbsenceManager(t)
	addShopRule(store)
	m.refreshWatchedTypeTables()

	_, err := catalog.Refresh(discoveryWithoutShop())
	require.NoError(t, err)
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).Types, 1, "type retained while pending")

	// The absence persists past the grace window with no other trigger.
	clock.advance(61 * time.Second)
	m.refreshWatchedTypeTables()

	table := mustAbsenceTable(t, m)
	assert.Empty(t, table.Types, "absence persisted past grace -> the type is removed")
	assert.Empty(t, table.PendingRemovals)
	assert.False(t, m.hasPendingRemovals())

	_, present := m.desiredInformerScope()[crGVR()]
	assert.False(t, present, "the removed type is no longer a desired informer")

	// An active informer for the now-removed type is reported obsolete for teardown.
	m.activeInformers = map[GVR]map[string]context.CancelFunc{crGVR(): {"": func() {}}}
	_, obsolete := m.compareInformerScope(m.desiredInformerScope())
	assert.Contains(t, obsolete, gvrNamespace{gvr: crGVR(), ns: ""})
}

func TestPersistentAbsence_RuleRemovedRemovesImmediately(t *testing.T) {
	m, store, _, _ := makeAbsenceManager(t)
	addShopRule(store)
	// A second rule keeps the GitTarget alive after the shop rule is removed, so we
	// observe an immediate per-type removal rather than the whole target vanishing.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("cm-rule", "tgt", "configmaps"),
		"tgt", "test-ns", "test-provider", "test-ns", "main", "path",
	)
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).Types, 2, "both types initially watched")

	// The user edits the rules so the shop CRD is no longer selected. The catalog still
	// serves it — this is intent, not a wobble — so it must be removed immediately.
	store.DeleteClusterWatchRule(k8stypes.NamespacedName{Name: "cr-rule"})
	m.refreshWatchedTypeTables()

	table := mustAbsenceTable(t, m)
	require.Len(t, table.Types, 1)
	assert.Equal(t, "ConfigMap", table.Types[0].GVK.Kind)
	assert.Empty(t, table.PendingRemovals, "explicit rule removal never holds")
	assert.False(t, m.hasPendingRemovals())
}

func TestPersistentAbsence_DegradedDiscoveryRetainsIndefinitely(t *testing.T) {
	m, store, catalog, clock := makeAbsenceManager(t)
	addShopRule(store)
	m.refreshWatchedTypeTables()

	// The group is cleanly removed (entries gone) and then discovery degrades for it, so
	// the catalog can no longer observe the type at all: an unobservable surface, never a
	// trusted absence.
	_, err := catalog.Refresh(discoveryWithoutShop())
	require.NoError(t, err)
	_, err = catalog.Refresh(degradedTestDiscovery(shopGV))
	require.NoError(t, err)
	m.refreshWatchedTypeTables()

	table := mustAbsenceTable(t, m)
	require.Len(t, table.Types, 1, "a degraded catalog retains the previous type")
	assert.Empty(t, table.PendingRemovals, "degraded retention carries no grace timer")
	assert.NotEmpty(t, table.BlockingMisses(), "the snapshot is blocked by the degraded miss")
	assert.False(t, m.hasPendingRemovals())

	// No grace timer means time alone never removes it.
	clock.advance(10 * time.Minute)
	m.refreshWatchedTypeTables()
	assert.Len(t, mustAbsenceTable(t, m).Types, 1, "retained indefinitely while discovery is degraded")
}

func TestPersistentAbsence_UnavailableCatalogOnStartupRetainsNothing(t *testing.T) {
	catalog := NewAPIResourceCatalog() // never refreshed -> not ready
	store := rulestore.NewStore()
	m := &Manager{Log: logr.Discard(), RuleStore: store, resourceCatalog: catalog}
	m.ensureWatchedTypeStore()
	addShopRule(store)

	m.refreshWatchedTypeTables()

	table := mustAbsenceTable(t, m)
	assert.Empty(t, table.Types, "no previous type means nothing is fabricated to retain")
	assert.Empty(t, table.PendingRemovals)
	assert.NotEmpty(t, table.BlockingMisses(), "an unavailable catalog fails the snapshot closed")
}

func TestResolveSnapshotGVRs_FailsClosedWhilePendingRemoval(t *testing.T) {
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(newCommonTestDiscovery())
	require.NoError(t, err)

	store := rulestore.NewStore()
	clock := newFakeClock()
	disco := apiResourceDiscovery(newCommonTestDiscovery())
	m := &Manager{
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: catalog,
		discoveryClient: func() (apiResourceDiscovery, error) { return disco, nil },
	}
	m.ensureWatchedTypeStore()
	m.watchedTypes.now = clock.now
	m.watchedTypes.removalGrace = 60 * time.Second
	addShopRule(store)

	ctx := context.Background()
	require.NoError(t, m.RefreshAPIResourceCatalog(ctx))
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).Types, 1)

	// The CRD disappears from discovery; the manager's own catalog refresh now sees it gone.
	disco = discoveryWithoutShop()
	require.NoError(t, m.RefreshAPIResourceCatalog(ctx))
	m.refreshWatchedTypeTables()
	require.Len(t, mustAbsenceTable(t, m).PendingRemovals, 1)

	_, err = m.resolveSnapshotGVRs(ctx, gitDestRef("tgt"))
	require.Error(t, err, "a pending removal must abort the gather rather than sweep a reduced view")
	assert.Contains(t, err.Error(), "pending removal")
}
