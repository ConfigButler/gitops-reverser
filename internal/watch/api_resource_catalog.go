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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// APIResourceEntry is one served Kubernetes API resource in the local catalog.
type APIResourceEntry struct {
	GVR          schema.GroupVersionResource
	GVK          schema.GroupVersionKind
	Namespaced   bool
	Verbs        map[string]struct{}
	Preferred    bool
	Subresource  bool
	Allowed      bool
	PolicyReason string
}

// Supports reports whether discovery advertised all requested verbs.
func (e APIResourceEntry) Supports(verbs ...string) bool {
	for _, verb := range verbs {
		if _, ok := e.Verbs[verb]; !ok {
			return false
		}
	}
	return true
}

type apiResourceDiscovery interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

type catalogGroupVersionState struct {
	trusted  bool
	degraded bool
}

type catalogRefreshCandidate struct {
	entries   map[schema.GroupVersion][]APIResourceEntry
	failed    map[schema.GroupVersion]error
	supported map[schema.GroupVersion]struct{}
	complete  bool
}

// APIResourceCatalog is GitOps Reverser's trusted in-memory Kubernetes API surface.
type APIResourceCatalog struct {
	mu sync.RWMutex

	byGVR        map[schema.GroupVersionResource]APIResourceEntry
	byGVK        map[schema.GroupVersionKind][]APIResourceEntry
	byResource   map[string][]APIResourceEntry
	byGroupRes   map[string][]APIResourceEntry
	byGroupVer   map[schema.GroupVersion][]APIResourceEntry
	groupVersion map[schema.GroupVersion]catalogGroupVersionState
	generation   uint64
	ready        bool
}

// NewAPIResourceCatalog constructs an empty API resource catalog.
func NewAPIResourceCatalog() *APIResourceCatalog {
	return &APIResourceCatalog{
		byGVR:        make(map[schema.GroupVersionResource]APIResourceEntry),
		byGVK:        make(map[schema.GroupVersionKind][]APIResourceEntry),
		byResource:   make(map[string][]APIResourceEntry),
		byGroupRes:   make(map[string][]APIResourceEntry),
		byGroupVer:   make(map[schema.GroupVersion][]APIResourceEntry),
		groupVersion: make(map[schema.GroupVersion]catalogGroupVersionState),
	}
}

// Ready reports whether the catalog has accepted any trusted discovery data.
func (c *APIResourceCatalog) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Generation reports the current published catalog generation.
func (c *APIResourceCatalog) Generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

// CatalogStats is a point-in-time summary of the catalog used to set the
// api_catalog_* gauges. All counts exclude subresources.
type CatalogStats struct {
	// AllowedResources is the count of served top-level resources the default
	// watch policy permits.
	AllowedResources int
	// ExcludedResources is the count of served top-level resources the default
	// watch policy excludes (pods, events, leases, …).
	ExcludedResources int
	// TrustedGroupVersions is the count of group/versions discovery served cleanly.
	TrustedGroupVersions int
	// DegradedGroupVersions is the count of group/versions discovery reported as failed.
	DegradedGroupVersions int
	// Generation is the current published catalog generation.
	Generation uint64
}

// Stats returns a point-in-time summary of the catalog for metrics. Counts are
// computed under the existing read lock.
func (c *APIResourceCatalog) Stats() CatalogStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	stats := CatalogStats{Generation: c.generation}
	for _, entry := range c.byGVR {
		if entry.Subresource {
			continue
		}
		if entry.Allowed {
			stats.AllowedResources++
		} else {
			stats.ExcludedResources++
		}
	}
	for _, state := range c.groupVersion {
		switch {
		case state.degraded:
			stats.DegradedGroupVersions++
		case state.trusted:
			stats.TrustedGroupVersions++
		}
	}
	return stats
}

// DegradedGroupVersions returns the group/versions discovery currently reports
// as failed, sorted for stable logging.
func (c *APIResourceCatalog) DegradedGroupVersions() []schema.GroupVersion {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]schema.GroupVersion, 0)
	for gv, state := range c.groupVersion {
		if state.degraded {
			out = append(out, gv)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// Refresh updates clean group/versions and preserves entries for group/versions
// that discovery reports as failed.
func (c *APIResourceCatalog) Refresh(disco apiResourceDiscovery) (bool, error) {
	candidate, err := discoverCatalogRefresh(disco)
	if err != nil || candidate == nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.initializeMaps()
	changed := c.applyCleanGroupVersions(candidate.entries)
	if c.markFailedGroupVersions(candidate.failed) {
		changed = true
	}
	if candidate.complete && c.removeUndiscoveredGroupVersions(candidate.supported) {
		changed = true
	}
	if changed {
		c.rebuildIndexesLocked()
		c.generation++
	}
	return changed, nil
}

func discoverCatalogRefresh(disco apiResourceDiscovery) (*catalogRefreshCandidate, error) {
	groups, resources, err := disco.ServerGroupsAndResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return nil, fmt.Errorf("discover server API resources: %w", err)
	}
	if groups == nil && len(resources) == 0 {
		if err != nil {
			return nil, fmt.Errorf("discover server API resources: %w", err)
		}
		return &catalogRefreshCandidate{
			entries:   map[schema.GroupVersion][]APIResourceEntry{},
			failed:    map[schema.GroupVersion]error{},
			supported: map[schema.GroupVersion]struct{}{},
			complete:  true,
		}, nil
	}

	failed, _ := discovery.GroupDiscoveryFailedErrorGroups(err)
	preferred := preferredVersions(groups)
	candidate := &catalogRefreshCandidate{
		entries:   make(map[schema.GroupVersion][]APIResourceEntry, len(resources)),
		failed:    failed,
		supported: supportedGroupVersions(groups),
		complete:  err == nil,
	}
	for _, resourceList := range resources {
		if resourceList == nil {
			continue
		}
		gv := parseGroupVersion(resourceList.GroupVersion).schema()
		if _, failedGV := failed[gv]; failedGV {
			continue
		}
		candidate.entries[gv] = catalogEntriesForResourceList(resourceList, preferred[gv.Group] == gv.Version)
	}
	return candidate, nil
}

func (c *APIResourceCatalog) applyCleanGroupVersions(
	candidate map[schema.GroupVersion][]APIResourceEntry,
) bool {
	changed := false
	for gv, entries := range candidate {
		if !catalogEntriesEqual(c.byGroupVer[gv], entries) ||
			c.groupVersion[gv] != (catalogGroupVersionState{trusted: true}) {
			changed = true
		}
		c.byGroupVer[gv] = entries
		c.groupVersion[gv] = catalogGroupVersionState{trusted: true}
		c.ready = true
	}
	return changed
}

func (c *APIResourceCatalog) markFailedGroupVersions(failed map[schema.GroupVersion]error) bool {
	changed := false
	for gv := range failed {
		state := c.groupVersion[gv]
		if !state.degraded {
			changed = true
		}
		state.degraded = true
		c.groupVersion[gv] = state
	}
	return changed
}

func (c *APIResourceCatalog) removeUndiscoveredGroupVersions(supported map[schema.GroupVersion]struct{}) bool {
	changed := false
	for gv := range c.byGroupVer {
		if _, ok := supported[gv]; ok {
			continue
		}
		delete(c.byGroupVer, gv)
		delete(c.groupVersion, gv)
		changed = true
	}
	return changed
}

// Entry returns one concrete catalog entry.
func (c *APIResourceCatalog) Entry(gvr schema.GroupVersionResource) (APIResourceEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.byGVR[gvr]
	return cloneAPIResourceEntry(entry), ok
}

// CatalogLookup is the trusted local answer to an exact GVK or GVR question. It
// carries the matched served entries plus enough catalog trust state for the
// mapping layer to tell "not served" apart from "could not be observed":
//
//   - Entries are the served catalog entries that match exactly. A GVR lookup is
//     single-valued (zero or one entry); a GVK lookup may return several (multiple
//     groups/versions serving the same kind, or a resource and its subresources).
//   - Degraded reports that discovery is currently failed for the looked-up
//     group/version, so an empty Entries set must not be trusted as absence.
//   - Ready mirrors the catalog readiness at lookup time; an empty result from a
//     not-ready catalog is "catalog unavailable", not "unserved".
//   - Generation is the published catalog generation the answer was read at.
type CatalogLookup struct {
	Entries    []APIResourceEntry
	Degraded   bool
	Ready      bool
	Generation uint64
}

// LookupGVK answers an exact group/version/kind question against trusted catalog
// data. It never infers a resource from the kind string; an unmatched GVK returns
// empty Entries, and the caller distinguishes unserved, degraded, and unavailable
// using Degraded/Ready. Subresource entries that share the kind are included so a
// caller can recognise a subresource-only match.
func (c *APIResourceCatalog) LookupGVK(gvk schema.GroupVersionKind) CatalogLookup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CatalogLookup{
		Entries:    cloneAPIResourceEntries(c.byGVK[gvk]),
		Degraded:   c.degradedForGroupVersionLocked(schema.GroupVersion{Group: gvk.Group, Version: gvk.Version}),
		Ready:      c.ready,
		Generation: c.generation,
	}
}

// LookupGVR answers an exact group/version/resource question against trusted
// catalog data. Entries holds the single matching served entry when present.
func (c *APIResourceCatalog) LookupGVR(gvr schema.GroupVersionResource) CatalogLookup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var entries []APIResourceEntry
	if entry, ok := c.byGVR[gvr]; ok {
		entries = cloneAPIResourceEntries([]APIResourceEntry{entry})
	}
	return CatalogLookup{
		Entries:    entries,
		Degraded:   c.degradedForGroupVersionLocked(schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}),
		Ready:      c.ready,
		Generation: c.generation,
	}
}

// degradedForGroupVersionLocked reports whether discovery currently fails for an
// exact group/version. Callers must hold the read lock.
func (c *APIResourceCatalog) degradedForGroupVersionLocked(gv schema.GroupVersion) bool {
	return c.groupVersion[gv].degraded
}

func (c *APIResourceCatalog) entriesForResource(resource string) []APIResourceEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneAPIResourceEntries(c.byResource[resource])
}

func (c *APIResourceCatalog) entriesForGroup(group string) []APIResourceEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []APIResourceEntry
	for gv, entries := range c.byGroupVer {
		if gv.Group == group {
			out = append(out, entries...)
		}
	}
	sortCatalogEntries(out)
	return cloneAPIResourceEntries(out)
}

func (c *APIResourceCatalog) entriesForGroupResource(group, resource string) []APIResourceEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneAPIResourceEntries(c.byGroupRes[groupResourceKey(group, resource)])
}

func (c *APIResourceCatalog) allEntries() []APIResourceEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]APIResourceEntry, 0, len(c.byGVR))
	for _, entry := range c.byGVR {
		out = append(out, entry)
	}
	sortCatalogEntries(out)
	return cloneAPIResourceEntries(out)
}

func (c *APIResourceCatalog) hasDegradedLookup(groups, versions []string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for gv, state := range c.groupVersion {
		if !state.degraded {
			continue
		}
		if matchLookupValue(groups, gv.Group) && matchLookupValue(versions, gv.Version) {
			return true
		}
	}
	return false
}

func (c *APIResourceCatalog) initializeMaps() {
	if c.byGVR == nil {
		c.byGVR = make(map[schema.GroupVersionResource]APIResourceEntry)
	}
	if c.byGVK == nil {
		c.byGVK = make(map[schema.GroupVersionKind][]APIResourceEntry)
	}
	if c.byResource == nil {
		c.byResource = make(map[string][]APIResourceEntry)
	}
	if c.byGroupRes == nil {
		c.byGroupRes = make(map[string][]APIResourceEntry)
	}
	if c.byGroupVer == nil {
		c.byGroupVer = make(map[schema.GroupVersion][]APIResourceEntry)
	}
	if c.groupVersion == nil {
		c.groupVersion = make(map[schema.GroupVersion]catalogGroupVersionState)
	}
}

func (c *APIResourceCatalog) rebuildIndexesLocked() {
	c.byGVR = make(map[schema.GroupVersionResource]APIResourceEntry)
	c.byGVK = make(map[schema.GroupVersionKind][]APIResourceEntry)
	c.byResource = make(map[string][]APIResourceEntry)
	c.byGroupRes = make(map[string][]APIResourceEntry)
	for _, entries := range c.byGroupVer {
		for _, entry := range entries {
			c.byGVR[entry.GVR] = entry
			c.byGVK[entry.GVK] = append(c.byGVK[entry.GVK], entry)
			c.byResource[entry.GVR.Resource] = append(c.byResource[entry.GVR.Resource], entry)
			key := groupResourceKey(entry.GVR.Group, entry.GVR.Resource)
			c.byGroupRes[key] = append(c.byGroupRes[key], entry)
		}
	}
	for key := range c.byGVK {
		sortCatalogEntries(c.byGVK[key])
	}
	for key := range c.byResource {
		sortCatalogEntries(c.byResource[key])
	}
	for key := range c.byGroupRes {
		sortCatalogEntries(c.byGroupRes[key])
	}
}

func preferredVersions(groups []*metav1.APIGroup) map[string]string {
	out := make(map[string]string, len(groups))
	for _, group := range groups {
		if group == nil {
			continue
		}
		if group.PreferredVersion.Version != "" {
			out[group.Name] = group.PreferredVersion.Version
		}
	}
	return out
}

func supportedGroupVersions(groups []*metav1.APIGroup) map[schema.GroupVersion]struct{} {
	out := make(map[schema.GroupVersion]struct{})
	for _, group := range groups {
		if group == nil {
			continue
		}
		for _, version := range group.Versions {
			out[schema.GroupVersion{Group: group.Name, Version: version.Version}] = struct{}{}
		}
	}
	return out
}

func catalogEntriesForResourceList(resourceList *metav1.APIResourceList, preferred bool) []APIResourceEntry {
	gv := parseGroupVersion(resourceList.GroupVersion).schema()
	entries := make([]APIResourceEntry, 0, len(resourceList.APIResources))
	for _, resource := range resourceList.APIResources {
		name := normalizeResource(resource.Name)
		if name == "" {
			continue
		}
		allowed, reason := allowedResource(gv.Group, name)
		entries = append(entries, APIResourceEntry{
			GVR: schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: name,
			},
			GVK: schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    resource.Kind,
			},
			Namespaced:   resource.Namespaced,
			Verbs:        resourceVerbs(resource.Verbs),
			Preferred:    preferred,
			Subresource:  strings.Contains(name, "/"),
			Allowed:      allowed,
			PolicyReason: reason,
		})
	}
	sortCatalogEntries(entries)
	return entries
}

func resourceVerbs(verbs metav1.Verbs) map[string]struct{} {
	out := make(map[string]struct{}, len(verbs))
	for _, verb := range verbs {
		out[verb] = struct{}{}
	}
	return out
}

func sortCatalogEntries(entries []APIResourceEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return key(entries[i].GVR.Group, entries[i].GVR.Version, entries[i].GVR.Resource) <
			key(entries[j].GVR.Group, entries[j].GVR.Version, entries[j].GVR.Resource)
	})
}

func catalogEntriesEqual(left, right []APIResourceEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].GVR != right[i].GVR ||
			left[i].GVK != right[i].GVK ||
			left[i].Namespaced != right[i].Namespaced ||
			left[i].Preferred != right[i].Preferred ||
			left[i].Subresource != right[i].Subresource ||
			left[i].Allowed != right[i].Allowed ||
			left[i].PolicyReason != right[i].PolicyReason ||
			!verbSetsEqual(left[i].Verbs, right[i].Verbs) {
			return false
		}
	}
	return true
}

func verbSetsEqual(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for verb := range left {
		if _, ok := right[verb]; !ok {
			return false
		}
	}
	return true
}

func cloneAPIResourceEntries(entries []APIResourceEntry) []APIResourceEntry {
	out := make([]APIResourceEntry, len(entries))
	for i := range entries {
		out[i] = cloneAPIResourceEntry(entries[i])
	}
	return out
}

func cloneAPIResourceEntry(entry APIResourceEntry) APIResourceEntry {
	entry.Verbs = resourceVerbs(metav1.Verbs(mapKeys(entry.Verbs)))
	return entry
}

func mapKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}

func groupResourceKey(group, resource string) string {
	return group + "|" + resource
}

func matchLookupValue(selectors []string, value string) bool {
	if len(selectors) == 0 {
		return true
	}
	for _, selector := range selectors {
		if selector == "*" || selector == value {
			return true
		}
	}
	return false
}

func (gv groupVersion) schema() schema.GroupVersion {
	return schema.GroupVersion{Group: gv.group, Version: gv.version}
}

// groupVersionSplit prevents magic numbers when splitting group/version strings.
const groupVersionSplit = 2

type groupVersion struct {
	group   string
	version string
}

// parseGroupVersion splits a discovery "group/version" string, treating a
// single-segment value as the core API group (e.g. "v1").
func parseGroupVersion(gvString string) groupVersion {
	parts := strings.SplitN(gvString, "/", groupVersionSplit)
	if len(parts) == 1 {
		return groupVersion{group: "", version: parts[0]}
	}
	return groupVersion{group: parts[0], version: parts[1]}
}

// key builds a stable group|version|resource index key.
func key(group, version, resource string) string {
	return group + "|" + version + "|" + resource
}
