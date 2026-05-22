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
	"strings"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

const (
	// groupVersionSplit prevents magic numbers when splitting group/version strings.
	groupVersionSplit = 2
)

// FilterDiscoverableGVRs returns only catalog GVRs that are listable,
// watchable, in scope, and allowed by GitOps Reverser resource policy.
func (m *Manager) FilterDiscoverableGVRs(ctx context.Context, in []GVR) []GVR {
	log := m.Log.WithName("discovery")
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		log.Error(err, "failed to refresh API resource catalog")
		return nil
	}

	var out []GVR
	for _, g := range in {
		entry, ok := m.apiResourceCatalog().Entry(g.schema())
		if !ok || !entry.Allowed || entry.Subresource || !entry.Supports("list", "watch") {
			continue
		}
		if matchesScope(entry.Namespaced, g.Scope) {
			out = append(out, g)
		}
	}

	removed := len(in) - len(out)
	if removed > 0 {
		log.Info("discovery filter removed items", "in", len(in), "out", len(out))
	}

	return out
}

// restConfig acquires the controller runtime REST config.
// Returns nil if no config is available (e.g., in unit tests without a cluster).
func (m *Manager) restConfig() *rest.Config {
	// ctrl.GetConfig reads KUBECONFIG or in-cluster config.
	// In tests/e2e this is set up by the test harness/Kind.
	// In unit tests without a cluster, this returns an error which we handle gracefully.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil
	}
	return cfg
}

type groupVersion struct {
	group   string
	version string
}

func parseGroupVersion(gvString string) groupVersion {
	parts := strings.SplitN(gvString, "/", groupVersionSplit)
	if len(parts) == 1 {
		// Core API group (e.g., "v1")
		return groupVersion{group: "", version: parts[0]}
	}
	return groupVersion{group: parts[0], version: parts[1]}
}

// matchesScope ensures the namespaced flag from discovery aligns with desired scope.
func matchesScope(namespaced bool, scope configv1alpha1.ResourceScope) bool {
	switch scope {
	case configv1alpha1.ResourceScopeNamespaced:
		return namespaced
	case configv1alpha1.ResourceScopeCluster:
		return !namespaced
	default:
		return false
	}
}

func key(group, version, resource string) string {
	return group + "|" + version + "|" + resource
}
