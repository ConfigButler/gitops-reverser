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

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mapping"
)

// CatalogMapper is the live-catalog mapping.ResourceMapper. It reads the trusted,
// continuously refreshed APIResourceCatalog and never calls Kubernetes discovery
// directly — the catalog owns discovery refresh and trust state, the mapper only
// reads that local view. It shares the reduction logic in internal/mapping with
// the static-snapshot mapper, so the live and test mappers agree on every status.
type CatalogMapper struct {
	catalog *APIResourceCatalog
}

// NewCatalogMapper wraps a catalog as a live-catalog ResourceMapper.
func NewCatalogMapper(catalog *APIResourceCatalog) *CatalogMapper {
	return &CatalogMapper{catalog: catalog}
}

// Mapper returns a live-catalog ResourceMapper backed by this manager's catalog. The
// catalog is a stable pointer that the manager refreshes in place, so the returned
// mapper tracks discovery updates. Before the catalog has trusted data, lookups report
// CatalogUnavailable rather than false absence, so callers fall closed.
func (m *Manager) Mapper() mapping.ResourceMapper {
	return NewCatalogMapper(m.apiResourceCatalog())
}

var _ mapping.ResourceMapper = (*CatalogMapper)(nil)

// Source reports live-catalog.
func (m *CatalogMapper) Source() mapping.MapperSource { return mapping.MapperSourceLiveCatalog }

// Ready reflects the catalog's readiness, surfacing whether any group/version is
// currently degraded so callers do not trust absence under partial discovery.
func (m *CatalogMapper) Ready() mapping.MapperReadiness {
	if m.catalog == nil {
		return mapping.MapperReadiness{Reason: "no catalog"}
	}
	degraded := len(m.catalog.DegradedGroupVersions()) > 0
	reason := "live catalog"
	if degraded {
		reason = "live catalog with degraded discovery"
	}
	return mapping.MapperReadiness{
		Ready:      m.catalog.Ready(),
		Degraded:   degraded,
		Generation: m.catalog.Generation(),
		Reason:     reason,
	}
}

// Generation reports the catalog's published generation.
func (m *CatalogMapper) Generation() uint64 {
	if m.catalog == nil {
		return 0
	}
	return m.catalog.Generation()
}

// GVRForGVK resolves an exact GVK to its served GVR through the catalog.
func (m *CatalogMapper) GVRForGVK(
	ctx context.Context,
	gvk schema.GroupVersionKind,
) (mapping.Result, error) {
	if err := ctx.Err(); err != nil {
		return mapping.Result{}, err
	}
	if m.catalog == nil {
		return mapping.ResolveGVK(gvk, nil, mapping.LookupState{}), nil
	}
	lookup := m.catalog.LookupGVK(gvk)
	return mapping.ResolveGVK(gvk, mappingEntries(lookup.Entries), lookupState(lookup)), nil
}

// lookupState projects catalog trust state into the mapping reduction input.
func lookupState(lookup CatalogLookup) mapping.LookupState {
	return mapping.LookupState{
		Degraded:   lookup.Degraded,
		Ready:      lookup.Ready,
		Generation: lookup.Generation,
	}
}

// mappingEntries converts catalog entries to the catalog-neutral mapping.Entry
// view the reduction helpers operate on.
func mappingEntries(entries []APIResourceEntry) []mapping.Entry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]mapping.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, mapping.Entry{
			GVK:         e.GVK,
			GVR:         e.GVR,
			Namespaced:  e.Namespaced,
			Verbs:       verbSlice(e.Verbs),
			Preferred:   e.Preferred,
			Subresource: e.Subresource,
			Allowed:     e.Allowed,
		})
	}
	return out
}

// verbSlice flattens a catalog verb set into a slice for the mapping view.
func verbSlice(verbs map[string]struct{}) []string {
	if len(verbs) == 0 {
		return nil
	}
	out := make([]string, 0, len(verbs))
	for verb := range verbs {
		out = append(out, verb)
	}
	return out
}
