// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sort"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Shared sources (docs/design/apply-trigger.md, "Shared sources").
//
// Flux appliers on one branch that apply one GitRepository form a group, and a group has one
// operation at a time: the artifact is shared, and an operation reads it again just before it
// resumes its Kustomization, so a sibling's fetch landing in between would make it apply another
// commit than the one it checked. The appliers of a group take turns (turnOrder). The group spans
// the workers writing the branch (apply_siblings.go): two GitProviders on one repository are two
// workers, and their triggers can apply one GitRepository, so a worker claims the group before it
// sends, and publishes the groups it has attempts out for.
//

// sourceGroup is the group a trigger's applier belongs to: its cluster and GitRepository, or "" for
// an applier that shares nothing this way.
func sourceGroup(trigger v1alpha3.ApplyTrigger) string {
	source, ok := trigger.SharedSource()
	if !ok {
		return ""
	}
	return trigger.ClusterProvider() + "/" + source.Namespace + "/" + source.Name
}

// group is the applier's group, as listed with any GitTarget naming it.
func (s *applierState) group() string {
	for _, target := range s.targets {
		return target.group
	}
	return ""
}

// attemptOut reports whether an attempt for the applier, or for another of its group, is out.
func (l *branchWorkerEventLoop) attemptOut(state *applierState) bool {
	return state.inflight != nil || l.groupBusy(state)
}

// groupBusy reports whether another applier of state's group has an attempt out, on this worker or
// on a sibling.
func (l *branchWorkerEventLoop) groupBusy(state *applierState) bool {
	group := state.group()
	if group == "" {
		return false
	}
	for _, other := range l.appliers {
		if other != state && other.inflight != nil && other.group() == group {
			return true
		}
	}
	return l.w.groupOutOnSibling(group)
}

// publishGroupsOut publishes the groups this worker has an attempt out for, for its siblings
// (branchSiblings.claimGroup). A group no longer out wakes them.
func (l *branchWorkerEventLoop) publishGroupsOut() {
	if l.w.siblings == nil {
		return
	}
	out := map[string]struct{}{}
	for _, state := range l.appliers {
		if group := state.group(); group != "" && state.inflight != nil {
			out[group] = struct{}{}
		}
	}
	l.w.siblings.publishGroupsOut(l.w, out)
}

// turnOrder is the appliers with the one whose last turn is oldest first, those never sent an
// attempt first of all, and by name between equals. Name order alone would starve a group: a push
// somebody else made while an operation is out is pending for every applier of the group, so the first
// by name would go again after each of its own operations, and pushes arriving faster than one
// operation lasts would never let its siblings apply.
func (l *branchWorkerEventLoop) turnOrder() []ApplierRef {
	refs := l.sortedAppliers()
	sort.SliceStable(refs, func(i, j int) bool {
		return l.appliers[refs[i]].lastTurn < l.appliers[refs[j]].lastTurn
	})
	return refs
}
