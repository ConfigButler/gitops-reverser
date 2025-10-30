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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

const (
	// groupVersionSplit prevents magic numbers when splitting group/version strings.
	groupVersionSplit = 2
)

// FilterDiscoverableGVRs returns only those GVRs that are currently present in
// API discovery and support list+watch. Scope must match server resource namespaced flag.
//
// Notes:
// - Uses ServerPreferredResources for simplicity; acceptable for MVP.
// - Handles partial discovery failures by ignoring broken groups (common in clusters).
func (m *Manager) FilterDiscoverableGVRs(_ context.Context, in []GVR) []GVR {
	log := m.Log.WithName("discovery")

	cfg := m.restConfig()
	if cfg == nil {
		log.Info("skipping discovery - no rest config available")
		return nil
	}

	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		log.Error(err, "failed to create discovery client")
		return nil
	}

	preferred, err := disco.ServerPreferredResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		log.Error(err, "failed to fetch server preferred resources")
		return nil
	}

	index := buildDiscoveryIndex(preferred)

	// Build default exclusion set locally (MVP) to avoid global state.
	// Keys use the form "group|resource" where group is empty string for core.
	exKey := func(group, resource string) string { return group + "|" + resource }
	defaultExclusions := map[string]struct{}{
		exKey("", "pods"):                                                    {},
		exKey("", "events"):                                                  {},
		exKey("events.k8s.io", "events"):                                     {},
		exKey("", "endpoints"):                                               {},
		exKey("discovery.k8s.io", "endpointslices"):                          {},
		exKey("coordination.k8s.io", "leases"):                               {},
		exKey("apps", "controllerrevisions"):                                 {},
		exKey("flowcontrol.apiserver.k8s.io", "flowschemas"):                 {},
		exKey("flowcontrol.apiserver.k8s.io", "prioritylevelconfigurations"): {},
		exKey("batch", "jobs"):                                               {},
		exKey("batch", "cronjobs"):                                           {},
	}

	var out []GVR
	for _, g := range in {
		// Apply built-in default exclusions for runtime/noisy resources (MVP).
		if _, skip := defaultExclusions[exKey(g.Group, g.Resource)]; skip {
			continue
		}
		if ent, ok := index[key(g.Group, g.Version, g.Resource)]; ok && matchesScope(ent.namespaced, g.Scope) {
			out = append(out, g)
		}
	}

	log.Info("discovery filter applied", "requested", len(in), "discoverable", len(out))
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

func hasVerbs(r metav1.APIResource, verbs ...string) bool {
	for _, v := range verbs {
		if !contains(r.Verbs, v) {
			return false
		}
	}
	return true
}

// discoveryEntry stores per-resource flags extracted from discovery.
type discoveryEntry struct {
	namespaced bool
}

// buildDiscoveryIndex converts discovery results into a fast lookup map keyed by group|version|resource.
func buildDiscoveryIndex(preferred []*metav1.APIResourceList) map[string]discoveryEntry {
	idx := make(map[string]discoveryEntry)
	for _, rl := range preferred {
		if rl == nil {
			continue
		}
		gv := parseGroupVersion(rl.GroupVersion)
		for _, r := range rl.APIResources {
			// Skip subresources (contain '/')
			if strings.Contains(r.Name, "/") {
				continue
			}
			// Require list+watch
			if !hasVerbs(r, "list", "watch") {
				continue
			}
			idx[key(gv.group, gv.version, r.Name)] = discoveryEntry{namespaced: r.Namespaced}
		}
	}
	return idx
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

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func key(group, version, resource string) string {
	return group + "|" + version + "|" + resource
}
