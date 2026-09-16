// SPDX-License-Identifier: Apache-2.0

package watch

import (
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

// This file resolves the per-GitTarget watched-type scope, with the fail-closed discipline that
// protects the mark-and-sweep: an unobserved API surface, or a type currently held `retained` (a
// discovery wobble), refuses rather than reconciling a reduced view. The desired set itself is
// never gathered here. It is the target watch's `sendInitialEvents` replay. This file holds only
// scope resolution and the object to DesiredResource projection. See docs/architecture.md.

// retainedWatchedTypes returns the GVKs of the target's watched types the registry currently
// holds as `retained` (followable under the grace, but not served right now), resolved against
// the GitTarget's OWN source cluster's registry.
func (m *Manager) retainedWatchedTypes(
	gitDest types.ResourceReference,
	table WatchedTypeTable,
) []schema.GroupVersionKind {
	reg := m.registryForGitTarget(gitDest)
	var out []schema.GroupVersionKind
	for _, wt := range table.Types {
		if typeWobbling(reg, wt.GVR) {
			out = append(out, wt.GVK)
		}
	}
	return out
}

// typeWobbling reports whether a registry currently holds gvr as `retained` — followable under
// the removal grace, but not actually served right now (a discovery wobble). It is the single
// "do not reconcile or sweep this type" predicate, shared by the whole-GitTarget scope resolve
// and the per-type gate, so both fail closed on exactly the same registry verdict. The registry
// is the GitTarget's own source cluster's, so a wobble on one cluster never sweeps another's.
func typeWobbling(reg *typeset.Registry, gvr schema.GroupVersionResource) bool {
	rec, ok := reg.ByGVR(gvr)
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

// desiredFromObject converts a materialized object into a desired resource, pairing the
// GVR-derived API identity with the sanitized object the writer will materialise. Both paths
// that build a desired set go through it (the `sendInitialEvents` replay fold and the LIST
// fallback), so the set is shaped identically however the objects were sourced.
//
// It applies the SAME deletion-as-intent rule the live path applies in
// operationForLiveTargetWatchEvent: an object carrying a deletionTimestamp is logically absent
// from the intent tree, so it is absent from the desired set too. ok=false means "this object is
// not part of desired", which for a Terminating object is the whole point — a resync sweeps what
// desired omits, so leaving it out is what makes the snapshot REMOVE the file, exactly as the
// live DELETE would have.
//
// Without this the two gatherings disagreed, and which one ran decided whether a deleted resource
// came back: a replay or LIST fallback that happened to run while an object was still Terminating
// folded it into desired, and desired entries are applied as upserts. The object the live path had
// already removed from Git reappeared, then vanished again on the next resync once the finalizers
// cleared. Both commits were spurious, and because sanitize strips deletionTimestamp the
// resurrected manifest looked like an ordinary live resource.
func desiredFromObject(
	gvr schema.GroupVersionResource,
	obj interface{},
) (manifestanalyzer.DesiredResource, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		return manifestanalyzer.DesiredResource{}, false
	}
	// The deletionTimestamp is the signal, not the finalizer that holds it: an object with no
	// finalizer is moments from gone and is no more intended to exist than one held for hours.
	if u.GetDeletionTimestamp() != nil {
		return manifestanalyzer.DesiredResource{}, false
	}
	id := types.NewResourceIdentifier(gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName())
	return manifestanalyzer.DesiredResource{Resource: id, Object: sanitize.Sanitize(u)}, true
}
