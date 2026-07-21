// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// clusterRuleForResource builds a "test-target" ClusterWatchRule for one core/v1 CLUSTER-SCOPED
// resource. Shared by the watched-type-table and materialization tests. A ClusterWatchRule selects
// cluster-scoped types only, so the fixture's resources must be genuinely cluster-scoped for the
// rule to resolve anything at all.
func clusterRuleForResource(name, resource string) configv1alpha3.ClusterWatchRule {
	return configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{
				Name:      "test-target",
				Namespace: "test-ns",
			},
			Rules: []configv1alpha3.ClusterResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{resource},
			}},
		},
	}
}

// watchRuleForTarget builds a namespaced configmaps WatchRule for one target in one namespace.
func watchRuleForTarget(name, gitTargetName, namespace string) configv1alpha3.WatchRule {
	return configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: configv1alpha3.WatchRuleSpec{
			TargetRef: configv1alpha3.LocalTargetReference{Name: gitTargetName},
			Rules: []configv1alpha3.ResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
			}},
		},
	}
}

// TestRefreshWatchedTypeTables_ConcurrentRefreshesConverge stresses the serialized refresh
// (refreshMu) from many goroutines while rules change, asserting it never deadlocks or races
// (run with -race) and converges to the final rule set. The concurrent read is the resident
// table set the splice scope resolution and demand Declare read.
func TestRefreshWatchedTypeTables_ConcurrentRefreshesConverge(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				manager.refreshWatchedTypeTables()
				_ = manager.allWatchedTypeTables()
			}
		}()
	}
	// Concurrently add a second rule mid-flight.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-2", "nodes"),
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
	assert.True(t, kinds["Namespace"] && kinds["Node"], "the settled table reflects both rules")
}
