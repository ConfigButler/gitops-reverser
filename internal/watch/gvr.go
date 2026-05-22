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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

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

func (g GVR) schema() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g.Group, Version: g.Version, Resource: g.Resource}
}

// ComputeRequestedGVRs aggregates resolved GVRs from the active RuleStore.
func (m *Manager) ComputeRequestedGVRs() []GVR {
	out, _ := m.computeRequestedGVRs()
	return out
}

func (m *Manager) computeRequestedGVRs() ([]GVR, []ResolveMiss) {
	if m.RuleStore == nil {
		return nil, nil
	}

	var out []GVR
	var misses []ResolveMiss
	resolver := m.ruleGVRResolver()

	// From WatchRule (namespaced-only)
	for _, cr := range m.RuleStore.SnapshotWatchRules() {
		gvrs, ruleMisses := gvrFromCompiledRule(resolver, cr, configv1alpha1.ResourceScopeNamespaced)
		out = append(out, gvrs...)
		misses = append(misses, ruleMisses...)
	}

	// From ClusterWatchRule (scope per rule)
	for _, ccr := range m.RuleStore.SnapshotClusterWatchRules() {
		for _, rr := range ccr.Rules {
			gvrs, ruleMisses := gvrFromClusterRule(resolver, rr)
			out = append(out, gvrs...)
			misses = append(misses, ruleMisses...)
		}
	}

	return dedupeGVRs(out), misses
}

// gvrFromCompiledRule extracts GVR entries from a compiled namespaced rule set.
func gvrFromCompiledRule(
	resolver *RuleGVRResolver,
	cr rulestore.CompiledRule,
	scope configv1alpha1.ResourceScope,
) ([]GVR, []ResolveMiss) {
	var out []GVR
	var misses []ResolveMiss
	for _, rr := range cr.ResourceRules {
		gvrs, ruleMisses := resolver.Resolve(rr.APIGroups, rr.APIVersions, rr.Resources, scope)
		out = append(out, gvrs...)
		misses = append(misses, ruleMisses...)
	}
	return out, misses
}

// gvrFromClusterRule extracts GVR entries from a single cluster rule with scope.
func gvrFromClusterRule(
	resolver *RuleGVRResolver,
	rr rulestore.CompiledClusterResourceRule,
) ([]GVR, []ResolveMiss) {
	return resolver.Resolve(rr.APIGroups, rr.APIVersions, rr.Resources, rr.Scope)
}

// normalizeResource lowercases the resource for consistent matching.
func normalizeResource(r string) string {
	return strings.ToLower(strings.TrimSpace(r))
}

// FormatResolveMisses produces an actionable summary for status and logs.
func FormatResolveMisses(misses []ResolveMiss) string {
	if len(misses) == 0 {
		return "all rule resources resolved"
	}
	parts := make([]string, 0, len(misses))
	for _, miss := range misses {
		detail := miss.Detail
		if detail == "" {
			detail = string(miss.Reason)
		}
		parts = append(parts, fmt.Sprintf("%q: %s", miss.Resource, detail))
	}
	return strings.Join(uniqueStrings(parts), "; ")
}
