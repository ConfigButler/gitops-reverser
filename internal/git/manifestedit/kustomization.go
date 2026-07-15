// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kustomization override sections the editor accepts. The editor is the
// mechanism half of the images/replicas edit-through
// (docs/design/support-boundary/finished/images-and-replicas-edit-through.md): it updates the
// scalar value of a field that ALREADY EXISTS on an entry that ALREADY EXISTS,
// and nothing else — it never adds or removes entries, keys, or files.
const (
	KustomizationSectionImages   = "images"
	KustomizationSectionReplicas = "replicas"
)

// KustomizationEdit sets one existing scalar field on one existing entry of a
// kustomization.yaml override section. EntryIndex pins the exact entry (two
// entries may share a name and kustomize applies them in order); EntryName is
// re-verified against it so a drifted file is skipped, never mis-edited.
type KustomizationEdit struct {
	// Section is KustomizationSectionImages or KustomizationSectionReplicas.
	Section string
	// EntryIndex is the entry's position within the section sequence.
	EntryIndex int
	// EntryName is the entry's name: value, verified against EntryIndex.
	EntryName string
	// Field is the scalar key to update: newName/newTag/digest, or count.
	Field string
	// Value is the new scalar value; for count it is a decimal integer.
	Value string
}

// locateKustomizationDocument finds and decodes the sole document in a
// single-document kustomization.yaml, ready for an editor to mutate root in place.
// ok is false when the file cannot be safely edited — a multi-document file,
// unparseable YAML, an empty document, or a non-mapping document — in which case
// reason names why, for the caller's skip diagnostic.
func locateKustomizationDocument(
	path string,
	content []byte,
) ([]rawDoc, int, *yaml.Node, string, bool) {
	if DocumentCount(content) != 1 {
		return nil, -1, nil, fmt.Sprintf("kustomization %s is not a single-document file", path), false
	}
	docs := splitDocuments(string(content))
	idx := -1
	for i, d := range docs {
		if !isBlankLine(d.body) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, -1, nil, fmt.Sprintf("kustomization %s holds no document", path), false
	}
	decoded, empty, err := decodeDoc(docs[idx].body)
	if err != nil || empty || decoded.Kind != yaml.MappingNode {
		return nil, -1, nil, fmt.Sprintf("kustomization %s is not an editable mapping document", path), false
	}
	return docs, idx, decoded, "", true
}

// PatchKustomization applies the edits to a single-document kustomization file,
// preserving comments, key order, and framing exactly as the manifest patch path
// does. All-or-nothing: any edit that cannot be applied (multi-document file,
// unparseable YAML, missing section/entry/field, a name mismatch at the pinned
// index) skips the whole call and returns the original content with a
// diagnostic — the writer must never guess inside a build directive.
func PatchKustomization(path string, content []byte, edits []KustomizationEdit) (EditResult, []Diagnostic) {
	skip := func(format string, args ...interface{}) (EditResult, []Diagnostic) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagWarning, Location{Path: path}, format, args...)}
	}

	docs, idx, root, reason, ok := locateKustomizationDocument(path, content)
	if !ok {
		return skip("%s", reason)
	}
	target := docs[idx].body
	for _, e := range edits {
		if err := applyKustomizationEdit(root, e); err != nil {
			return skip("kustomization %s: %v", path, err)
		}
	}

	encoded, err := encodeNode(root)
	if err != nil {
		return skip("kustomization %s: re-encode failed: %v", path, err)
	}
	body := reskinDocument(target, string(encoded))
	if body == target {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}
	docs[idx].body = body
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// applyKustomizationEdit updates one scalar in place, or reports why it cannot.
func applyKustomizationEdit(root *yaml.Node, e KustomizationEdit) error {
	section := nodeMapGet(root, e.Section)
	if section == nil || section.Kind != yaml.SequenceNode {
		return fmt.Errorf("no %s sequence", e.Section)
	}
	if e.EntryIndex < 0 || e.EntryIndex >= len(section.Content) {
		return fmt.Errorf("%s entry %d out of range", e.Section, e.EntryIndex)
	}
	item := section.Content[e.EntryIndex]
	if item.Kind != yaml.MappingNode {
		return fmt.Errorf("%s entry %d is not a mapping", e.Section, e.EntryIndex)
	}
	name := nodeMapGet(item, "name")
	if name == nil || name.Value != e.EntryName {
		return fmt.Errorf("%s entry %d is not named %q", e.Section, e.EntryIndex, e.EntryName)
	}
	field := nodeMapGet(item, e.Field)
	if field == nil || field.Kind != yaml.ScalarNode {
		return fmt.Errorf("%s entry %q has no scalar field %s", e.Section, e.EntryName, e.Field)
	}
	setOverrideScalar(field, e)
	return nil
}

// AppendKustomizationResource adds one entry to an existing kustomization.yaml's
// resources: sequence — the mechanism half of the "add to the right kustomize
// file" (docs/spec/gittarget-new-file-placement-rules.md): a
// new sibling file placed inside a kustomize-governed directory must also be named
// in that directory's resources: list, or kustomize never renders it.
//
// It is idempotent: if entry already appears in the sequence, the call is a no-op
// (EditNoChange), never a duplicate append. All-or-nothing like PatchKustomization:
// a multi-document file, unparseable YAML, or a document with no existing
// resources: sequence skips the whole call with a diagnostic — the writer never
// invents a resources: key that is not already there, mirroring the edit-through's "never
// creates a kustomization file" boundary one level down (never creates a
// resources: section either).
func AppendKustomizationResource(path string, content []byte, entry string) (EditResult, []Diagnostic) {
	skip := func(format string, args ...interface{}) (EditResult, []Diagnostic) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagWarning, Location{Path: path}, format, args...)}
	}

	docs, idx, root, reason, ok := locateKustomizationDocument(path, content)
	if !ok {
		return skip("%s", reason)
	}
	target := docs[idx].body

	section := nodeMapGet(root, "resources")
	if section == nil || section.Kind != yaml.SequenceNode {
		return skip("kustomization %s has no resources sequence", path)
	}
	for _, item := range section.Content {
		if item.Kind == yaml.ScalarNode && strings.TrimSpace(item.Value) == strings.TrimSpace(entry) {
			return EditResult{Content: content, Mode: EditNoChange}, nil
		}
	}
	section.Content = append(section.Content, &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: entry,
	})

	encoded, err := encodeNode(root)
	if err != nil {
		return skip("kustomization %s: re-encode failed: %v", path, err)
	}
	body := reskinDocument(target, string(encoded))
	if body == target {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}
	docs[idx].body = body
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// AppendKustomizationOverride AUTHORS a new images:/replicas: override entry in a
// kustomization.yaml — the mechanism half of "edit a specific environment and the override is
// added for you" (docs/design/support-boundary/render-root-scoping.md §4). Unlike
// PatchKustomization, which only updates a scalar on an entry that already exists, this creates
// the entry (and the section sequence, if the kustomization has none yet) so an overlay can
// override a value its base supplies WITHOUT touching the read-only base.
//
// It writes exactly one field beside name:, matching how the writer inverts one changed image
// component or replica count at a time:
//
//	images:   { name: <image name>, newName|newTag|digest: <value> }
//	replicas: { name: <resource name>, count: <value> }
//
// It is idempotent and never duplicates: an entry named name that already sets field to value is
// a no-op (EditNoChange), so a resync re-observing the same live state does not append a second
// entry. All-or-nothing like its siblings: a multi-document file, unparseable YAML, or a
// non-mapping document skips the whole call with a diagnostic. Every proposal it produces is put
// to kustomize by the re-render oracle before it can become a commit, so an entry that would
// over-reach (an images: name shared by another object the overlay did not mean to move) is
// refused there, not written.
func AppendKustomizationOverride(
	path string, content []byte, section, name, field, value string,
) (EditResult, []Diagnostic) {
	skip := func(format string, args ...interface{}) (EditResult, []Diagnostic) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagWarning, Location{Path: path}, format, args...)}
	}
	if section != KustomizationSectionImages && section != KustomizationSectionReplicas {
		return skip("kustomization %s: unknown override section %q", path, section)
	}

	docs, idx, root, reason, ok := locateKustomizationDocument(path, content)
	if !ok {
		return skip("%s", reason)
	}
	target := docs[idx].body

	seq := nodeMapGet(root, section)
	if seq != nil && seq.Kind != yaml.SequenceNode {
		return skip("kustomization %s: %s is not a sequence", path, section)
	}
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: section},
			seq)
	}
	if overrideEntryPresent(seq, name, field, value) {
		return EditResult{Content: content, Mode: EditNoChange}, nil // already authored
	}
	seq.Content = append(seq.Content, overrideEntryNode(section, name, field, value))

	encoded, err := encodeNode(root)
	if err != nil {
		return skip("kustomization %s: re-encode failed: %v", path, err)
	}
	body := reskinDocument(target, string(encoded))
	if body == target {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}
	docs[idx].body = body
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// overrideEntryPresent reports whether the sequence already holds an entry named name that sets
// field to value — the idempotency check that keeps a resync from appending a duplicate.
func overrideEntryPresent(seq *yaml.Node, name, field, value string) bool {
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if n := nodeMapGet(item, "name"); n == nil || n.Value != name {
			continue
		}
		if f := nodeMapGet(item, field); f != nil && f.Value == value {
			return true
		}
	}
	return false
}

// overrideEntryNode builds the `{ name: <name>, <field>: <value> }` mapping for a new override
// entry. count is integer-typed (kustomize's replicas count is an int); image components are
// string-typed and left plain so the encoder can quote a "1.29"-style tag safely.
func overrideEntryNode(section, name, field, value string) *yaml.Node {
	valueNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if section == KustomizationSectionReplicas && field == "count" {
		valueNode.Tag = "!!int"
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: field},
		valueNode,
	}}
}

// RemoveKustomizationResource drops one entry from an existing kustomization.yaml's
// resources: sequence. It is AppendKustomizationResource's counterpart, and it exists for
// exactly the reason that one does, read backwards: a file named in resources: that no
// longer exists is a file kustomize refuses to build over —
//
//	accumulating resources ... '/scan/apps/api.yaml' doesn't exist
//
// so deleting a managed document without removing its entry leaves a repository no GitOps
// controller can deploy. Deleting the manifest is only half the delete.
//
// It is idempotent (an entry that is not there is EditNoChange) and all-or-nothing in the
// same way as its sibling: a multi-document file, unparseable YAML, or a document with no
// resources: sequence skips the whole call with a diagnostic rather than inventing structure.
// An emptied sequence is left as an empty sequence — removing the key is not this function's
// call to make.
func RemoveKustomizationResource(path string, content []byte, entry string) (EditResult, []Diagnostic) {
	skip := func(format string, args ...interface{}) (EditResult, []Diagnostic) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{diag(DiagWarning, Location{Path: path}, format, args...)}
	}

	docs, idx, root, reason, ok := locateKustomizationDocument(path, content)
	if !ok {
		return skip("%s", reason)
	}
	target := docs[idx].body

	section := nodeMapGet(root, "resources")
	if section == nil || section.Kind != yaml.SequenceNode {
		return skip("kustomization %s has no resources sequence", path)
	}

	kept := make([]*yaml.Node, 0, len(section.Content))
	removed := false
	for _, item := range section.Content {
		if item.Kind == yaml.ScalarNode && strings.TrimSpace(item.Value) == strings.TrimSpace(entry) {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if !removed {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}
	section.Content = kept

	encoded, err := encodeNode(root)
	if err != nil {
		return skip("kustomization %s: re-encode failed: %v", path, err)
	}
	body := reskinDocument(target, string(encoded))
	if body == target {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}
	docs[idx].body = body
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// setOverrideScalar writes the new value, keeping the value string-typed for the
// image fields (the encoder quotes "1.29"-style values when the tag is !!str) and
// integer-typed for count. An existing quoting style is kept; other styles reset
// to plain so the encoder can choose safely.
func setOverrideScalar(n *yaml.Node, e KustomizationEdit) {
	n.Value = e.Value
	if e.Field == "count" {
		n.Tag = "!!int"
		n.Style = 0
		return
	}
	n.Tag = "!!str"
	if n.Style != yaml.SingleQuotedStyle && n.Style != yaml.DoubleQuotedStyle {
		n.Style = 0
	}
}
