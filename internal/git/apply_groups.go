// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sort"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Shared sources (docs/design/apply-trigger.md, "Shared sources and prefetch").
//
// Flux appliers on one branch that apply one GitRepository form a group, and a group has one
// operation at a time: the artifact is shared, and an operation reads it again just before it
// resumes its Kustomization, so a sibling's fetch landing in between would make it apply another
// commit than the one it checked. The appliers of a group take turns (turnOrder). The group spans
// the workers writing the branch (apply_siblings.go): two GitProviders on one repository are two
// workers, and their triggers can apply one GitRepository, so a worker claims the group before it
// sends, and publishes the groups it has attempts out for.
//
// When every consumer of the GitRepository is held suspended by a FluxTrigger, a new artifact
// applies nothing, so a push somebody else made is fetched as soon as it is observed (prefetch)
// rather than when the branch is next quiet: the artifact is then ready when the apply is due. It
// is asked only while the branch is busy, because on a quiet branch the attempt sent at once fetches
// for itself, and never while an operation of the group is in flight.

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

// prefetchSources asks each group whose trigger may prefetch, and that has no attempt out, to fetch
// a push somebody else made. One request per group: its appliers share the GitRepository.
func (l *branchWorkerEventLoop) prefetchSources() {
	w := l.w
	tip, due := w.inbox.takePrefetch()
	if !due || w.applyRequester == nil {
		return
	}
	busy := map[string]bool{}
	first := map[string]*applierState{}
	for _, ref := range l.sortedAppliers() {
		state := l.appliers[ref]
		group := state.group()
		if group == "" {
			continue
		}
		busy[group] = busy[group] || state.inflight != nil
		if _, seen := first[group]; !seen && state.targets[0].prefetch {
			first[group] = state
		}
	}
	groups := make([]string, 0, len(first))
	for group := range first {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		if busy[group] || w.groupOutOnSibling(group) {
			continue
		}
		state := first[group]
		w.Log.V(1).Info("Fetching a push ahead of its apply", "branch", w.Branch,
			"applier", state.ref.String(), "commit", tip)
		w.applyRequester.PrefetchSource(PrefetchRequest{
			Applier: state.ref,
			Commit:  tip,
		})
	}
}
