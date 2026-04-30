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

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// makeEvent builds a ConfigMap event for grouping tests. value lets the same
// path appear with different data so the caller can verify "latest wins".
func makeEvent(author, target, name, value string) Event {
	return Event{
		Operation: "UPDATE",
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "default",
			Name:      name,
		},
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": "default",
				},
				"data": map[string]interface{}{"v": value},
			},
		},
		UserInfo:           UserInfo{Username: author},
		Path:               "team-" + target,
		GitTargetName:      target,
		GitTargetNamespace: "default",
	}
}

func TestGroupCommits_EmptyBuffer(t *testing.T) {
	groups := groupCommits(nil)
	assert.Empty(t, groups)
	assert.Empty(t, groupCommits([]Event{}))
}

// Two authors with disjoint resource sets in one window split on the author
// boundary regardless of path overlap.
func TestGroupCommits_TwoAuthorsDisjoint(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "A", "v1"),
		makeEvent("alice", "t", "B", "v1"),
		makeEvent("alice", "t", "C", "v1"),
		makeEvent("bob", "t", "D", "v1"),
		makeEvent("bob", "t", "E", "v1"),
	}
	groups := groupCommits(events)
	require.Len(t, groups, 2)

	assert.Equal(t, "alice", groups[0].Author)
	assert.Len(t, groups[0].pathOrder, 3)

	assert.Equal(t, "bob", groups[1].Author)
	assert.Len(t, groups[1].pathOrder, 2)
}

// Cross-author shared file: each author's write is its own commit, last
// writer wins on the tree.
func TestGroupCommits_TwoAuthorsSharedFile(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "F", "alice-v1"),
		makeEvent("bob", "t", "F", "bob-v1"),
	}
	groups := groupCommits(events)
	require.Len(t, groups, 2)

	assert.Equal(t, "alice", groups[0].Author)
	assert.Equal(t, "bob", groups[1].Author)
}

// GUI-toggle case: one author re-edits the same path many times in quick
// succession. Coalescing collapses the burst to a single commit holding the
// final state.
func TestGroupCommits_GUIToggleCollapsesToOne(t *testing.T) {
	const editCount = 15
	events := make([]Event, editCount)
	for i := range editCount {
		events[i] = makeEvent("alice", "t", "F", "v"+strconv.Itoa(i))
	}
	groups := groupCommits(events)
	require.Len(t, groups, 1, "fifteen same-author re-edits collapse to one group")
	assert.Equal(t, "alice", groups[0].Author)
	assert.Len(t, groups[0].pathOrder, 1, "only one path in the group")

	// The last edit wins inside the group.
	finalEvent := groups[0].pathToEvent[groupPathKey(events[0])]
	assert.Equal(
		t,
		"v"+strconv.Itoa(editCount-1),
		finalEvent.Object.Object["data"].(map[string]interface{})["v"],
		"the latest event for the path is what survives",
	)
}

// A → B → A produces three groups; we never reorder to coalesce A's writes
// across B's group, since that would violate arrival order.
func TestGroupCommits_AuthorAThenBThenA_NoReordering(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "a", "v1"),
		makeEvent("bob", "t", "b", "v1"),
		makeEvent("alice", "t", "c", "v1"),
	}
	groups := groupCommits(events)
	require.Len(t, groups, 3)

	assert.Equal(t, "alice", groups[0].Author)
	assert.Equal(t, "bob", groups[1].Author)
	assert.Equal(t, "alice", groups[2].Author)
}

// Same author, two different gitTargets in the same window produces two
// groups so per-target encryption / bootstrap stay clean.
func TestGroupCommits_SameAuthorTwoTargets(t *testing.T) {
	events := []Event{
		makeEvent("alice", "team-a", "X1", "v1"),
		makeEvent("alice", "team-a", "X2", "v1"),
		makeEvent("alice", "team-b", "Y1", "v1"),
		makeEvent("alice", "team-b", "Y2", "v1"),
	}
	groups := groupCommits(events)
	require.Len(t, groups, 2)

	assert.Equal(t, "alice", groups[0].Author)
	assert.Equal(t, "team-a", groups[0].GitTarget)

	assert.Equal(t, "alice", groups[1].Author)
	assert.Equal(t, "team-b", groups[1].GitTarget)
}

// Distinct author strings (e.g. service accounts) are never coalesced even
// when only the principal differs.
func TestGroupCommits_DistinctAuthorStringsNeverCoalesced(t *testing.T) {
	events := []Event{
		makeEvent("system:serviceaccount:ns:foo", "t", "A", "v1"),
		makeEvent("foo", "t", "B", "v1"),
		makeEvent("system:apiserver", "t", "C", "v1"),
	}
	groups := groupCommits(events)
	require.Len(t, groups, 3, "three different principal strings → three groups")
	assert.Equal(t, "system:serviceaccount:ns:foo", groups[0].Author)
	assert.Equal(t, "foo", groups[1].Author)
	assert.Equal(t, "system:apiserver", groups[2].Author)
}

// Rule (c): when same-author writes a path that a prior different-author
// group already committed, a new group splits to keep authorship honest.
// Subsequent same-author re-edits of that path then coalesce into the
// new group.
func TestGroupCommits_DifferentAuthorPathCollisionSplits(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "F", "alice-v1"),
		makeEvent("bob", "t", "F", "bob-v1"),
		makeEvent("alice", "t", "X", "alice-x1"), // new path; alice's second group opens
		makeEvent("alice", "t", "F", "alice-v2"), // F was previously committed by bob → split
		makeEvent("alice", "t", "F", "alice-v3"), // same group as previous: coalesce
	}
	groups := groupCommits(events)
	require.Len(t, groups, 4)

	// Group 0: alice {F}
	assert.Equal(t, "alice", groups[0].Author)
	assert.Len(t, groups[0].pathOrder, 1)

	// Group 1: bob {F}
	assert.Equal(t, "bob", groups[1].Author)
	assert.Len(t, groups[1].pathOrder, 1)

	// Group 2: alice {X}
	assert.Equal(t, "alice", groups[2].Author)
	assert.Len(t, groups[2].pathOrder, 1)

	// Group 3: alice {F} — alice's response to bob's F, plus the coalesced
	// re-edit. Latest version wins.
	assert.Equal(t, "alice", groups[3].Author)
	assert.Len(t, groups[3].pathOrder, 1, "the two alice F edits coalesce into one group")
	finalEvent := groups[3].pathToEvent[groupPathKey(events[3])]
	assert.Equal(
		t,
		"alice-v3",
		finalEvent.Object.Object["data"].(map[string]interface{})["v"],
		"latest write wins inside the new alice group",
	)
}

// Same-author path collision (without an intervening different-author write)
// must NOT split — it's the GUI-toggle pattern at scale.
func TestGroupCommits_SameAuthorPathCollisionDoesNotSplit(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "F", "v1"),
		makeEvent("alice", "t", "G", "v1"),
		makeEvent("alice", "t", "F", "v2"), // re-edit; same group
	}
	groups := groupCommits(events)
	require.Len(t, groups, 1, "all same author/target → single group")
	assert.Len(t, groups[0].pathOrder, 2, "two distinct paths inside the group")
}

// orderedEvents preserves the order in which paths first appeared in the
// group (so the rendered Resources list matches arrival shape).
func TestCommitGroup_OrderedEventsPreservesFirstSeen(t *testing.T) {
	events := []Event{
		makeEvent("alice", "t", "A", "v1"),
		makeEvent("alice", "t", "B", "v1"),
		makeEvent("alice", "t", "A", "v2"), // re-edit — does NOT move A in pathOrder
	}
	groups := groupCommits(events)
	require.Len(t, groups, 1)

	ordered := groups[0].orderedEvents()
	require.Len(t, ordered, 2)
	assert.Equal(t, "A", ordered[0].Identifier.Name, "A appeared first; should still be first")
	assert.Equal(t, "B", ordered[1].Identifier.Name)

	// Latest content wins for A.
	assert.Equal(t, "v2", ordered[0].Object.Object["data"].(map[string]interface{})["v"])
}

// buildGroupedCommitMessageData populates the Operations counter and
// Resources slice in arrival order — what the default groupTemplate consumes.
func TestBuildGroupedCommitMessageData_OperationsAndResources(t *testing.T) {
	createEv := makeEvent("alice", "t", "A", "v1")
	createEv.Operation = "CREATE"
	updateEv := makeEvent("alice", "t", "B", "v1")
	updateEv.Operation = "UPDATE"
	deleteEv := makeEvent("alice", "t", "C", "v1")
	deleteEv.Operation = "DELETE"

	events := []Event{createEv, updateEv, deleteEv}
	groups := groupCommits(events)
	require.Len(t, groups, 1)

	data := buildGroupedCommitMessageData(groups[0])
	assert.Equal(t, "alice", data.Author)
	assert.Equal(t, "t", data.GitTarget)
	assert.Equal(t, 3, data.Count)
	assert.Equal(t, map[string]int{"CREATE": 1, "UPDATE": 1, "DELETE": 1}, data.Operations)
	require.Len(t, data.Resources, 3)
	assert.Equal(t, "A", data.Resources[0].Name)
	assert.Equal(t, "B", data.Resources[1].Name)
	assert.Equal(t, "C", data.Resources[2].Name)
}

// renderGroupCommitMessage uses the GroupTemplate from the resolved config.
func TestRenderGroupCommitMessage_DefaultTemplate(t *testing.T) {
	events := []Event{makeEvent("alice", "team-a", "A", "v1")}
	groups := groupCommits(events)
	require.Len(t, groups, 1)

	msg, err := renderGroupCommitMessage(groups[0], ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "alice on team-a: 1 resource(s)", msg)
}
