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

package manifestreport

import (
	"sort"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Action is what the reconcile would do to bring Git in line with the cluster.
// The report is read-only: these are intents, never executed here.
type Action string

const (
	// ActionNoChange means Git already matches the desired projection.
	ActionNoChange Action = "no-change"
	// ActionUpdate means an existing document would be edited (patch or whole-replace).
	ActionUpdate Action = "update"
	// ActionCreate means a desired resource has no document in Git yet. Placement
	// is an upstream decision; this report only flags that a file would be created.
	ActionCreate Action = "create"
	// ActionDelete means a document exists in Git for a resource the cluster no
	// longer has (or a duplicate loser). A prune candidate — the prune trigger
	// lives in the reconcile layer, not here.
	ActionDelete Action = "delete"
	// ActionSkip means the document exists but cannot be edited in place
	// (encrypted, disallowed construct, non-KRM) — reported, never silently acted on.
	ActionSkip Action = "skip"
)

// Entry is one resource's verdict in the report.
type Entry struct {
	Identity manifestedit.Identity
	Action   Action
	// Location is the Git document this verdict concerns. It is the zero value for
	// ActionCreate, which has no existing location.
	Location manifestedit.Location
	// Reason is the human-readable explanation, carried straight from the Decide
	// reason for update/no-op/skip verdicts.
	Reason string
}

// Report is the read-only verdict over a (Git folder, cluster state) pair.
type Report struct {
	Entries []Entry
}

// Counts returns the number of entries per action, for a bounded summary.
func (r Report) Counts() map[Action]int {
	out := make(map[Action]int)
	for _, e := range r.Entries {
		out[e.Action]++
	}
	return out
}

// BuildReport is the read-only, inventory-driven reconcile: it indexes the Git
// folder, compares it to the desired cluster state, and reports what it would
// add, remove, or update — without mutating Git or touching the writer. It uses
// manifestedit.Decide only (never Apply), so it cannot change anything; this is
// the trust-building step before the comparison is wired into the commit path.
//
// The trust model is a single repository transaction: files must be the content
// of one checked-out commit/worktree, and the resulting verdicts are valid only
// for that snapshot. See
// docs/future/manifestedit-integration-readonly-reconcile.md.
func BuildReport(
	files []manifestedit.FileContent,
	desired []*unstructured.Unstructured,
) (Report, []manifestedit.Diagnostic) {
	inv, diags := manifestedit.IndexFiles(files)
	contentByPath := indexContent(files)
	opts := EditOptions()

	var entries []Entry
	desiredSeen := make(map[manifestedit.Identity]bool, len(desired))

	// Desired side: create / update / no-op / skip for every cluster object.
	for _, obj := range desired {
		id := identityOf(obj)
		desiredSeen[id] = true

		loc, ok := inv.Location(id)
		if !ok {
			// Git may still hold this identity as a non-editable document (encrypted,
			// disallowed construct, …). That is a skip — surfaced exactly once by
			// gitOnlyEntries below — not a create. Only a truly absent resource is a
			// create, so don't double-classify it here.
			if inventoryHasNonEditable(inv, id) {
				continue
			}
			entries = append(entries, Entry{
				Identity: id,
				Action:   ActionCreate,
				Reason:   "no existing document in Git; placement is an upstream decision",
			})
			continue
		}

		doc, _ := manifestedit.NewDocumentAt(loc.Path, contentByPath[loc.Path], loc.DocumentIndex)
		c := manifestedit.Comparison{Git: doc, Desired: Project(obj), Options: opts}
		d := manifestedit.Decide(c)
		entries = append(entries, Entry{
			Identity: id,
			Action:   actionFromDecision(d.Action),
			Location: loc,
			Reason:   d.Reason,
		})
	}

	entries = append(entries, gitOnlyEntries(inv, desiredSeen)...)

	sortEntries(entries)
	return Report{Entries: entries}, diags
}

// gitOnlyEntries flags documents in Git with no desired counterpart: prune
// candidates for authoritative records the cluster lacks, every duplicate loser,
// and a skip for records that are not editable at all.
func gitOnlyEntries(inv manifestedit.Inventory, desiredSeen map[manifestedit.Identity]bool) []Entry {
	var entries []Entry
	for _, rec := range inv.Records {
		if !rec.Editable {
			entries = append(entries, Entry{
				Identity: rec.Identity,
				Action:   ActionSkip,
				Location: rec.Location,
				Reason:   nonEditableReason(rec),
			})
			continue
		}
		// Only the authoritative location is a content delete here; a duplicate
		// loser is handled below regardless of whether the cluster still has it.
		loc, ok := inv.Location(rec.Identity)
		if ok && loc == rec.Location && !desiredSeen[rec.Identity] {
			entries = append(entries, Entry{
				Identity: rec.Identity,
				Action:   ActionDelete,
				Location: rec.Location,
				Reason:   "present in Git, absent from the cluster: prune candidate",
			})
		}
	}
	for _, dup := range inv.Duplicates() {
		entries = append(entries, Entry{
			Identity: dup.Identity,
			Action:   ActionDelete,
			Location: dup.Location,
			Reason:   "duplicate of the authoritative copy: prune candidate",
		})
	}
	return entries
}

// actionFromDecision maps a manifestedit decision intent to a report action.
func actionFromDecision(a manifestedit.DecisionAction) Action {
	switch a {
	case manifestedit.ActionNoChange:
		return ActionNoChange
	case manifestedit.ActionPatch, manifestedit.ActionReplace:
		return ActionUpdate
	case manifestedit.ActionDelete:
		return ActionDelete
	case manifestedit.ActionSkip:
		return ActionSkip
	default:
		return ActionSkip
	}
}

// inventoryHasNonEditable reports whether Git holds a non-editable document for
// the identity. The desired side uses this to defer to the skip gitOnlyEntries
// emits, instead of reporting a contradictory create for the same identity.
func inventoryHasNonEditable(inv manifestedit.Inventory, id manifestedit.Identity) bool {
	for _, rec := range inv.Records {
		if rec.Identity == id && !rec.Editable {
			return true
		}
	}
	return false
}

// nonEditableReason returns the recorded reason for a non-editable record, or a
// generic fallback.
func nonEditableReason(rec manifestedit.DocumentRecord) string {
	if rec.Reason != "" {
		return "not editable: " + rec.Reason
	}
	return "not editable"
}

// indexContent maps each file path to its raw bytes for document construction.
func indexContent(files []manifestedit.FileContent) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

// sortEntries orders the report deterministically by path, then document index,
// then identity, so output is stable regardless of map iteration order.
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Location.Path != b.Location.Path {
			return a.Location.Path < b.Location.Path
		}
		if a.Location.DocumentIndex != b.Location.DocumentIndex {
			return a.Location.DocumentIndex < b.Location.DocumentIndex
		}
		return identityString(a.Identity) < identityString(b.Identity)
	})
}

// identityString renders an identity for stable sorting and diagnostics.
func identityString(id manifestedit.Identity) string {
	return id.APIVersion + "/" + id.Kind + "/" + id.Namespace + "/" + id.Name
}
