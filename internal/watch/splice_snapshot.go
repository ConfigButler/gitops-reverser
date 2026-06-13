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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// SpliceSnapshotForType computes one type's current desired set for a GitTarget by SPLICING the
// per-type materialization in Redis (checkpoint + log) instead of streaming the live API — the
// pivot of R2 (api-source-of-truth-reconcile.md §6). It is the splice twin of StreamSnapshotForType,
// reusing the same scope resolution (resolveSnapshotGVRForType) and the same DesiredResource shape
// (desiredFromObject), so a per-type reconcile is a drop-in consumer of the materialized API with
// ZERO per-reconcile API calls.
//
// The bool reports whether a snapshot is serviceable. It is false (with a nil error) for the two
// benign holds the caller must no-op on, never sweep:
//   - the GitTarget does not watch this type (nothing to reconcile);
//   - the type's checkpoint is not yet Synced (the materializer is still filling it, or a re-anchor
//     is in flight) — the fail-closed gate of §7: never sweep on an unobservable/partial view.
//
// A non-nil error is a genuine fail-closed condition (an unobserved API surface, a wobbling type,
// or a Redis/splice failure): the caller holds and surfaces it. On success the desired set is
// filtered to the GitTarget's namespaces and pinned to the checkpoint revision.
func (m *Manager) SpliceSnapshotForType(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
) (ClusterSnapshot, bool, error) {
	log := m.Log.WithValues("gitDest", gitDest.String(), "gvr", gvr.String())

	if m.TypeSplicer == nil {
		return ClusterSnapshot{}, false, fmt.Errorf("no type splicer wired for %s", gvr.String())
	}

	// Same fail-closed scope resolution the streaming path uses: refuses (error) on an unobserved
	// surface or a wobbling type, and reports !watched when this GitTarget does not follow the type.
	sg, watched, err := m.resolveSnapshotGVRForType(ctx, gitDest, gvr)
	if err != nil {
		return ClusterSnapshot{}, false, err
	}
	if !watched {
		return ClusterSnapshot{}, false, nil
	}

	// Fail-closed gate (§7): only a Synced checkpoint is a trustworthy full view. A Dormant /
	// Requested / Syncing type has no checkpoint to anchor on, and a Resyncing / Failing one is
	// mid-swap — hold in every case. The TypeSynced wake fires the splice exactly when this passes.
	if phase, _ := m.materializerInstance().Phase(gvr); phase != typeset.PhaseSynced {
		log.V(1).Info("per-type splice held: checkpoint not synced", "phase", phase)
		return ClusterSnapshot{}, false, nil
	}

	objs, revision, coverageHead, err := m.TypeSplicer.SpliceType(ctx, gvr.Group, gvr.Resource)
	if err != nil {
		return ClusterSnapshot{}, false, fmt.Errorf("splice %s for %s: %w", gvr.String(), gitDest.String(), err)
	}

	desired := scopeAndProjectSplicedObjects(gvr, objs, sg.namespaces)
	log.Info("Spliced per-type snapshot",
		"resources", len(desired), "revision", revision, "coverageHead", coverageHead)
	return ClusterSnapshot{Desired: desired, Revision: revision, CoverageHead: coverageHead}, true, nil
}

// scopeAndProjectSplicedObjects filters the folded objects to the GitTarget's namespaces and maps
// each into a DesiredResource via the same desiredFromObject the streaming path uses, so a spliced
// reconcile and a streamed reconcile produce byte-identical desired sets. An empty namespaces slice
// is a cluster-wide / all-namespaces selection (no filter).
func scopeAndProjectSplicedObjects(
	gvr schema.GroupVersionResource,
	objs []*unstructured.Unstructured,
	namespaces []string,
) []manifestanalyzer.DesiredResource {
	inScope := namespaceScopePredicate(namespaces)
	desired := make([]manifestanalyzer.DesiredResource, 0, len(objs))
	for _, obj := range objs {
		if !inScope(obj.GetNamespace()) {
			continue
		}
		if dr, ok := desiredFromObject(gvr, obj); ok {
			desired = append(desired, dr)
		}
	}
	return desired
}

// namespaceScopePredicate reports which object namespaces are in a GitTarget's scope for a type. An
// empty namespaces slice (cluster-wide stream: a cluster-scoped type, or a namespaced type a
// ClusterWatchRule follows everywhere) admits every object; otherwise only the named namespaces.
func namespaceScopePredicate(namespaces []string) func(string) bool {
	if len(namespaces) == 0 {
		return func(string) bool { return true }
	}
	set := make(map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		set[ns] = struct{}{}
	}
	return func(ns string) bool {
		_, ok := set[ns]
		return ok
	}
}
