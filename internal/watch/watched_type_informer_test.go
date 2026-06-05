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
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopCancel returns a CancelFunc that records whether it was called.
func noopCancel(called *bool) context.CancelFunc {
	return func() { *called = true }
}

func activeInformers(cm GVR, namespaces ...string) map[GVR]map[string]context.CancelFunc {
	ns := map[string]context.CancelFunc{}
	for _, n := range namespaces {
		ns[n] = func() {}
	}
	return map[GVR]map[string]context.CancelFunc{cm: ns}
}

func TestInformersObsolete_NamespaceShrinkTearsDownDroppedNamespace(t *testing.T) {
	cm := nsGVR("", "configmaps")
	obsolete := informersObsolete(activeInformers(cm, "ns-a", "ns-b"), map[GVR]map[string]struct{}{cm: {"ns-a": {}}})

	require.Len(t, obsolete, 1)
	assert.Equal(t, gvrNamespace{gvr: cm, ns: "ns-b"}, obsolete[0])
}

func TestInformersObsolete_NamespaceToClusterWideTearsDownNamespaced(t *testing.T) {
	cm := nsGVR("", "configmaps")
	// New scope is cluster-wide ("") for the same GVR.
	obsolete := informersObsolete(activeInformers(cm, "ns-a"), map[GVR]map[string]struct{}{cm: {"": {}}})

	require.Len(t, obsolete, 1)
	assert.Equal(t, gvrNamespace{gvr: cm, ns: "ns-a"}, obsolete[0])
}

func TestInformersObsolete_WholeGVRGoneTearsDownEveryNamespace(t *testing.T) {
	cm := nsGVR("", "configmaps")
	obsolete := informersObsolete(activeInformers(cm, "ns-a", ""), map[GVR]map[string]struct{}{})

	assert.Len(t, obsolete, 2)
}

func TestInformersToStart_StartsOnlyMissingNamespaces(t *testing.T) {
	cm := nsGVR("", "configmaps")
	toStart := informersToStart(activeInformers(cm, "ns-a"), map[GVR]map[string]struct{}{cm: {"ns-a": {}, "ns-b": {}}})

	assert.Equal(
		t,
		[]gvrNamespace{{gvr: cm, ns: "ns-b"}},
		toStart,
		"only the added namespace ns-b is started; ns-a stays",
	)
}

func TestStopInformerNamespace_IsIdempotentAndDropsEmptyGVR(t *testing.T) {
	cm := nsGVR("", "configmaps")
	cancelledA := false
	m := &Manager{
		Log: logr.Discard(),
		activeInformers: map[GVR]map[string]context.CancelFunc{
			cm: {"ns-a": noopCancel(&cancelledA), "ns-b": func() {}},
		},
	}

	m.stopInformerNamespace(cm, "ns-a")
	assert.True(t, cancelledA, "the stopped namespace's informer must be cancelled")
	assert.NotContains(t, m.activeInformers[cm], "ns-a")
	assert.Contains(t, m.activeInformers[cm], "ns-b", "the surviving namespace stays")

	// Idempotent: stopping an already-stopped namespace is a no-op.
	m.stopInformerNamespace(cm, "ns-a")

	// Stopping the last namespace drops the GVR entry entirely.
	m.stopInformerNamespace(cm, "ns-b")
	_, present := m.activeInformers[cm]
	assert.False(t, present, "a GVR with no remaining namespaces is removed")

	// Stopping a namespace of a GVR that is no longer active is a safe no-op.
	assert.NotPanics(t, func() { m.stopInformerNamespace(cm, "ns-a") })
}

func TestDesiredInformerScope_ClusterWideWinsOverNamedNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	// configmaps watched in ns-a (WatchRule) AND cluster-wide (ClusterWatchRule).
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-a", "test-target", "ns-a"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cw", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	scope := manager.desiredInformerScope()
	assert.Equal(t, map[string]struct{}{"": {}}, scope[nsGVR("", "configmaps")],
		"a cluster-wide selection collapses the named namespace to a single cluster-wide stream")
}

func TestCompareInformerScope_NamespaceToClusterWideStartsAndRetires(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	cm := nsGVR("", "configmaps")
	// Pretend the old namespace-scoped informer is running.
	manager.activeInformers = map[GVR]map[string]context.CancelFunc{cm: {"ns-a": func() {}}}

	toStart, obsolete := manager.compareInformerScope(manager.desiredInformerScope())

	assert.Contains(t, toStart, gvrNamespace{gvr: cm, ns: ""}, "the cluster-wide scope needs a new informer")
	require.Len(t, obsolete, 1)
	assert.Equal(t, gvrNamespace{gvr: cm, ns: "ns-a"}, obsolete[0],
		"the obsolete namespace-scoped informer must be retired, not left running")
}

func TestCompareInformerScope_InitializesActiveInformers(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	cm := nsGVR("", "configmaps")
	// activeInformers is nil — compareInformerScope must lazily initialize it.
	toStart, obsolete := manager.compareInformerScope(manager.desiredInformerScope())

	assert.Contains(t, toStart, gvrNamespace{gvr: cm, ns: ""})
	assert.Empty(t, obsolete)
	assert.NotNil(t, manager.activeInformers)
}

func TestChangedInformerGVRs_DeduplicatesAcrossStartAndObsolete(t *testing.T) {
	cm := nsGVR("", "configmaps")
	secrets := nsGVR("", "secrets")

	got := changedInformerGVRs(
		[]gvrNamespace{{gvr: cm, ns: "ns-b"}},
		[]gvrNamespace{{gvr: cm, ns: "ns-a"}, {gvr: secrets, ns: ""}},
	)

	assert.ElementsMatch(t, []GVR{cm, secrets}, got)
}

// TestRefreshWatchedTypeTables_ConcurrentRefreshesConverge stresses the serialized
// refresh (refreshMu) from many goroutines while rules change, asserting it never
// deadlocks or races (run with -race) and converges to the final rule set.
func TestRefreshWatchedTypeTables_ConcurrentRefreshesConverge(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				manager.refreshWatchedTypeTables()
				_ = manager.desiredInformerScope()
			}
		}()
	}
	// Concurrently add a second rule mid-flight.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-2", "test-target", "secrets"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	wg.Wait()

	// A final settled refresh must reflect both rules.
	manager.refreshWatchedTypeTables()
	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.True(t, ok)
	kinds := map[string]bool{}
	for _, wt := range table.Types {
		kinds[wt.GVK.Kind] = true
	}
	assert.True(t, kinds["ConfigMap"] && kinds["Secret"], "the settled table reflects both rules")
}
