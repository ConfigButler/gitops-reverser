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
// resumes its Kustomization, so another applier's fetch landing in between would make it apply
// another commit than the one it checked. The appliers of a group take turns (turnOrder). One
// worker writes a repository branch (branch_owner.go), so a group's appliers are all its own.

// sourceGroup is the group a trigger's applier belongs to: its cluster and GitRepository, or "" for
// an applier that shares nothing this way.
func sourceGroup(trigger v1alpha3.ApplyTrigger) string {
	source, ok := trigger.SharedSource()
	if !ok {
		return ""
	}
	return trigger.KubeProviderKey().String() + "/" + source.Namespace + "/" + source.Name
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

// groupBusy reports whether another applier of state's group has an attempt out.
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
	return false
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
