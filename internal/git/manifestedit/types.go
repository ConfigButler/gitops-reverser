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

/*
Package manifestedit is an isolated proof of concept for the manifest-inventory
"file-agnostic placement" feature. It indexes Kubernetes resources from YAML
content and edits a single document in place while preserving the formatting of
everything it did not change.

It is intentionally throw-away: the package proves whether gopkg.in/yaml.v3 node
editing is good enough before any of this is wired into the real writer. See
docs/future/manifest-parser-poc.md.
*/
package manifestedit

// Identity is the manifest (content) identity of a Kubernetes object: the GVK
// plus name and, for namespaced objects, namespace, exactly as written in YAML.
// It is deliberately not the API-side resource identity (GVR); mapping a GVK to
// a GVR needs a live RESTMapper and is out of scope for this POC.
type Identity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// Location points at one document inside one file, relative to the scan root.
type Location struct {
	Path          string
	DocumentIndex int
}

// DiagnosticLevel classifies how serious a diagnostic is.
type DiagnosticLevel string

const (
	// DiagInfo is informational and never blocks editing.
	DiagInfo DiagnosticLevel = "info"
	// DiagWarning marks something skipped or ignored but not fatal to the file.
	DiagWarning DiagnosticLevel = "warning"
	// DiagError marks content that cannot be edited safely.
	DiagError DiagnosticLevel = "error"
)

// Diagnostic explains an inventory or edit decision.
type Diagnostic struct {
	Level         DiagnosticLevel
	Message       string
	Path          string
	DocumentIndex int
}

// DocumentRecord is one indexed Kubernetes document.
type DocumentRecord struct {
	Identity Identity
	Location Location
	// Editable is false when the document uses constructs the POC refuses to edit
	// (anchors, aliases, merge keys) or when it lost a duplicate-identity contest.
	Editable bool
	// Reason explains a non-editable record.
	Reason string
	// Encrypted is true for a SOPS-managed document with cleartext identity.
	Encrypted bool
}

// Inventory is the mapping from resource identity to its authoritative location,
// plus the full list of records and any duplicate losers that must be deleted.
type Inventory struct {
	// Records are all indexed documents in stable scan order (path, then index).
	Records []DocumentRecord
	// byIdentity holds the winning location for each identity.
	byIdentity map[Identity]Location
	// duplicates are records that lost the first-occurrence-wins contest and
	// should be deleted so Git converges to a single copy.
	duplicates []DocumentRecord
}

// Location returns the authoritative location for an identity, if indexed.
func (inv Inventory) Location(id Identity) (Location, bool) {
	loc, ok := inv.byIdentity[id]
	return loc, ok
}

// Duplicates returns the records that lost the first-occurrence-wins contest.
func (inv Inventory) Duplicates() []DocumentRecord {
	return inv.duplicates
}

// Summary is a compact, bounded overview of an inventory. The vision flags that
// GitTarget status cannot enumerate thousands of manifests, so this seeds the
// "high-level stats first" direction: a status surface shows these counts and
// keeps per-resource detail for a separate read path.
type Summary struct {
	Documents   int
	Editable    int
	NonEditable int
	Encrypted   int
	Duplicates  int
}

// Summary returns bounded counts over the inventory.
func (inv Inventory) Summary() Summary {
	s := Summary{Duplicates: len(inv.duplicates)}
	for _, r := range inv.Records {
		s.Documents++
		if r.Editable {
			s.Editable++
		} else {
			s.NonEditable++
		}
		if r.Encrypted {
			s.Encrypted++
		}
	}
	return s
}

// CountByLevel groups diagnostics by severity, for a bounded status summary
// instead of listing every diagnostic.
func CountByLevel(diags []Diagnostic) map[DiagnosticLevel]int {
	out := make(map[DiagnosticLevel]int)
	for _, d := range diags {
		out[d.Level]++
	}
	return out
}

// EditMode describes what PatchDocument did.
type EditMode string

const (
	// EditNoChange means the document already matched the clean desired projection.
	EditNoChange EditMode = "no-change"
	// EditPatched means only the changed nodes were updated in place.
	EditPatched EditMode = "patched"
	// EditWholeReplace means the whole document body was re-rendered as a fallback.
	EditWholeReplace EditMode = "whole-replace"
	// EditSkipped means the document was left untouched because editing was unsafe.
	EditSkipped EditMode = "skipped"
)

// EditResult is the outcome of editing one document.
type EditResult struct {
	// Content is the full file content after the edit.
	Content []byte
	Mode    EditMode
}

// FileContent pairs a path with its raw bytes for multi-file indexing.
type FileContent struct {
	Path    string
	Content []byte
}
