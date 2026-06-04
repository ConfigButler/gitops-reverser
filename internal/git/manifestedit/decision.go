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

package manifestedit

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Everything this package does is a function of two representations of the same
// Kubernetes object: the Git version (a document at a known location) and the
// desired version (the clean object Git should contain). Comparison makes that
// two-version comparison a first-class value; Decide is a pure preflight over it
// and Apply is the authoritative edit. See
// docs/design/manifest/manifestedit-abstraction-plan.md.

// Document is immutable data describing one target document inside a file: the
// whole file content, the target document index, and the manifest identity. It
// deliberately carries no parsed node tree — Decide and Apply each parse
// internally, so nothing one mutates can affect the other. This is what lets
// Decide stay non-mutating.
type Document struct {
	// Path is the file location relative to the scan root, carried so Apply's
	// diagnostics can name the file (e.g. "apps/deploy.yaml doc 0"), not just the
	// document index. It is informational: it does not affect the edit.
	Path string
	// Content is the whole file, so Apply can splice the edited document back
	// among its untouched siblings.
	Content []byte
	// DocumentIndex is the target document's position within the file.
	DocumentIndex int
	// Identity is the manifest identity of the target document, as written.
	Identity Identity
}

// NewDocument builds a Document for one target document with no known file path.
// See NewDocumentAt to carry the path for diagnostics.
func NewDocument(content []byte, documentIndex int) (*Document, bool) {
	return NewDocumentAt("", content, documentIndex)
}

// NewDocumentAt builds a Document for one target document at a known path,
// deriving its identity from the content. ok is false when the index is out of
// range; the Document is still returned (with a zero Identity) so callers can
// hand it to Decide, which reports the out-of-range condition as a skip.
func NewDocumentAt(path string, content []byte, documentIndex int) (*Document, bool) {
	doc := &Document{Path: path, Content: content, DocumentIndex: documentIndex}
	docs := splitDocuments(string(content))
	if documentIndex < 0 || documentIndex >= len(docs) {
		return doc, false
	}
	if root, empty, err := decodeDoc(docs[documentIndex].body); err == nil && !empty {
		doc.Identity, _ = identityFromNode(root)
	}
	return doc, true
}

// FieldPath is a path to a node within a document, used by the ownership
// predicate. The root object is the empty path; "spec", "replicas" addresses
// spec.replicas.
type FieldPath []string

// ListMatchStrategy aligns desired and Git sequence items. The zero value
// matches by index (today's behavior). A keyed strategy names the field to
// match on; the GVK->field choice is made above this layer, never baked into
// the YAML merge.
type ListMatchStrategy struct {
	// KeyField, when set, matches list items by that field instead of by index.
	KeyField string
}

// EditOptions carries the injected strategies. They are the one seam where later
// strategies plug in, so the core merge stays small and pure.
type EditOptions struct {
	// Render is the canonical renderer for whole-document replacement and new
	// files — the house output format, so it is policy: injected, not owned here.
	// Nil is allowed only when no canonical output is needed (pure patch, no-op,
	// delete); a path that needs it with no Render fails loudly with a diagnostic.
	Render func(*unstructured.Unstructured) ([]byte, error)
	// ListMatch aligns sequences (default: by index).
	ListMatch ListMatchStrategy
	// Owns is a DORMANT mechanism seam, not a product feature. It reports whether a
	// field path is owned by the reverser; an absent field is deleted only when
	// owned. The product decision is API-first, whole-object truth: production
	// MUST leave this nil (own everything), so a field absent from the desired
	// projection is deleted from Git. See
	// docs/design/manifest/manifestedit-field-ownership-spike.md. Do not grow configuration
	// on top of this; it exists only to keep the deletion decision explicit and to
	// keep the merge testable.
	Owns func(path FieldPath) bool
}

// Comparison is the two-version comparison: an existing Git document against the
// desired object Git should contain.
type Comparison struct {
	// Git is required: a Comparison always describes an existing document. A nil
	// Git is not a valid comparison — creating a brand-new resource is a placement
	// decision owned upstream, not a content edit.
	Git *Document
	// Desired is the clean object Git should contain. Nil means "absent" and
	// models deletion as just another cell of the same comparison.
	Desired *unstructured.Unstructured
	// Options injects the renderer and the (future) list-match and ownership
	// strategies.
	Options EditOptions
}

// DecisionAction is the intent Decide states before any merge runs.
type DecisionAction string

const (
	// ActionNoChange means Git already matches the desired projection.
	ActionNoChange DecisionAction = "no-change"
	// ActionPatch means a field-level in-place edit is expected.
	ActionPatch DecisionAction = "patch"
	// ActionReplace means the document must be re-rendered canonically.
	ActionReplace DecisionAction = "replace"
	// ActionDelete means the document should be removed.
	ActionDelete DecisionAction = "delete"
	// ActionSkip means the document is left untouched, with a diagnostic.
	ActionSkip DecisionAction = "skip"
)

// SnapshotRef is the identity and content fingerprint that Decide observed.
// Apply re-parses the document and validates against this, refusing if the file
// drifted in between.
type SnapshotRef struct {
	// Identity is the observed manifest identity of the target document.
	Identity Identity
	// DocumentIndex is the observed target document index.
	DocumentIndex int
	// BodyHash fingerprints the target document body only — sibling documents can
	// change without invalidating the target edit.
	BodyHash string
}

// Decision is the result of the pure preflight. It states an intent; the merge
// happens only in Apply, whose EditResult.Mode is authoritative.
type Decision struct {
	Action   DecisionAction
	Reason   string
	Snapshot SnapshotRef
	// level is the diagnostic severity to surface for a skip; Apply emits it.
	level DiagnosticLevel
}

// Decide is a pure preflight: it inspects and compares, never mutating Git. It
// runs only cheap, non-mutating checks — parseable? disallowed construct?
// encrypted? non-mapping root? object-level equality — and never runs the
// structural merge, so a decision can never silently change Git.
func Decide(c Comparison) Decision {
	if c.Git == nil {
		return Decision{Action: ActionSkip, level: DiagError,
			Reason: "comparison requires an existing Git document"}
	}

	docs := splitDocuments(string(c.Git.Content))
	idx := c.Git.DocumentIndex
	if idx < 0 || idx >= len(docs) {
		return Decision{Action: ActionSkip, level: DiagError, Reason: "document index out of range"}
	}
	target := docs[idx].body
	snap := SnapshotRef{DocumentIndex: idx, BodyHash: hashBody(target)}

	// Deletion is content-agnostic: it never decrypts or merges, so an encrypted,
	// disallowed-construct, or duplicate-loser document can always be pruned. It
	// is decided before any content check.
	if c.Desired == nil {
		if root, empty, err := decodeDoc(target); err == nil && !empty {
			snap.Identity, _ = identityFromNode(root)
		}
		return Decision{Action: ActionDelete, Reason: "desired absent: delete the document", Snapshot: snap}
	}

	return decideContentEdit(c, target, snap)
}

// decideContentEdit runs the cheap, non-mutating checks for a content edit
// (Patch / Replace / NoChange) plus the refusals that apply only to content
// edits. It never runs the structural merge, so a decision can never change Git.
func decideContentEdit(c Comparison, target string, snap SnapshotRef) Decision {
	root, empty, err := decodeDoc(target)
	if err != nil {
		return Decision{Action: ActionSkip, level: DiagWarning, Reason: "invalid YAML", Snapshot: snap}
	}
	if empty {
		return Decision{Action: ActionSkip, level: DiagWarning,
			Reason: "empty document, nothing to patch", Snapshot: snap}
	}
	if reason, bad := hasDisallowed(root); bad {
		return Decision{Action: ActionSkip, level: DiagWarning,
			Reason: "ignored: " + reason + " is not editable", Snapshot: snap}
	}
	// Encrypted documents are authoritative but never patched in place: an
	// in-place merge would drop the sops metadata and write the secret back in
	// cleartext. Route them to the re-encrypt writer instead.
	if nodeMapGet(root, "sops") != nil {
		return Decision{Action: ActionSkip, level: DiagWarning,
			Reason:   "encrypted document: in-place patch is unsafe, use the re-encrypt writer path",
			Snapshot: snap}
	}
	snap.Identity, _ = identityFromNode(root)
	if root.Kind != yaml.MappingNode {
		return Decision{Action: ActionReplace,
			Reason: "non-mapping root cannot be patched field-by-field", Snapshot: snap}
	}

	// No-op vs change: compare the raw Git document to the desired projection.
	// Equal means a true no-op (preserve bytes); different means a patch.
	var rawObj map[string]interface{}
	if err := yaml.Unmarshal([]byte(target), &rawObj); err == nil {
		if reflect.DeepEqual(normalizeJSON(rawObj), normalizeJSON(c.Desired.Object)) {
			return Decision{Action: ActionNoChange, Reason: "Git already matches desired", Snapshot: snap}
		}
	}
	return Decision{Action: ActionPatch, Reason: "Git differs from desired", Snapshot: snap}
}

// Apply is authoritative: it re-parses c.Git, validates the snapshot, performs
// the edit, and returns what actually happened. There is no separate file
// argument — c.Git is the single source of truth for the bytes. The returned
// EditResult.Mode is the truth about what happened, not the Decision: a Patch
// intent may legitimately land on Replace if a node turns out ambiguous, or on a
// soft Skip if the snapshot drifted.
func Apply(c Comparison, d Decision) (EditResult, []Diagnostic) {
	if c.Git == nil {
		return EditResult{Mode: EditSkipped}, []Diagnostic{{Level: d.level, Message: d.Reason}}
	}
	content := c.Git.Content
	loc := Location{Path: c.Git.Path, DocumentIndex: c.Git.DocumentIndex}

	if d.Action == ActionSkip {
		level := d.level
		if level == "" {
			level = DiagWarning
		}
		return EditResult{Content: content, Mode: EditSkipped}, []Diagnostic{diag(level, loc, "%s", d.Reason)}
	}

	docs := splitDocuments(string(content))
	idx := c.Git.DocumentIndex
	if idx < 0 || idx >= len(docs) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagError, loc, "document index out of range")}
	}
	target := docs[idx].body

	if drift := validateSnapshot(d.Snapshot, idx, target, loc); drift != nil {
		return EditResult{Content: content, Mode: EditSkipped}, []Diagnostic{*drift}
	}

	return applyDecision(c, d, docs, idx, target, loc)
}

// validateSnapshot enforces the full Decide->Apply contract: the document Apply
// is about to edit must be the same one Decide compared — same index, same
// identity, same body. A mismatch returns a soft skip diagnostic so the next
// reconcile can re-decide cleanly against the changed file, rather than landing a
// stale edit on the wrong document. It returns nil when the snapshot still holds.
//
// The body hash is the strongest check (identical bytes imply identical identity),
// but the index and identity checks make the contract explicit and give a precise
// diagnostic when a Decision is carried and applied against a drifted file.
func validateSnapshot(snap SnapshotRef, idx int, target string, loc Location) *Diagnostic {
	if snap.DocumentIndex != idx {
		d := diag(DiagWarning, loc, "decision was for document %d, applying to %d, skipping",
			snap.DocumentIndex, idx)
		return &d
	}
	if snap.BodyHash != "" && hashBody(target) != snap.BodyHash {
		d := diag(DiagWarning, loc, "document changed since decision, skipping")
		return &d
	}
	if snap.Identity != (Identity{}) {
		if root, empty, err := decodeDoc(target); err == nil && !empty {
			if id, ok := identityFromNode(root); ok && id != snap.Identity {
				d := diag(DiagWarning, loc, "document identity changed since decision, skipping")
				return &d
			}
		}
	}
	return nil
}

// applyDecision performs the edit named by the validated decision. Apply has
// already confirmed the document exists and the snapshot still matches.
func applyDecision(
	c Comparison,
	d Decision,
	docs []rawDoc,
	idx int,
	target string,
	loc Location,
) (EditResult, []Diagnostic) {
	switch d.Action {
	case ActionNoChange:
		return EditResult{Content: c.Git.Content, Mode: EditNoChange}, nil
	case ActionDelete:
		return applyDelete(docs, idx), nil
	case ActionReplace:
		return applyReplace(docs, idx, c, loc)
	case ActionPatch:
		return applyPatch(docs, idx, target, c, loc)
	case ActionSkip:
		// Handled in Apply, before snapshot validation; here for exhaustiveness.
		return EditResult{Content: c.Git.Content, Mode: EditSkipped}, nil
	default:
		return EditResult{Content: c.Git.Content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagError, loc, "unknown decision action %q", d.Action)}
	}
}

// applyDelete removes the target document, splicing siblings verbatim. Removing
// the only document yields empty content so the caller can delete the file.
func applyDelete(docs []rawDoc, idx int) EditResult {
	if len(docs) == 1 {
		return EditResult{Content: nil, Mode: EditDeleted}
	}
	docs = append(docs[:idx], docs[idx+1:]...)
	// Drop the leading separator so a deleted first document does not leave the
	// file starting with "---".
	if idx == 0 {
		docs[0].sep = ""
	}
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditDeleted}
}

// applyPatch merges the desired object onto a fresh parse of the target, falling
// back to a whole-document replace if the merge turns out ambiguous.
func applyPatch(docs []rawDoc, idx int, target string, c Comparison, loc Location) (EditResult, []Diagnostic) {
	root, _, err := decodeDoc(target)
	if err != nil || root == nil || root.Kind != yaml.MappingNode {
		return applyReplace(docs, idx, c, loc)
	}

	changed, ok := mergeMapping(mergeCtx{owns: c.Options.Owns, list: c.Options.ListMatch}, nil, root, c.Desired.Object)
	if !ok {
		return applyReplace(docs, idx, c, loc)
	}
	if !changed {
		return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditNoChange}, nil
	}

	encoded, err := encodeNode(root)
	if err != nil {
		return applyReplace(docs, idx, c, loc)
	}

	docs[idx].body = reskinDocument(target, string(encoded))
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// applyReplace re-renders the target document canonically using the injected
// renderer. With no renderer it fails loudly: a missing wiring must not mask
// itself as plausible YAML.
func applyReplace(docs []rawDoc, idx int, c Comparison, loc Location) (EditResult, []Diagnostic) {
	if c.Options.Render == nil {
		return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditSkipped},
			[]Diagnostic{diag(DiagError, loc, "canonical output required but no Render injected")}
	}
	rendered, err := c.Options.Render(c.Desired)
	if err != nil {
		return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditSkipped},
			[]Diagnostic{diag(DiagError, loc, "cannot render document: %v", err)}
	}
	docs[idx].body = string(rendered)
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditWholeReplace},
		[]Diagnostic{diag(DiagWarning, loc, "field-level preservation not possible, replaced whole document")}
}

// hashBody fingerprints a document body for snapshot validation.
func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
