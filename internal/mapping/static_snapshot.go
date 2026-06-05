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

package mapping

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Snapshot is a serialized, catalog-shaped fixture for the static-snapshot mapper.
// It is an explicit test/review input, not live discovery: it can model old
// clusters, partial catalogs, policy exclusions, ambiguity, and degraded discovery
// on purpose, but it must not be mistaken for proof about a running cluster.
type Snapshot struct {
	// Entries are the served resources the snapshot declares. Allowed defaults to
	// false on a zero Entry, so fixtures opt resources in explicitly by setting
	// Entry.Allowed.
	Entries []Entry
	// DegradedGroupVersions are group/versions whose discovery is modeled as failed,
	// so a miss in that scope reports DiscoveryDegraded instead of Unserved.
	DegradedGroupVersions []schema.GroupVersion
	// NotReady models a catalog that has no trusted data yet, so every miss reports
	// CatalogUnavailable.
	NotReady bool
	// Generation is the reported catalog generation.
	Generation uint64
}

// StaticSnapshotMapper answers lookups from a fixed Snapshot. It shares the same
// reduction logic as the live catalog mapper, so the two agree on every status.
type StaticSnapshotMapper struct {
	byGVK    map[schema.GroupVersionKind][]Entry
	degraded map[schema.GroupVersion]struct{}
	ready    bool
	gen      uint64
}

// NewStaticSnapshotMapper indexes a Snapshot into a static mapper.
func NewStaticSnapshotMapper(snap Snapshot) *StaticSnapshotMapper {
	mapper := &StaticSnapshotMapper{
		byGVK:    make(map[schema.GroupVersionKind][]Entry),
		degraded: make(map[schema.GroupVersion]struct{}, len(snap.DegradedGroupVersions)),
		ready:    !snap.NotReady,
		gen:      snap.Generation,
	}
	for _, entry := range snap.Entries {
		mapper.byGVK[entry.GVK] = append(mapper.byGVK[entry.GVK], entry)
	}
	for _, gv := range snap.DegradedGroupVersions {
		mapper.degraded[gv] = struct{}{}
	}
	return mapper
}

// Source reports static-snapshot.
func (m *StaticSnapshotMapper) Source() MapperSource { return MapperSourceStaticSnapshot }

// Ready reflects the snapshot's declared readiness and whether any group/version
// is modeled as degraded.
func (m *StaticSnapshotMapper) Ready() MapperReadiness {
	return MapperReadiness{
		Ready:      m.ready,
		Degraded:   len(m.degraded) > 0,
		Generation: m.gen,
		Reason:     "static snapshot",
	}
}

// Generation reports the snapshot generation.
func (m *StaticSnapshotMapper) Generation() uint64 { return m.gen }

// GVRForGVK resolves an exact GVK against the snapshot.
func (m *StaticSnapshotMapper) GVRForGVK(
	ctx context.Context,
	gvk schema.GroupVersionKind,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	state := m.stateFor(schema.GroupVersion{Group: gvk.Group, Version: gvk.Version})
	return ResolveGVK(gvk, m.byGVK[gvk], state), nil
}

func (m *StaticSnapshotMapper) stateFor(gv schema.GroupVersion) LookupState {
	_, degraded := m.degraded[gv]
	return LookupState{Degraded: degraded, Ready: m.ready, Generation: m.gen}
}
