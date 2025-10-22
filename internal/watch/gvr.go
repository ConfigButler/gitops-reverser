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
	"strings"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// GVR represents a concrete Group/Version/Resource target with a scope.
// This is used to plan dynamic informer creation from active rules.
type GVR struct {
	Group    string
	Version  string
	Resource string
	Scope    configv1alpha1.ResourceScope
}

// ComputeRequestedGVRs aggregates concrete GVRs from the active RuleStore,
// deduplicated across WatchRule and ClusterWatchRule sources.
//
// MVP behavior:
// - Only includes concrete (non-wildcard) combinations:
//   - APIGroups: exactly one value and not "*"
//   - APIVersions: exactly one value and not "*"
//   - Resources: value not "*" and not a subresource pattern (no "/")
//
// - WatchRule entries are treated as Namespaced scope.
// - ClusterWatchRule entries carry scope from their per-rule definition.
//
// Future improvements:
// - Expand wildcard handling using discovery to enumerate actual served versions/resources.
// - Handle subresource patterns (e.g., "pods/log") when needed.
// - Validate GVR existence using API discovery and drop unknown entries.
func (m *Manager) ComputeRequestedGVRs() []GVR {
	if m.RuleStore == nil {
		return nil
	}

	seen := make(map[string]struct{})
	var out []GVR

	// From WatchRule (namespaced-only)
	for _, cr := range m.RuleStore.SnapshotWatchRules() {
		out = append(out, m.gvrFromCompiledRule(cr, configv1alpha1.ResourceScopeNamespaced, seen)...)
	}

	// From ClusterWatchRule (scope per rule)
	for _, ccr := range m.RuleStore.SnapshotClusterWatchRules() {
		for _, rr := range ccr.Rules {
			out = append(out, gvrFromClusterRule(rr, seen)...)
		}
	}

	return out
}

// gvrFromCompiledRule extracts GVR entries from a compiled namespaced rule set.
func (m *Manager) gvrFromCompiledRule(
	cr rulestore.CompiledRule,
	scope configv1alpha1.ResourceScope,
	seen map[string]struct{},
) []GVR {
	var out []GVR
	for _, rr := range cr.ResourceRules {
		// Default empty apiGroups to core API
		groups := rr.APIGroups
		if len(groups) == 0 {
			groups = []string{""} // Core API group
		}
		groupsConcrete, ok := singleConcrete(groups)
		if !ok {
			continue
		}

		// Default empty apiVersions to v1 (most common for core resources)
		versions := rr.APIVersions
		if len(versions) == 0 {
			versions = []string{"v1"} // Default version for core API
		}
		versionsConcrete, ok := singleConcrete(versions)
		if !ok {
			continue
		}

		// Concrete resources only (skip "*" and subresources)
		for _, res := range rr.Resources {
			r := normalizeResource(res)
			if r == "" || r == "*" || strings.Contains(r, "/") {
				continue
			}
			addGVR(groupsConcrete[0], versionsConcrete[0], r, scope, &out, seen)
		}
	}
	return out
}

// gvrFromClusterRule extracts GVR entries from a single cluster rule with scope.
func gvrFromClusterRule(
	rr rulestore.CompiledClusterResourceRule,
	seen map[string]struct{},
) []GVR {
	var out []GVR

	// Default empty apiGroups to core API
	groups := rr.APIGroups
	if len(groups) == 0 {
		groups = []string{""} // Core API group
	}
	groupsConcrete, ok := singleConcrete(groups)
	if !ok {
		return out
	}

	// Default empty apiVersions to v1
	versions := rr.APIVersions
	if len(versions) == 0 {
		versions = []string{"v1"} // Default version for core API
	}
	versionsConcrete, ok := singleConcrete(versions)
	if !ok {
		return out
	}

	for _, res := range rr.Resources {
		r := normalizeResource(res)
		if r == "" || r == "*" || strings.Contains(r, "/") {
			continue
		}
		addGVR(groupsConcrete[0], versionsConcrete[0], r, rr.Scope, &out, seen)
	}
	return out
}

// singleConcrete returns a single-element slice if the input means a concrete set:
// - len==1 AND value != "*"
// - Note: Empty string "" is valid for apiGroup (means core API), so it's considered concrete
// - len==0 is treated as wildcard (not concrete) and returns false.
func singleConcrete(vals []string) ([]string, bool) {
	if len(vals) != 1 {
		return nil, false
	}
	v := strings.TrimSpace(vals[0])
	// Only reject wildcard "*", not empty string (empty string is valid for core API group)
	if v == "*" {
		return nil, false
	}
	return []string{v}, true
}

// normalizeResource lowercases the resource for consistent matching.
func normalizeResource(r string) string {
	return strings.ToLower(strings.TrimSpace(r))
}

// addGVR appends a GVR to output if it was not seen before.
func addGVR(
	group, version, resource string,
	scope configv1alpha1.ResourceScope,
	out *[]GVR,
	seen map[string]struct{},
) {
	key := group + "|" + version + "|" + resource + "|" + string(scope)
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*out = append(*out, GVR{
		Group:    group,
		Version:  version,
		Resource: resource,
		Scope:    scope,
	})
}
