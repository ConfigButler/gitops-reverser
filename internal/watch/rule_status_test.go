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

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// The rule-resource status reports only what the rule actually watches — the registry's
// followable set, the same records the informer/snapshot path follows. It never explains
// why an individual selector matched nothing (absent, refused, and not-yet-served are all
// the same to a mirror), so the only False case is a catalog that has not been observed.

func watchRule(rules ...configv1alpha1.ResourceRule) configv1alpha1.WatchRule {
	return configv1alpha1.WatchRule{Spec: configv1alpha1.WatchRuleSpec{Rules: rules}}
}

func TestResolveWatchRuleResources_ReportsFollowableMatchCount(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha1.ResourceRule{Resources: []string{"deployments"}}))

	assert.True(t, resolved)
	assert.Equal(t, "watching 1 resource type(s)", message)
}

func TestResolveWatchRuleResources_UnmatchedResourceStillResolvesWhenCatalogReady(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	// "ghosts" is not served. The app does not flag that as a problem: a ready catalog is
	// resolved, it just watches nothing for this rule.
	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha1.ResourceRule{Resources: []string{"ghosts"}}))

	assert.True(t, resolved)
	assert.Equal(t, "watching 0 resource type(s)", message)
}

func TestResolveWatchRuleResources_NotReadyFailsClosed(t *testing.T) {
	// A catalog that has never observed discovery leaves the registry unready, the one
	// case the status reports as unresolved.
	manager := &Manager{Log: logr.Discard(), resourceCatalog: NewAPIResourceCatalog()}

	resolved, message := manager.ResolveWatchRuleResources(context.Background(),
		watchRule(configv1alpha1.ResourceRule{Resources: []string{"deployments"}}))

	assert.False(t, resolved)
	assert.Equal(t, "API resource catalog is not ready", message)
}

func TestResolveClusterWatchRuleResources_WildcardWatchesManyTypes(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), resourceCatalog: newCommonTestCatalog(t)}

	resolved, message := manager.ResolveClusterWatchRuleResources(context.Background(),
		configv1alpha1.ClusterWatchRule{Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{{
				APIGroups:   []string{"*"},
				APIVersions: []string{"*"},
				Resources:   []string{"*"},
				Scope:       configv1alpha1.ResourceScopeNamespaced,
			}},
		}})

	assert.True(t, resolved)
	assert.NotEqual(t, "watching 0 resource type(s)", message,
		"a wildcard rule watches the followable namespaced types")
}
