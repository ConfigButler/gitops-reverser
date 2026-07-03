// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

type apiResourceDiscovery interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

// APIResourceCatalog is the per-scan normalizer between Kubernetes discovery and the
// typeset registry: it turns one ServerGroupsAndResources() result into a
// policy-annotated typeset.Scan. It holds NO judgement and no time-sensitive state —
// retain-on-error and the removal grace for omissions both live in
// typeset.Registry.UpdateFromScan (see docs/design/typeset-owns-discovery-grace.md).
// The only state kept is mechanical bookkeeping: the last normalized scan (the change
// fingerprint, and the registry's re-derive source for refreshes without a discovery
// round-trip), the scan generation, and readiness.
type APIResourceCatalog struct {
	mu sync.RWMutex

	lastScan   typeset.Scan
	generation uint64
	ready      bool
}

// NewAPIResourceCatalog constructs an empty API resource catalog.
func NewAPIResourceCatalog() *APIResourceCatalog {
	return &APIResourceCatalog{}
}

// Ready reports whether the catalog has accepted any trusted discovery data.
func (c *APIResourceCatalog) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Generation reports the current scan generation. It bumps only when the normalized
// scan facts change, so downstream consumers gated on it do not churn on steady
// rescans.
func (c *APIResourceCatalog) Generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

// Refresh normalizes one discovery scan and stores it as the latest. It returns
// whether the normalized facts changed from the previous scan. It makes no retention
// decision of any kind: a group/version this scan failed on or no longer lists is
// simply reported as such in the scan; the registry judges what that means.
func (c *APIResourceCatalog) Refresh(disco apiResourceDiscovery) (bool, error) {
	scan, err := normalizeDiscoveryScan(disco)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	changed := !c.ready || !scansEquivalent(c.lastScan, scan)
	if changed {
		c.generation++
	}
	c.lastScan = scan
	if len(scan.ScannedGroupVersions) > 0 {
		c.ready = true
	}
	return changed, nil
}

// Scan returns the last normalized scan, policy-annotated and stamped with the
// current generation — the registry's one input (UpdateFromScan). ok is false before
// the first trusted scan. sensitive is the operator-configured
// SensitiveResourcePolicy, applied at projection exactly like the allow/deny resource
// policy is applied at normalization: a startup-known fact typeset never infers.
func (c *APIResourceCatalog) Scan(sensitive types.SensitiveResourcePolicy) (typeset.Scan, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return typeset.Scan{}, false
	}
	scan := c.lastScan
	scan.Generation = c.generation
	scan.Entries = make([]typeset.Entry, len(c.lastScan.Entries))
	for i, e := range c.lastScan.Entries {
		e.Sensitive = sensitive.IsSensitive(e.GVR.Group, e.GVR.Resource)
		scan.Entries[i] = e
	}
	return scan, true
}

// DegradedGroupVersions returns the group/versions the latest scan reported as
// failed, sorted for stable logging.
func (c *APIResourceCatalog) DegradedGroupVersions() []schema.GroupVersion {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := append([]schema.GroupVersion(nil), c.lastScan.FailedGroupVersions...)
	sortGroupVersions(out)
	return out
}

// CatalogStats is a point-in-time summary of the latest scan used to set the
// api_catalog_* gauges. All counts exclude subresources.
type CatalogStats struct {
	// AllowedResources is the count of served top-level resources the default
	// watch policy permits.
	AllowedResources int
	// ExcludedResources is the count of served top-level resources the default
	// watch policy excludes (pods, events, leases, …).
	ExcludedResources int
	// TrustedGroupVersions is the count of group/versions the latest scan served cleanly.
	TrustedGroupVersions int
	// DegradedGroupVersions is the count of group/versions the latest scan reported as failed.
	DegradedGroupVersions int
	// Generation is the current scan generation.
	Generation uint64
}

// Stats returns a point-in-time summary of the latest scan for metrics.
func (c *APIResourceCatalog) Stats() CatalogStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	stats := CatalogStats{
		TrustedGroupVersions:  len(c.lastScan.ScannedGroupVersions),
		DegradedGroupVersions: len(c.lastScan.FailedGroupVersions),
		Generation:            c.generation,
	}
	for _, e := range c.lastScan.Entries {
		if e.Subresource {
			continue
		}
		if e.Allowed {
			stats.AllowedResources++
		} else {
			stats.ExcludedResources++
		}
	}
	return stats
}

// normalizeDiscoveryScan reduces one ServerGroupsAndResources() result to per-scan
// facts: the served entries (policy-annotated, sorted), which group/versions were
// cleanly scanned, which failed, and whether the scan was complete. Sensitivity is
// applied later, at Scan() projection.
func normalizeDiscoveryScan(disco apiResourceDiscovery) (typeset.Scan, error) {
	groups, resources, err := disco.ServerGroupsAndResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return typeset.Scan{}, fmt.Errorf("discover server API resources: %w", err)
	}
	if groups == nil && len(resources) == 0 {
		if err != nil {
			return typeset.Scan{}, fmt.Errorf("discover server API resources: %w", err)
		}
		return typeset.Scan{Complete: true}, nil
	}

	failed, _ := discovery.GroupDiscoveryFailedErrorGroups(err)
	preferred := preferredVersions(groups)
	scan := typeset.Scan{Complete: err == nil}
	for gv := range failed {
		scan.FailedGroupVersions = append(scan.FailedGroupVersions, gv)
	}
	for _, resourceList := range resources {
		if resourceList == nil {
			continue
		}
		gv := parseGroupVersion(resourceList.GroupVersion).schema()
		if _, failedGV := failed[gv]; failedGV {
			continue
		}
		scan.ScannedGroupVersions = append(scan.ScannedGroupVersions, gv)
		scan.Entries = append(scan.Entries,
			scanEntriesForResourceList(resourceList, preferred[gv.Group] == gv.Version)...)
	}
	sortGroupVersions(scan.ScannedGroupVersions)
	sortGroupVersions(scan.FailedGroupVersions)
	sortScanEntries(scan.Entries)
	return scan, nil
}

// scanEntriesForResourceList converts one group/version's discovery resource list to
// neutral typeset entries, applying the served-resource (allow/deny) policy.
func scanEntriesForResourceList(resourceList *metav1.APIResourceList, preferred bool) []typeset.Entry {
	gv := parseGroupVersion(resourceList.GroupVersion).schema()
	entries := make([]typeset.Entry, 0, len(resourceList.APIResources))
	for _, resource := range resourceList.APIResources {
		name := normalizeResource(resource.Name)
		if name == "" {
			continue
		}
		allowed, reason := allowedResource(gv.Group, name)
		entries = append(entries, typeset.Entry{
			GVK: schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    resource.Kind,
			},
			GVR: schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: name,
			},
			Namespaced:   resource.Namespaced,
			Verbs:        sortedVerbs(resource.Verbs),
			Preferred:    preferred,
			Subresource:  strings.Contains(name, "/"),
			Allowed:      allowed,
			PolicyReason: reason,
		})
	}
	return entries
}

func sortedVerbs(verbs metav1.Verbs) []string {
	out := append([]string(nil), verbs...)
	sort.Strings(out)
	return out
}

func sortScanEntries(entries []typeset.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].GVR.String() < entries[j].GVR.String()
	})
}

func sortGroupVersions(gvs []schema.GroupVersion) {
	sort.Slice(gvs, func(i, j int) bool {
		if gvs[i].Group != gvs[j].Group {
			return gvs[i].Group < gvs[j].Group
		}
		return gvs[i].Version < gvs[j].Version
	})
}

// scansEquivalent reports whether two normalized scans carry the same facts. Both
// sides are sorted by normalizeDiscoveryScan, so this is a plain ordered compare.
// Generation is bookkeeping, not a fact, and is excluded.
func scansEquivalent(a, b typeset.Scan) bool {
	if a.Complete != b.Complete ||
		!groupVersionsEqual(a.ScannedGroupVersions, b.ScannedGroupVersions) ||
		!groupVersionsEqual(a.FailedGroupVersions, b.FailedGroupVersions) ||
		len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		if !scanEntriesEqual(a.Entries[i], b.Entries[i]) {
			return false
		}
	}
	return true
}

func groupVersionsEqual(a, b []schema.GroupVersion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func scanEntriesEqual(a, b typeset.Entry) bool {
	if a.GVK != b.GVK || a.GVR != b.GVR ||
		a.Namespaced != b.Namespaced ||
		a.Preferred != b.Preferred ||
		a.Subresource != b.Subresource ||
		a.Allowed != b.Allowed ||
		a.PolicyReason != b.PolicyReason ||
		len(a.Verbs) != len(b.Verbs) {
		return false
	}
	for i := range a.Verbs {
		if a.Verbs[i] != b.Verbs[i] {
			return false
		}
	}
	return true
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
