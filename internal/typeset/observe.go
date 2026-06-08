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

package typeset

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Entry is one served API resource's raw facts — the neutral input to the scan. Both
// the live discovery catalog and a serialized snapshot convert their resources to
// this shape, so observation-building (identity uniqueness, origin, scale, policy)
// lives in exactly one place and the live and fixture paths agree on every verdict.
type Entry struct {
	GVK          schema.GroupVersionKind
	GVR          schema.GroupVersionResource
	Namespaced   bool
	Verbs        []string
	Preferred    bool
	Subresource  bool
	Allowed      bool   // product policy permits mirroring this resource
	PolicyReason string // why it is not allowed, when Allowed is false
	Degraded     bool   // the backing group/version is currently degraded
	// Sensitive reports whether this resource must use the encrypted Git write path.
	// It is a startup-known policy fact, applied by the entry builder (the catalog
	// applies the configured SensitiveResourcePolicy), not inferred inside typeset.
	Sensitive bool
}

// ObservationsFromEntries projects served entries into one Observation per top-level
// type — the "Scan -> Observation" reduction. Subresources are folded into their
// parent's record, never emitted as their own observation. catalogReady reports
// whether the backing scan holds trusted data, feeding the trusted requirement's
// catalog-unavailable distinction.
func ObservationsFromEntries(entries []Entry, catalogReady bool) []Observation {
	gvrsByGVK := distinctGVRsByGVK(entries)
	gvksByGVR := distinctGVKsByGVR(entries)
	scaleParents := subresourceParents(entries, "scale")
	statusParents := subresourceParents(entries, "status")

	out := make([]Observation, 0, len(entries))
	for _, e := range entries {
		if e.Subresource {
			continue
		}
		out = append(out, observationFromEntry(
			e, catalogReady, gvrsByGVK[e.GVK], gvksByGVR[e.GVR], scaleParents, statusParents,
		))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Identity.GVR.String() < out[j].Identity.GVR.String()
	})
	return out
}

// observationFromEntry builds one served type's observation from its entry plus the
// cross-entry facts (identity uniqueness in both directions, parent subresources).
// servingGVRs are the distinct resources serving this kind; servingGVKs are the
// distinct kinds this resource resolves to — a closed bijection requires both to be 1.
func observationFromEntry(
	e Entry,
	catalogReady bool,
	servingGVRs []schema.GroupVersionResource,
	servingGVKs []schema.GroupVersionKind,
	scaleParents, statusParents map[schema.GroupVersionResource]struct{},
) Observation {
	gvkUnique := len(servingGVRs) == 1
	gvrUnique := len(servingGVKs) == 1
	return Observation{
		Identity:          Identity{GVK: e.GVK, GVR: e.GVR, Scope: scopeFor(e.Namespaced)},
		Origin:            classifyOrigin(e.GVR.Group),
		Preferred:         e.Preferred,
		Verbs:             append([]string(nil), e.Verbs...),
		Subresources:      subresourcesFor(e.GVR, scaleParents, statusParents),
		Served:            true,
		Trusted:           !e.Degraded,
		CatalogReady:      catalogReady,
		GVKUnique:         gvkUnique,
		GVRUnique:         gvrUnique,
		GVKConflictDetail: gvkConflictDetail(gvkUnique, servingGVRs),
		GVRConflictDetail: gvrConflictDetail(gvrUnique, servingGVKs),
		Denied:            !e.Allowed,
		DenyDetail:        e.PolicyReason,
		Sensitive:         e.Sensitive,
		// Sensitive types route through the encrypted Git write path, a supported
		// handling, so sensitivity never refuses a followable type today.
		SensitiveSupported: true,
	}
}

// distinctGVRsByGVK indexes each kind to the distinct served top-level resources for
// it, so a kind served by more than one resource is recognised as non-unique identity
// (gvk-not-unique). Exact duplicates (same GVR) collapse, so a doubly-listed resource
// is not mistaken for a conflict.
func distinctGVRsByGVK(entries []Entry) map[schema.GroupVersionKind][]schema.GroupVersionResource {
	seen := map[schema.GroupVersionKind]map[schema.GroupVersionResource]struct{}{}
	for _, e := range entries {
		if e.Subresource {
			continue
		}
		if seen[e.GVK] == nil {
			seen[e.GVK] = map[schema.GroupVersionResource]struct{}{}
		}
		seen[e.GVK][e.GVR] = struct{}{}
	}
	out := make(map[schema.GroupVersionKind][]schema.GroupVersionResource, len(seen))
	for gvk, set := range seen {
		for gvr := range set {
			out[gvk] = append(out[gvk], gvr)
		}
	}
	return out
}

// distinctGVKsByGVR indexes each resource to the distinct kinds it resolves to, so a
// resource that resolves to more than one kind is recognised as non-unique identity
// (gvr-not-unique) — the reverse half of the GVK<->GVR bijection. Real discovery keeps
// a resource name unique per group/version, so this only fires for a malformed or
// hand-crafted (snapshot) surface; modelling it keeps the bijection honest in both
// directions rather than silently picking a winner.
func distinctGVKsByGVR(entries []Entry) map[schema.GroupVersionResource][]schema.GroupVersionKind {
	seen := map[schema.GroupVersionResource]map[schema.GroupVersionKind]struct{}{}
	for _, e := range entries {
		if e.Subresource {
			continue
		}
		if seen[e.GVR] == nil {
			seen[e.GVR] = map[schema.GroupVersionKind]struct{}{}
		}
		seen[e.GVR][e.GVK] = struct{}{}
	}
	out := make(map[schema.GroupVersionResource][]schema.GroupVersionKind, len(seen))
	for gvr, set := range seen {
		for gvk := range set {
			out[gvr] = append(out[gvr], gvk)
		}
	}
	return out
}

// subresourceParents returns the set of parent GVRs that expose the named
// subresource (e.g. "scale"), so a parent type can fold its subresource facts in.
func subresourceParents(entries []Entry, name string) map[schema.GroupVersionResource]struct{} {
	out := map[schema.GroupVersionResource]struct{}{}
	for _, e := range entries {
		parent, sub, ok := splitSubresource(e.GVR.Resource)
		if !ok || sub != name {
			continue
		}
		out[schema.GroupVersionResource{Group: e.GVR.Group, Version: e.GVR.Version, Resource: parent}] = struct{}{}
	}
	return out
}

// subresourcesFor folds a parent's /scale and /status facts into its record. Scale is
// enabled when the parent exposes a /scale subresource; its write binding comes from
// the built-in scale registry, and is left unusable for a CRD or aggregated parent
// whose replica path is not yet enriched (the scale requirement then refuses it rather
// than guessing .spec.replicas).
func subresourcesFor(
	gvr schema.GroupVersionResource,
	scaleParents, statusParents map[schema.GroupVersionResource]struct{},
) Subresources {
	var subs Subresources
	if _, ok := statusParents[gvr]; ok {
		subs.Status = StatusFact{Enabled: true}
	}
	if _, ok := scaleParents[gvr]; ok {
		if binding, known := BuiltinScale(gvr.Group, gvr.Resource); known {
			subs.Scale = binding
		} else {
			subs.Scale = ScaleBinding{Enabled: true, Source: "unknown", Usable: false}
		}
	}
	return subs
}

// splitSubresource splits "deployments/scale" into ("deployments", "scale", true). A
// name without a slash is a top-level resource and reports false.
func splitSubresource(resource string) (string, string, bool) {
	idx := strings.IndexByte(resource, '/')
	if idx < 0 {
		return "", "", false
	}
	return resource[:idx], resource[idx+1:], true
}

func scopeFor(namespaced bool) Scope {
	if namespaced {
		return ScopeNamespaced
	}
	return ScopeCluster
}

func gvkConflictDetail(unique bool, serving []schema.GroupVersionResource) string {
	if unique {
		return ""
	}
	resources := make([]string, 0, len(serving))
	for _, gvr := range serving {
		resources = append(resources, gvr.Resource)
	}
	sort.Strings(resources)
	return strings.Join(resources, ", ")
}

func gvrConflictDetail(unique bool, serving []schema.GroupVersionKind) string {
	if unique {
		return ""
	}
	kinds := make([]string, 0, len(serving))
	for _, gvk := range serving {
		kinds = append(kinds, gvk.Kind)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}

// builtinGroupSuffix marks the Kubernetes built-in API groups by their shared
// suffix; the core group is empty, and a handful of legacy groups have no suffix.
const builtinGroupSuffix = ".k8s.io"

// classifyOrigin infers a served type's origin from its API group. It is a shape
// heuristic, not evidence: the core group, the *.k8s.io groups, and the legacy
// built-in groups are builtin; everything else is treated as a CRD. Confidence is
// inferred, and it never returns unknown for a served type, so the origin requirement
// passes for every served type until real CRD/APIService evidence is wired in.
func classifyOrigin(group string) Origin {
	if group == "" || strings.HasSuffix(group, builtinGroupSuffix) || legacyBuiltinGroup(group) {
		return Origin{Kind: OriginBuiltin, Confidence: ConfidenceInferred}
	}
	return Origin{Kind: OriginCRD, Confidence: ConfidenceInferred}
}

func legacyBuiltinGroup(group string) bool {
	switch group {
	case "apps", "batch", "autoscaling", "policy", "extensions":
		return true
	default:
		return false
	}
}
