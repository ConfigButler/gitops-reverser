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

package manifestanalyzer

import (
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// PlanDelete resolves a steady-state DELETE watch event to a single delete-document
// plan action over the store, or no action when Git holds no managed document for the
// resource. It is the M6 milestone: closing the delete-identity gap the review names
// (docs/design/manifest/current-manifest-support-review.md, "Cons And Gaps") so a
// moved manifest is still deleted, and the writer (M7) deletes by RecordRef instead of
// regenerating a canonical path.
//
// This is the per-event delete path of the design's "Two Paths, One Plan Type"
// (docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md). Unlike BuildPlan's
// full-snapshot mark-and-sweep, it targets exactly ONE identity and NEVER sweeps, so a
// lone delete intent can never be mistaken for "every other document is now an orphan".
// M7's steady-state loop folds this over its coalesced PendingChanges (a delete is a
// PendingChange whose Object is nil).
//
// A DELETE event carries only a GVR-based resource identity and NO object body, so the
// manifest identity cannot be derived from the event. The document is therefore located
// only by its RESOLVED RESOURCE identity — the ByResourceIdentity index B3 built while
// scanning the GitTarget folder. If that inventory has no entry, there is no managed
// document to delete.
//
// Deletion is content-agnostic (manifestedit.DeleteDocument never decrypts or merges),
// so an encrypted or non-editable document is still removed when its resource leaves the
// cluster — editability gates patches, not removals.
//
// PlanDelete is a commit-boundary operation, not a per-event one: like the rest of the
// planner it reuses documentLocations / collidedIdentities (each O(store)). M7 hoists
// those per-commit maps so folding many deletes stays bounded by the batch; M9 caches
// across batches.
func PlanDelete(
	store *ManifestStore,
	resource types.ResourceIdentifier,
) (PlanAction, bool) {
	dm, found := resolveDeleteTarget(store, resource)
	if !found {
		// Git holds no managed document for this resource: the cluster dropped a
		// resource Git never materialised. Already converged — nothing to delete.
		return PlanAction{}, false
	}
	if collidedIdentities(store)[dm.ManifestIdentity] {
		// A duplicate-identity collision refuses the whole GitTarget at acceptance
		// (M4), so the steady-state writer — which gates on Accept — never reaches a
		// collided identity here. Guard defensively anyway: deleting one arbitrary copy
		// of a collided identity is exactly the ambiguity the design refuses to guess at.
		return PlanAction{}, false
	}
	return PlanAction{
		Kind:     PlanDeleteDocument,
		Ref:      documentLocations(store)[dm],
		Identity: dm.ManifestIdentity,
		Resource: resource,
		Reason:   "watched resource deleted from the cluster: drop its managed document",
	}, true
}

// resolveDeleteTarget locates the managed document a GVR-based delete event targets.
// The second result is false when Git holds no managed document for the resource.
func resolveDeleteTarget(store *ManifestStore, resource types.ResourceIdentifier) (*DocumentModel, bool) {
	if dm := store.ByResourceIdentity[resource]; dm != nil {
		return dm, true
	}
	return nil, false
}
