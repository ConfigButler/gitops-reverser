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
	"sort"
	"strings"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// ResolveMissReason describes why one declared resource did not become a GVR.
type ResolveMissReason string

const (
	// ResolveMissNotServed means trusted catalog data has no matching resource.
	ResolveMissNotServed ResolveMissReason = "NotServed"
	// ResolveMissAmbiguous means an omitted apiGroups selector found multiple groups.
	ResolveMissAmbiguous ResolveMissReason = "Ambiguous"
	// ResolveMissDisallowed means resource policy excludes a served resource.
	ResolveMissDisallowed ResolveMissReason = "Disallowed"
	// ResolveMissWildcardGroup means explicit "*" apiGroups expansion is unsupported.
	ResolveMissWildcardGroup ResolveMissReason = "WildcardGroup"
	// ResolveMissWildcardResource means explicit "*" resource expansion is unsupported.
	ResolveMissWildcardResource ResolveMissReason = "WildcardResource"
	// ResolveMissCatalogUnavailable means discovery has not populated a catalog yet.
	ResolveMissCatalogUnavailable ResolveMissReason = "CatalogUnavailable"
	// ResolveMissDiscoveryDegraded means failed discovery can change the result.
	ResolveMissDiscoveryDegraded ResolveMissReason = "DiscoveryDegraded"
)

// ResolveMiss captures one declared resource that could not be planned.
type ResolveMiss struct {
	Resource string
	Reason   ResolveMissReason
	Detail   string
}

// RuleGVRResolver applies WatchRule resource semantics to APIResourceCatalog.
type RuleGVRResolver struct {
	catalog *APIResourceCatalog
}

// NewRuleGVRResolver creates a resolver over a catalog.
func NewRuleGVRResolver(catalog *APIResourceCatalog) *RuleGVRResolver {
	return &RuleGVRResolver{catalog: catalog}
}

// Resolve maps one rule shape to concrete watchable GVRs.
func (r *RuleGVRResolver) Resolve(
	groups, versions, resources []string,
	scope configv1alpha1.ResourceScope,
) ([]GVR, []ResolveMiss) {
	var gvrs []GVR
	var misses []ResolveMiss
	for _, resource := range resources {
		resolved, miss := r.resolveResource(groups, versions, normalizeResource(resource), scope)
		gvrs = append(gvrs, resolved...)
		misses = append(misses, miss...)
	}
	return dedupeGVRs(gvrs), misses
}

func (r *RuleGVRResolver) resolveResource(
	groups, versions []string,
	resource string,
	scope configv1alpha1.ResourceScope,
) ([]GVR, []ResolveMiss) {
	if misses, stop := r.preflightMisses(groups, resource); stop {
		return nil, misses
	}

	candidates := r.resourceCandidates(groups, resource)
	candidates = filterCandidateVersions(candidates, versions)
	candidates = filterScope(candidates, scope)
	if len(candidates) == 0 {
		return nil, []ResolveMiss{r.emptyCandidateMiss(groups, versions, resource)}
	}

	if miss := ambiguityMiss(groups, resource, candidates); miss != nil {
		return nil, []ResolveMiss{*miss}
	}
	candidates = choosePreferredVersions(candidates, versions)
	if miss := disallowedMiss(resource, candidates); miss != nil {
		return nil, []ResolveMiss{*miss}
	}

	return gvrsForCandidates(resource, candidates, scope)
}

func (r *RuleGVRResolver) preflightMisses(groups []string, resource string) ([]ResolveMiss, bool) {
	switch {
	case resource == "":
		return nil, true
	case resource == "*":
		return []ResolveMiss{
			newResolveMiss(resource, ResolveMissWildcardResource, "resource wildcard expansion is unsupported"),
		}, true
	case hasWildcard(groups):
		return []ResolveMiss{
			newResolveMiss(resource, ResolveMissWildcardGroup, "apiGroups wildcard expansion is unsupported"),
		}, true
	case strings.Contains(resource, "/"):
		return []ResolveMiss{
			newResolveMiss(resource, ResolveMissNotServed, "subresource planning is unsupported"),
		}, true
	case r.catalog == nil || !r.catalog.Ready():
		return []ResolveMiss{
			newResolveMiss(resource, ResolveMissCatalogUnavailable, "API resource catalog is not ready"),
		}, true
	default:
		return nil, false
	}
}

func (r *RuleGVRResolver) emptyCandidateMiss(groups, versions []string, resource string) ResolveMiss {
	if r.catalog.hasDegradedLookup(groups, versions) {
		return newResolveMiss(resource, ResolveMissDiscoveryDegraded,
			"discovery is degraded for a lookup scope that may serve this resource")
	}
	return newResolveMiss(resource, ResolveMissNotServed, "resource is not served")
}

func ambiguityMiss(groups []string, resource string, candidates []APIResourceEntry) *ResolveMiss {
	if len(groups) != 0 {
		return nil
	}
	servedGroups := candidateGroups(candidates)
	if len(servedGroups) <= 1 {
		return nil
	}
	miss := newResolveMiss(resource, ResolveMissAmbiguous,
		fmt.Sprintf("set apiGroups to one of [%s]", strings.Join(quoteStrings(servedGroups), ", ")))
	return &miss
}

func gvrsForCandidates(
	resource string,
	candidates []APIResourceEntry,
	scope configv1alpha1.ResourceScope,
) ([]GVR, []ResolveMiss) {
	var out []GVR
	for _, candidate := range candidates {
		if !candidate.Allowed || candidate.Subresource || !candidate.Supports("list", "watch") {
			continue
		}
		out = append(out, GVR{
			Group:    candidate.GVR.Group,
			Version:  candidate.GVR.Version,
			Resource: candidate.GVR.Resource,
			Scope:    scope,
		})
	}
	if len(out) == 0 {
		return nil, []ResolveMiss{newResolveMiss(resource, ResolveMissNotServed,
			"resource does not support GitOps Reverser list and watch planning")}
	}
	return out, nil
}

func (r *RuleGVRResolver) resourceCandidates(groups []string, resource string) []APIResourceEntry {
	if len(groups) == 0 {
		return r.catalog.entriesForResource(resource)
	}
	var out []APIResourceEntry
	for _, group := range groups {
		out = append(out, r.catalog.entriesForGroupResource(strings.TrimSpace(group), resource)...)
	}
	return out
}

func filterCandidateVersions(entries []APIResourceEntry, versions []string) []APIResourceEntry {
	if len(versions) == 0 || hasWildcard(versions) {
		return entries
	}
	var out []APIResourceEntry
	for _, entry := range entries {
		if matchLookupValue(versions, entry.GVR.Version) {
			out = append(out, entry)
		}
	}
	return out
}

func filterScope(entries []APIResourceEntry, scope configv1alpha1.ResourceScope) []APIResourceEntry {
	var out []APIResourceEntry
	for _, entry := range entries {
		if matchesScope(entry.Namespaced, scope) {
			out = append(out, entry)
		}
	}
	return out
}

func choosePreferredVersions(entries []APIResourceEntry, versions []string) []APIResourceEntry {
	if len(versions) != 0 && !hasWildcard(versions) {
		return entries
	}
	byGroupResource := make(map[string][]APIResourceEntry)
	for _, entry := range entries {
		key := groupResourceKey(entry.GVR.Group, entry.GVR.Resource)
		byGroupResource[key] = append(byGroupResource[key], entry)
	}
	var out []APIResourceEntry
	for _, candidates := range byGroupResource {
		sortCatalogEntries(candidates)
		selected := candidates[0]
		for _, candidate := range candidates {
			if candidate.Preferred {
				selected = candidate
				break
			}
		}
		out = append(out, selected)
	}
	sortCatalogEntries(out)
	return out
}

func disallowedMiss(resource string, entries []APIResourceEntry) *ResolveMiss {
	for _, entry := range entries {
		if entry.Allowed {
			return nil
		}
	}
	entry := entries[0]
	detail := fmt.Sprintf(
		"%s/%s is served but %s",
		entry.GVR.GroupVersion().String(),
		entry.GVR.Resource,
		entry.PolicyReason,
	)
	miss := newResolveMiss(resource, ResolveMissDisallowed, detail)
	return &miss
}

func newResolveMiss(resource string, reason ResolveMissReason, detail string) ResolveMiss {
	return ResolveMiss{Resource: resource, Reason: reason, Detail: detail}
}

func candidateGroups(entries []APIResourceEntry) []string {
	groups := make(map[string]struct{})
	for _, entry := range entries {
		groups[entry.GVR.Group] = struct{}{}
	}
	out := make([]string, 0, len(groups))
	for group := range groups {
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}

func quoteStrings(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fmt.Sprintf("%q", value)
	}
	return out
}

// matchesScope reports whether a discovery namespaced flag aligns with a
// declared resource scope.
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

func hasWildcard(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "*" {
			return true
		}
	}
	return false
}

func dedupeGVRs(in []GVR) []GVR {
	seen := make(map[GVR]struct{}, len(in))
	out := make([]GVR, 0, len(in))
	for _, gvr := range in {
		if _, ok := seen[gvr]; ok {
			continue
		}
		seen[gvr] = struct{}{}
		out = append(out, gvr)
	}
	return out
}
