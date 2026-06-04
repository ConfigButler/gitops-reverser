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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
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
// manifest identity cannot be derived from content. The document is therefore located
// by its RESOLVED RESOURCE identity — the ByResourceIdentity index B3 built with the
// mapper — which is content-derived, so a manifest moved off its canonical generated
// path is still found (the exact gap the old path-derived scan left). When that index
// has no entry, the mapper reverse-maps the event GVR back to a GVK and the document is
// located by manifest identity instead.
//
// Deletion is content-agnostic (manifestedit.DeleteDocument never decrypts or merges),
// so an encrypted or non-editable document is still removed when its resource leaves the
// cluster — editability gates patches, not removals.
//
// Failure policy is fail-closed. If the resource-identity lookup misses and the mapper
// fallback cannot be trusted, PlanDelete returns an error rather than "nothing to
// delete", and the caller (M7) holds the delete and retries. Two cases fail closed: a Go
// error (discovery RPC failure, cancelled context), and a reverse-map status that means
// the API surface could not be OBSERVED (CatalogUnavailable / DiscoveryDegraded). The
// mapping design is explicit that an inability to observe the surface is not authoritative
// absence (docs/design/manifest/gvk-gvr-mapping-layer.md, "Failure Policy"), so it must
// not silently cancel a delete and leave a stale manifest. A TRUSTED "no served GVK"
// answer (Unserved / Disallowed / Subresource) is different: it is a genuine no-op.
//
// PlanDelete is a commit-boundary operation, not a per-event one: like the rest of the
// planner it reuses documentLocations / collidedIdentities (each O(store)). M7 hoists
// those per-commit maps so folding many deletes stays bounded by the batch; M9 caches
// across batches.
func PlanDelete(
	ctx context.Context,
	store *ManifestStore,
	mapper mapping.ResourceMapper,
	resource types.ResourceIdentifier,
) (PlanAction, bool, error) {
	dm, found, err := resolveDeleteTarget(ctx, store, mapper, resource)
	if err != nil {
		return PlanAction{}, false, err
	}
	if !found {
		// Git holds no managed document for this resource: the cluster dropped a
		// resource Git never materialised. Already converged — nothing to delete.
		return PlanAction{}, false, nil
	}
	if collidedIdentities(store)[dm.ManifestIdentity] {
		// A duplicate-identity collision refuses the whole GitTarget at acceptance
		// (M4), so the steady-state writer — which gates on Accept — never reaches a
		// collided identity here. Guard defensively anyway: deleting one arbitrary copy
		// of a collided identity is exactly the ambiguity the design refuses to guess at.
		return PlanAction{}, false, nil
	}
	return PlanAction{
		Kind:     PlanDeleteDocument,
		Ref:      documentLocations(store)[dm],
		Identity: dm.ManifestIdentity,
		Resource: resource,
		Reason:   "watched resource deleted from the cluster: drop its managed document",
	}, true, nil
}

// resolveDeleteTarget locates the managed document a GVR-based delete event targets.
//
//   - Primary: the resolved resource-identity index. It is content-derived (B3 built it
//     through the mapper), so a document found here is found regardless of where its file
//     sits — the moved-manifest case the milestone is about.
//   - Fallback: reverse-map the event GVR to a served GVK via the mapper and match by
//     manifest identity, for a document the resource index never indexed (e.g. a
//     structure-only store paired with a reverse-capable mapper). A TRUSTED "no served
//     GVK" answer (Unserved / Disallowed / Subresource) yields no managed document — a
//     no-op, not a guess. But an unobservable-surface answer (CatalogUnavailable /
//     DiscoveryDegraded) is NOT trusted absence: it returns an error so the caller fails
//     closed and retries, never silently dropping the delete (the mapping doc's Failure
//     Policy). StructureOnly likewise yields no managed document — a structure-only mapper
//     is never a delete driver.
//
// The second result is false (with a nil error) when Git holds no managed document for
// the resource.
func resolveDeleteTarget(
	ctx context.Context,
	store *ManifestStore,
	mapper mapping.ResourceMapper,
	resource types.ResourceIdentifier,
) (*DocumentModel, bool, error) {
	if dm := store.ByResourceIdentity[resource]; dm != nil {
		return dm, true, nil
	}
	if mapper == nil {
		return nil, false, nil
	}
	res, err := mapper.GVKForGVR(ctx, gvrOf(resource))
	if err != nil {
		return nil, false, err
	}
	// The API surface could not be observed (no trusted catalog yet, or discovery degraded
	// for this group/version). Per the mapping design's Failure Policy this is NOT
	// authoritative absence: returning "no managed document" would silently drop the delete
	// and leave a stale manifest. Fail closed so the caller retries once the catalog recovers.
	if res.Status == mapping.MappingCatalogUnavailable || res.Status == mapping.MappingDiscoveryDegraded {
		return nil, false, fmt.Errorf(
			"delete target for %s unresolved: API surface unobservable (%s); holding for retry",
			resource.Key(), res.Status)
	}
	// Any non-resolved status now names no single served GVK we could derive a manifest
	// identity from — trusted absence (Unserved / Ambiguous / Disallowed / Subresource) or
	// no API source at all (StructureOnly). Either way there is no managed document to
	// delete: a no-op, not a guess, and stable under retry (unlike the unobservable case).
	if res.Status != mapping.MappingResolved {
		return nil, false, nil
	}
	id := manifestedit.Identity{
		APIVersion: res.GVK.GroupVersion().String(),
		Kind:       res.GVK.Kind,
		Namespace:  resource.Namespace,
		Name:       resource.Name,
	}
	dm, ok := store.ByManifestIdentity[id]
	return dm, ok, nil
}

// gvrOf projects a ResourceIdentifier's GVR fields into a schema.GroupVersionResource
// for the reverse mapper lookup.
func gvrOf(r types.ResourceIdentifier) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
}
