// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Kustomization override sections the editor accepts. The editor is the
// mechanism half of the F1 images/replicas edit-through
// (docs/design/gitops-api/f1-images-replicas-edit-through.md): it updates the
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

	if DocumentCount(content) != 1 {
		return skip("kustomization %s is not a single-document file", path)
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
		return skip("kustomization %s holds no document", path)
	}
	target := docs[idx].body

	root, empty, err := decodeDoc(target)
	if err != nil || empty || root.Kind != yaml.MappingNode {
		return skip("kustomization %s is not an editable mapping document", path)
	}
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
