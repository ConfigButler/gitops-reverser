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

// openWindow is the one live commit-shaped event window owned by a branch
// worker. It accepts only events with the same author and target; repeated
// writes to the same Git path are last-write-wins while preserving first-seen
// path order.
type openWindow struct {
	// Author is event.UserInfo.Username verbatim.
	Author string
	// GitTarget is the target this window is bound to. Each finalized commit
	// covers exactly one target so per-target encryption and bootstrap
	// configuration apply unambiguously.
	GitTarget string
	// GitTargetNamespace is the namespace of the target, needed by the
	// BranchWorker to resolve the target's encryption configuration.
	GitTargetNamespace string

	// pathOrder records the order in which distinct Git paths were first
	// added to the window. Resources renders in this order so the message
	// reflects the burst's arrival shape.
	pathOrder []string
	// pathToEvent holds the latest event for each path inside the window.
	// Earlier events at the same path are subsumed (see GUI-toggle rationale
	// in the design).
	pathToEvent map[string]Event
}

const groupedCommitOperationKinds = 3

func newOpenWindow(e Event) *openWindow {
	return &openWindow{
		Author:             e.UserInfo.Username,
		GitTarget:          e.GitTargetName,
		GitTargetNamespace: e.GitTargetNamespace,
		pathToEvent:        make(map[string]Event),
	}
}

func (w *openWindow) canAppend(e Event) bool {
	if w == nil {
		return false
	}
	return e.UserInfo.Username == w.Author &&
		e.GitTargetName == w.GitTarget &&
		e.GitTargetNamespace == w.GitTargetNamespace
}

// add records an event in the window. If the path was already present the
// event replaces the previous one (last-write-wins inside a single window);
// otherwise pathOrder is extended.
func (w *openWindow) add(e Event) {
	key := windowPathKey(e)
	if _, exists := w.pathToEvent[key]; !exists {
		w.pathOrder = append(w.pathOrder, key)
	}
	w.pathToEvent[key] = e
}

// orderedEvents returns one event per distinct path, in the order paths were
// first seen. This is what gets applied to the worktree during commit.
func (w *openWindow) orderedEvents() []Event {
	out := make([]Event, 0, len(w.pathOrder))
	for _, key := range w.pathOrder {
		out = append(out, w.pathToEvent[key])
	}
	return out
}

// windowPathKey derives a stable key from an event's destination path so
// re-edits of the same resource inside an open window collapse.
func windowPathKey(e Event) string {
	filePath := generateFilePath(e.Identifier)
	if base := sanitizePath(e.Path); base != "" {
		return base + "/" + filePath
	}
	return filePath
}

// buildGroupedCommitMessageData produces the template context for a grouped
// commit unit. Operations are counted by Operation tag; Resources is the
// deduplicated list of resource refs in arrival order.
func buildGroupedCommitMessageData(author, gitTarget string, events []Event) GroupedCommitMessageData {
	operations := make(map[string]int, groupedCommitOperationKinds)
	resources := make([]ResourceRef, 0, len(events))
	for _, e := range events {
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
		Author:     author,
		GitTarget:  gitTarget,
		Count:      len(events),
		Operations: operations,
		Resources:  resources,
	}
}
