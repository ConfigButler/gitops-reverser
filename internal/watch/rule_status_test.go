// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// The rule-resource status reports only what the rule actually watches — the registry's
// followable set, the same records the informer/snapshot path follows. It never explains
// why an individual selector matched nothing (absent, refused, and not-yet-served are all
// the same to a mirror), so the only False case is a catalog that has not been observed.

func watchRule(rules ...configv1alpha3.ResourceRule) configv1alpha3.WatchRule {
	return configv1alpha3.WatchRule{Spec: configv1alpha3.WatchRuleSpec{Rules: rules}}
}

func TestResolveWatchRuleResources_ReportsFollowableMatchCount(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha3.ResourceRule{Resources: []string{"deployments"}}))

	assert.True(t, resolved)
	assert.Equal(t, "watching 1 resource type(s)", message)
}

func TestResolveWatchRuleResources_UnmatchedResourceStillResolvesWhenCatalogReady(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	// "ghosts" is not served. The app does not flag that as a problem: a ready catalog is
	// resolved, it just watches nothing for this rule.
	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha3.ResourceRule{Resources: []string{"ghosts"}}))

	assert.True(t, resolved)
	assert.Equal(t, "watching 0 resource type(s)", message)
}

func TestResolveWatchRuleResources_NotReadyFailsClosed(t *testing.T) {
	// A catalog that has never observed discovery leaves the registry unready, the one
	// case the status reports as unresolved.
	manager := &Manager{Log: logr.Discard(), resourceCatalog: NewAPIResourceCatalog()}

	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha3.ResourceRule{Resources: []string{"deployments"}}))

	assert.False(t, resolved)
	assert.Equal(t, "API resource catalog is not ready", message)
}

func TestResolveClusterWatchRuleResources_WildcardWatchesManyTypes(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	resolved, message := manager.ResolveClusterWatchRuleResources(context.Background(),
		configv1alpha3.ClusterWatchRule{Spec: configv1alpha3.ClusterWatchRuleSpec{
			Rules: []configv1alpha3.ClusterResourceRule{{
				APIGroups:   []string{"*"},
				APIVersions: []string{"*"},
				Resources:   []string{"*"},
				Scope:       configv1alpha3.ResourceScopeNamespaced,
			}},
		}})

	assert.True(t, resolved)
	assert.NotEqual(t, "watching 0 resource type(s)", message,
		"a wildcard rule watches the followable namespaced types")
}
