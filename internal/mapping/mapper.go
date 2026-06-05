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

// Package mapping is the GVK->GVR resolver described in
// docs/design/manifest/gvk-gvr-mapping-layer.md. It maps manifest identity
// (apiVersion+kind, i.e. a GroupVersionKind) to served resource identity
// (GroupVersionResource), reporting whether the mapping is trusted, unserved,
// ambiguous, disallowed, degraded, or unknowable.
//
// The package owns only the contract and the runtime-independent implementations
// (structure-only, static-snapshot). A catalog-backed implementation lives in
// internal/watch, where the trusted Kubernetes discovery catalog lives; it
// implements ResourceMapper here. Keeping the interface free of any controller or
// discovery dependency preserves the manifest analyzer's no-cluster promise: the
// analyzer can depend on this package without pulling in the watch manager.
package mapping

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResourceMapper maps manifest GVKs to served resource GVRs. It is an
// injected dependency: the controller backs it with the live discovery catalog, a
// CLI can back it with a kubeconfig-built catalog, tests use a static snapshot,
// and structure-only analysis uses a nil-equivalent mapper that always declines.
//
// Lookup methods return a Result whose Status carries the expected
// outcomes (unserved, ambiguous, disallowed, ...). Errors are reserved for
// implementation failures — a discovery/snapshot load failure or a cancelled
// context — never for "this GVK is not served".
type ResourceMapper interface {
	// Source reports how this process obtained (or declined to obtain) the API
	// discovery data behind the mapping.
	Source() MapperSource
	// Ready reports whether the mapper currently has trusted data to answer
	// watched/unwatched questions, plus any degraded state.
	Ready() MapperReadiness
	// Generation is the catalog generation answers are currently read at; it is 0
	// for sources without a refreshable catalog (structure-only).
	Generation() uint64

	// GVRForGVK resolves a manifest GVK to its served GVR. The match is exact: it
	// never bridges API versions (extensions/v1beta1 Deployment does not map to
	// apps/v1 Deployment).
	GVRForGVK(ctx context.Context, gvk schema.GroupVersionKind) (Result, error)
}

// MapperSource describes how the mapper obtained its API discovery data. It is
// not four competing truths — it is the provenance and freshness of one truth
// (Kubernetes discovery), or the explicit absence of it (structure-only).
type MapperSource string

const (
	// MapperSourceLiveCatalog reads the controller's continuously refreshed
	// in-process APIResourceCatalog.
	MapperSourceLiveCatalog MapperSource = "live-catalog"
	// MapperSourceKubeconfig builds a catalog from a kubeconfig discovery client on
	// demand (CLI/offline).
	MapperSourceKubeconfig MapperSource = "kubeconfig"
	// MapperSourceStaticSnapshot loads a serialized catalog-shaped fixture
	// (tests, CI, offline review).
	MapperSourceStaticSnapshot MapperSource = "static-snapshot"
	// MapperSourceStructureOnly has no API discovery data; mapping is deliberately
	// unknown. This preserves the analyzer's no-cluster mode.
	MapperSourceStructureOnly MapperSource = "structure-only"
)

// MapperReadiness summarizes whether the mapper can be trusted right now.
type MapperReadiness struct {
	// Ready is true when the mapper can answer lookups from trusted data.
	Ready bool
	// Degraded is true when some lookup scope is currently unobservable, so an
	// "unserved" answer in that scope must not be trusted as absence.
	Degraded bool
	// Generation is the catalog generation behind this readiness snapshot.
	Generation uint64
	// Reason is a short, display-only explanation; never parsed for decisions.
	Reason string
}

// Result is the outcome of one lookup. GVK and GVR are populated as far as
// the lookup got: a Resolved result fills both; an Unserved GVK lookup fills only
// the requested GVK. Status is the authoritative field — callers branch on it,
// not on Reason.
type Result struct {
	GVK schema.GroupVersionKind
	GVR schema.GroupVersionResource

	Namespaced bool
	Verbs      []string
	Preferred  bool
	Allowed    bool

	Status Status
	Reason string
}

// Status is the policy-relevant outcome of a lookup. Expected outcomes are
// statuses, not errors, so callers decide policy without parsing message strings.
type Status string

const (
	// MappingResolved means exactly one served, policy-allowed resource matched.
	MappingResolved Status = "Resolved"
	// MappingUnserved means trusted catalog data has no matching served resource.
	MappingUnserved Status = "Unserved"
	// MappingAmbiguous means more than one served resource could match.
	MappingAmbiguous Status = "Ambiguous"
	// MappingDisallowed means the resource is served but excluded by resource policy.
	MappingDisallowed Status = "Disallowed"
	// MappingSubresource means only a subresource (e.g. deployments/status) matched.
	MappingSubresource Status = "Subresource"
	// MappingCatalogUnavailable means no trusted catalog data exists yet.
	MappingCatalogUnavailable Status = "CatalogUnavailable"
	// MappingDiscoveryDegraded means discovery currently fails for the lookup scope.
	MappingDiscoveryDegraded Status = "DiscoveryDegraded"
	// MappingStructureOnly means no API source was consulted; structure alone is known.
	MappingStructureOnly Status = "StructureOnly"
)

// Entry is the catalog-neutral view of one served API resource the reduction
// helpers operate on. Both the catalog-backed mapper (internal/watch) and the
// static-snapshot mapper convert their entries to this shape so the status logic
// lives in exactly one place.
type Entry struct {
	GVK         schema.GroupVersionKind
	GVR         schema.GroupVersionResource
	Namespaced  bool
	Verbs       []string
	Preferred   bool
	Subresource bool
	Allowed     bool
}

// LookupState carries the catalog trust state a reduction needs to tell unserved
// apart from degraded and unavailable.
type LookupState struct {
	// Degraded reports discovery is currently failed for the lookup's group/version.
	Degraded bool
	// Ready reports the backing catalog has accepted any trusted discovery data.
	Ready bool
	// Generation is the catalog generation the entries were read at.
	Generation uint64
}

// ResolveGVK reduces the exact-GVK candidate entries (and trust state) into a
// Result. It is the single decision point for GVK lookups across all
// catalog-backed implementations.
//
// Among non-subresource candidates, resource policy is applied first: if none are
// allowed the kind is Disallowed; exactly one allowed entry is Resolved; more than
// one is Ambiguous. With no real resource match, a subresource-only match is
// Subresource, and an empty result is CatalogUnavailable (not ready), then
// DiscoveryDegraded (degraded scope), then Unserved.
func ResolveGVK(gvk schema.GroupVersionKind, candidates []Entry, state LookupState) Result {
	result := Result{GVK: gvk}

	served, subresourceOnly := partitionSubresources(candidates)
	if len(served) == 0 {
		if subresourceOnly {
			result.Status = MappingSubresource
			result.Reason = "only a subresource matched this kind"
			return result
		}
		result.Status, result.Reason = emptyStatus(state)
		return result
	}

	allowed := allowedEntries(served)
	switch {
	case len(allowed) == 0:
		result.Status = MappingDisallowed
		result.Reason = "served but excluded by resource policy"
	case len(allowed) > 1:
		result.Status = MappingAmbiguous
		result.Reason = "more than one served resource matches this kind"
	default:
		result = resolvedResult(gvk, allowed[0], state)
	}
	return result
}

// resolvedResult builds a Resolved GVK->GVR result.
func resolvedResult(gvk schema.GroupVersionKind, entry Entry, state LookupState) Result {
	return Result{
		GVK:        gvk,
		GVR:        entry.GVR,
		Namespaced: entry.Namespaced,
		Verbs:      sortedVerbs(entry.Verbs),
		Preferred:  entry.Preferred,
		Allowed:    entry.Allowed,
		Status:     MappingResolved,
		Reason:     reasonForGeneration(state),
	}
}

// emptyStatus classifies a no-candidate result using catalog trust state.
func emptyStatus(state LookupState) (Status, string) {
	switch {
	case !state.Ready:
		return MappingCatalogUnavailable, "no trusted catalog data yet"
	case state.Degraded:
		return MappingDiscoveryDegraded, "discovery is degraded for this group/version"
	default:
		return MappingUnserved, "no served resource matches"
	}
}

// partitionSubresources splits candidates into served (non-subresource) entries
// and reports whether the only matches were subresources. The second result lets
// a caller tell "kind served only as a subresource" apart from "kind not served".
func partitionSubresources(candidates []Entry) ([]Entry, bool) {
	var served []Entry
	sawSubresource := false
	for _, e := range candidates {
		if e.Subresource {
			sawSubresource = true
			continue
		}
		served = append(served, e)
	}
	return served, len(served) == 0 && sawSubresource
}

func allowedEntries(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Allowed {
			out = append(out, e)
		}
	}
	return out
}

func sortedVerbs(verbs []string) []string {
	if len(verbs) == 0 {
		return nil
	}
	out := append([]string(nil), verbs...)
	sort.Strings(out)
	return out
}

func reasonForGeneration(_ LookupState) string {
	return "resolved from trusted catalog data"
}
