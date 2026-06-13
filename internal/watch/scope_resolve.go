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
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// This file resolves the per-GitTarget watched-type scope the api-source-of-truth splice
// reconciles against, with the fail-closed discipline that protects the mark-and-sweep: an
// unobserved API surface, or a type currently held `retained` (a discovery wobble), refuses
// rather than reconciling a reduced view (R11, §7). The desired set itself is no longer
// gathered live — it is the spliced materialization (splice_snapshot.go) — so this file holds
// only scope resolution and the object→DesiredResource projection both the splice and the
// demand Declare share. See docs/design/stream/api-source-of-truth-reconcile.md.

// ClusterSnapshot is one type's revision-pinned desired set for a GitTarget: Desired is the
// scoped object set the worker folds over the git folder; Revision is the checkpoint
// resourceVersion the set is anchored at (it stays the commit-message {{.Revision}} and the
// resync request revision); CoverageHead is the splice coverage head Hc = max(checkpoint rv,
// highest folded audit-log entry rv), the value the per-(GitTarget, GVR) freshness watermark gates
// the audit tail on. CoverageHead >= Revision, and is strictly greater whenever post-checkpoint
// log entries were folded. See signing-snapshot-tail-replay-failure-investigation.md §5/§7.
type ClusterSnapshot struct {
	Desired      []manifestanalyzer.DesiredResource
	Revision     string
	CoverageHead string
}

// snapshotGVR is one resolved watched resource type with the namespace scope to gather it
// under: an empty namespaces slice means cluster-wide.
type snapshotGVR struct {
	gvr        schema.GroupVersionResource
	namespaces []string
}

// resolveSnapshotGVRForType resolves one watched type's (GVR, namespace-scope) for a GitTarget,
// with the same fail-closed discipline as resolveSnapshotGVRs but scoped to the single type. The
// bool is false when this GitTarget does not watch the type (so there is nothing to reconcile).
// It refuses (error) when the surface is unobserved or the type is currently `retained` (a
// wobble) — the per-type expression of the anti-sweep invariant (R9/R11).
func (m *Manager) resolveSnapshotGVRForType(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
) (snapshotGVR, bool, error) {
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return snapshotGVR{}, false, fmt.Errorf("refresh API resource catalog for %s: %w", gitDest.String(), err)
	}
	m.refreshWatchedTypeTables()

	if !m.typeRegistryInstance().Ready() {
		return snapshotGVR{}, false, fmt.Errorf(
			"aborting per-type reconcile for %s: the cluster API surface has not been observed yet",
			gitDest.String())
	}

	table := m.residentWatchedTypeTable(gitDest)
	var watched *WatchedType
	for i := range table.Types {
		if table.Types[i].GVR == gvr {
			watched = &table.Types[i]
			break
		}
	}
	if watched == nil {
		return snapshotGVR{}, false, nil
	}

	if m.typeWobbling(gvr) {
		return snapshotGVR{}, false, fmt.Errorf(
			"aborting per-type reconcile for %s: %s within the removal grace (currently unserved); "+
				"refusing to reconcile a reduced view",
			gitDest.String(), gvr.String())
	}

	return snapshotGVR{gvr: watched.GVR, namespaces: watched.SnapshotNamespaces()}, true, nil
}

// resolveSnapshotGVRs returns the GitTarget's watched (GVR, namespace-scope) set, read from the
// resident watched-type table. It refreshes the trusted API catalog, the registry, and the
// table first, then fails closed if the registry is not ready — a reconcile must never be built
// from an unobserved API surface, and a mark-and-sweep over a reduced view would delete KRM from
// git. A type that briefly leaves discovery stays followable (and so stays in the table) for the
// registry's removal grace, so a transient wobble never sweeps git. It is the scope side of the
// splice and the demand Declare (DEC-L3).
func (m *Manager) resolveSnapshotGVRs(
	ctx context.Context,
	gitDest types.ResourceReference,
) ([]snapshotGVR, error) {
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return nil, fmt.Errorf("refresh API resource catalog for %s: %w", gitDest.String(), err)
	}
	m.refreshWatchedTypeTables()

	if !m.typeRegistryInstance().Ready() {
		return nil, fmt.Errorf(
			"aborting scope resolution for %s: the cluster API surface has not been observed yet; "+
				"refusing to reconcile a partial cluster view",
			gitDest.String())
	}

	table := m.residentWatchedTypeTable(gitDest)

	// A watched type the registry holds as `retained` is followable under the removal grace but
	// is not actually served right now (a discovery wobble). Reconciling it would sweep a reduced
	// view and delete a still-valid mirror, so fail closed until the wobble resolves.
	if retained := m.retainedWatchedTypes(table); len(retained) > 0 {
		return nil, fmt.Errorf(
			"aborting scope resolution for %s: %s within the removal grace (currently unserved); "+
				"refusing to sweep a reduced cluster view",
			gitDest.String(), gvkListSummary(retained))
	}

	return snapshotGVRsFromTable(table), nil
}

// retainedWatchedTypes returns the GVKs of the target's watched types the registry currently
// holds as `retained` (followable under the grace, but not served right now).
func (m *Manager) retainedWatchedTypes(table WatchedTypeTable) []schema.GroupVersionKind {
	var out []schema.GroupVersionKind
	for _, wt := range table.Types {
		if m.typeWobbling(wt.GVR) {
			out = append(out, wt.GVK)
		}
	}
	return out
}

// typeWobbling reports whether the registry currently holds gvr as `retained` — followable under
// the removal grace, but not actually served right now (a discovery wobble). It is the single
// "do not reconcile or sweep this type" predicate, shared by the whole-GitTarget scope resolve
// and the per-type gate, so both fail closed on exactly the same registry verdict.
func (m *Manager) typeWobbling(gvr schema.GroupVersionResource) bool {
	rec, ok := m.typeRegistryInstance().ByGVR(gvr)
	return ok && rec.Followability.Verdict == typeset.VerdictRetained
}

// gvkListSummary renders held GVKs for the fail-closed error, naming each so a blocked reconcile
// log says exactly which wobbling types caused it.
func gvkListSummary(gvks []schema.GroupVersionKind) string {
	parts := make([]string, 0, len(gvks))
	for _, gvk := range gvks {
		parts = append(parts, gvk.String())
	}
	sort.Strings(parts)
	if len(parts) == 1 {
		return "watched type " + parts[0]
	}
	return fmt.Sprintf("%d watched types [%s]", len(parts), strings.Join(parts, ", "))
}

// snapshotGVRsFromTable projects a watched-type table into the deterministic, sorted
// (GVR, namespace-scope) set. A cluster-wide type yields no namespaces; the per-type
// SnapshotNamespaces collapse preserves the historic behaviour (a cluster-wide selection
// overrides any named namespaces).
func snapshotGVRsFromTable(table WatchedTypeTable) []snapshotGVR {
	out := make([]snapshotGVR, 0, len(table.Types))
	for _, wt := range table.Types {
		out = append(out, snapshotGVR{gvr: wt.GVR, namespaces: wt.SnapshotNamespaces()})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].gvr.String() < out[j].gvr.String()
	})
	return out
}

// desiredFromObject converts a materialized object into a desired resource, pairing the
// GVR-derived API identity with the sanitized object the writer will materialise. It is shared
// by the splice's scope projection (splice_snapshot.go) so a reconcile's desired set is shaped
// identically however the object was sourced.
func desiredFromObject(
	gvr schema.GroupVersionResource,
	obj interface{},
) (manifestanalyzer.DesiredResource, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		return manifestanalyzer.DesiredResource{}, false
	}
	id := types.NewResourceIdentifier(gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName())
	return manifestanalyzer.DesiredResource{Resource: id, Object: sanitize.Sanitize(u)}, true
}
