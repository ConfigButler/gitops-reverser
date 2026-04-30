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

package git

// commitGroup represents one (author, gitTarget) commit produced by the
// commit-window batching pipeline. Each group collapses a contiguous arrival
// run of same-(author, gitTarget) events; multiple events for the same Git
// path inside the group keep only the final state (the latest event).
//
// See docs/design/commit-window-batching-design.md → Grouping key for the
// invariants this type encodes.
type commitGroup struct {
	// Author is event.UserInfo.Username verbatim.
	Author string
	// GitTarget is the target this group is bound to. Each grouped commit
	// covers exactly one target so per-target encryption and bootstrap
	// configuration apply unambiguously.
	GitTarget string
	// GitTargetNamespace is the namespace of the target — needed by the
	// BranchWorker to resolve the target's encryption configuration.
	GitTargetNamespace string

	// pathOrder records the order in which distinct Git paths were first
	// added to the group. Resources renders in this order so the message
	// reflects the burst's arrival shape.
	pathOrder []string
	// pathToEvent holds the latest event for each path inside the group.
	// Earlier events at the same path are subsumed (see GUI-toggle rationale
	// in the design).
	pathToEvent map[string]Event
}

const groupedCommitOperationKinds = 3

func newCommitGroup(e Event) *commitGroup {
	return &commitGroup{
		Author:             e.UserInfo.Username,
		GitTarget:          e.GitTargetName,
		GitTargetNamespace: e.GitTargetNamespace,
		pathToEvent:        make(map[string]Event),
	}
}

// add records an event in the group. If the path was already present the
// event replaces the previous one (last-write-wins inside a single group);
// otherwise pathOrder is extended.
func (g *commitGroup) add(e Event) {
	key := groupPathKey(e)
	if _, exists := g.pathToEvent[key]; !exists {
		g.pathOrder = append(g.pathOrder, key)
	}
	g.pathToEvent[key] = e
}

// orderedEvents returns one event per distinct path, in the order paths were
// first seen. This is what gets applied to the worktree during commit.
func (g *commitGroup) orderedEvents() []Event {
	out := make([]Event, 0, len(g.pathOrder))
	for _, key := range g.pathOrder {
		out = append(out, g.pathToEvent[key])
	}
	return out
}

// groupPathKey derives a stable key from an event's destination path so that
// re-edits of the same resource by the same author collapse, and so that
// path-collision detection across authors compares apples to apples.
func groupPathKey(e Event) string {
	filePath := generateFilePath(e.Identifier)
	if base := sanitizePath(e.Path); base != "" {
		return base + "/" + filePath
	}
	return filePath
}

// groupCommits walks the events in arrival order and produces one commitGroup
// per logical commit. The grouping rules (per design):
//
//   - A new group starts when (a) the author changes, (b) the gitTarget
//     changes, or (c) the file path of the incoming event collides with a
//     path already committed by a prior different-author group in this
//     flush.
//   - Same-(author, gitTarget) re-edits of the same path stay in the
//     same group — only the latest event per path is committed.
//
// The function is O(N) in the number of events with a small map lookup per
// event; the buffer is bounded by --branch-buffer-max-bytes.
func groupCommits(events []Event) []*commitGroup {
	if len(events) == 0 {
		return nil
	}

	groups := make([]*commitGroup, 0, 1)
	var current *commitGroup
	// seenPathAuthor records, per path, the author of the most recently
	// closed group that committed it. Rule (c) only fires when this prior
	// author differs from the current event's author — same-author re-edits
	// are intended to coalesce.
	seenPathAuthor := make(map[string]string)

	for _, e := range events {
		if shouldStartNewCommitGroup(current, seenPathAuthor, e) {
			groups = appendClosedCommitGroup(groups, current, seenPathAuthor)
			current = newCommitGroup(e)
		}

		current.add(e)
	}

	if current != nil {
		groups = append(groups, current)
	}
	return groups
}

func shouldStartNewCommitGroup(
	current *commitGroup,
	seenPathAuthor map[string]string,
	e Event,
) bool {
	if current == nil {
		return true
	}
	if e.UserInfo.Username != current.Author || e.GitTargetName != current.GitTarget {
		return true
	}

	// Rule (c) only matters when the incoming path is NEW to this group.
	// Re-edits of a path already in the group always coalesce.
	key := groupPathKey(e)
	if _, alreadyInGroup := current.pathToEvent[key]; alreadyInGroup {
		return false
	}

	priorAuthor, collides := seenPathAuthor[key]
	return collides && priorAuthor != current.Author
}

func appendClosedCommitGroup(
	groups []*commitGroup,
	current *commitGroup,
	seenPathAuthor map[string]string,
) []*commitGroup {
	if current == nil {
		return groups
	}

	groups = append(groups, current)
	for p := range current.pathToEvent {
		seenPathAuthor[p] = current.Author
	}
	return groups
}

// buildGroupedCommitMessageData produces the template context for the group.
// Operations are counted by Operation tag; Resources is the deduplicated list
// of resource refs in arrival order.
func buildGroupedCommitMessageData(g *commitGroup) GroupedCommitMessageData {
	ordered := g.orderedEvents()
	operations := make(map[string]int, groupedCommitOperationKinds)
	resources := make([]ResourceRef, 0, len(ordered))
	for _, e := range ordered {
		operations[e.Operation]++
		resources = append(resources, ResourceRef{
			Group:     e.Identifier.Group,
			Version:   e.Identifier.Version,
			Resource:  e.Identifier.Resource,
			Namespace: e.Identifier.Namespace,
			Name:      e.Identifier.Name,
		})
	}
	return GroupedCommitMessageData{
		Author:     g.Author,
		GitTarget:  g.GitTarget,
		Count:      len(ordered),
		Operations: operations,
		Resources:  resources,
	}
}
