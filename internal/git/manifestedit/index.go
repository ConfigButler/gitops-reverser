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
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// IndexFile builds an inventory from a single file's content.
func IndexFile(path string, content []byte) (Inventory, []Diagnostic) {
	return IndexFiles([]FileContent{{Path: path, Content: content}})
}

// IndexFiles builds an inventory from several files. Scan order is deterministic
// (lexicographic path, then document index) so duplicate resolution is stable.
func IndexFiles(files []FileContent) (Inventory, []Diagnostic) {
	sorted := append([]FileContent(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var records []DocumentRecord
	var diags []Diagnostic

	for _, f := range sorted {
		recs, ds := indexOneFile(f)
		records = append(records, recs...)
		diags = append(diags, ds...)
	}

	inv, dupDiags := resolveDuplicates(records)
	diags = append(diags, dupDiags...)
	return inv, diags
}

// indexOneFile indexes the documents of one file.
func indexOneFile(f FileContent) ([]DocumentRecord, []Diagnostic) {
	encrypted := isSOPSFile(f.Path)
	docs := splitDocuments(string(f.Content))

	var records []DocumentRecord
	var diags []Diagnostic

	for i, doc := range docs {
		loc := Location{Path: f.Path, DocumentIndex: i}
		root, empty, err := decodeDoc(doc.body)
		if err != nil {
			diags = append(diags, diagR(DiagError, ReasonInvalidYAML, loc, "invalid YAML: %v", err))
			continue
		}
		if empty {
			diags = append(diags, diagR(DiagInfo, ReasonEmptyDocument, loc, "empty document, ignored"))
			continue
		}

		id, ok := identityFromNode(root)
		if !ok {
			diags = append(diags, diagR(DiagInfo, ReasonNotKRM, loc, "not a Kubernetes manifest, ignored"))
			continue
		}

		if reason, bad := hasDisallowed(root); bad {
			diags = append(diags, diagR(DiagWarning, ReasonNonEditable, loc, "ignored: %s is not editable", reason))
			records = append(records, DocumentRecord{Identity: id, Location: loc, Editable: false, Reason: reason})
			continue
		}

		if encrypted {
			if nodeMapGet(root, "sops") == nil {
				diags = append(
					diags,
					diagR(DiagError, ReasonMissingSopsKey, loc, "SOPS file without a sops key, invalid"),
				)
				continue
			}
			records = append(records, DocumentRecord{Identity: id, Location: loc, Editable: true, Encrypted: true})
			continue
		}

		records = append(records, DocumentRecord{Identity: id, Location: loc, Editable: true})
	}

	return records, diags
}

// resolveDuplicates applies first-occurrence-wins: the first record for an
// identity keeps its location, later copies become deletable duplicates.
func resolveDuplicates(records []DocumentRecord) (Inventory, []Diagnostic) {
	inv := Inventory{byIdentity: make(map[Identity]Location)}
	var diags []Diagnostic

	for _, rec := range records {
		inv.Records = append(inv.Records, rec)
		if !rec.Editable {
			continue
		}
		if winner, seen := inv.byIdentity[rec.Identity]; seen {
			inv.duplicates = append(inv.duplicates, rec)
			diags = append(diags, Diagnostic{
				Level:         DiagWarning,
				Reason:        ReasonDuplicateIdentity,
				Path:          rec.Location.Path,
				DocumentIndex: rec.Location.DocumentIndex,
				Message: fmt.Sprintf("%s: keeping %s document %d, removing duplicate in %s document %d",
					identityString(rec.Identity), winner.Path, winner.DocumentIndex,
					rec.Location.Path, rec.Location.DocumentIndex),
			})
			continue
		}
		inv.byIdentity[rec.Identity] = rec.Location
	}

	return inv, diags
}

// decodeDoc parses a document body into its root node without expanding aliases,
// so a billion-laughs alias bomb cannot blow up here. It reports empty documents.
func decodeDoc(body string) (*yaml.Node, bool, error) {
	if strings.TrimSpace(stripComments(body)) == "" {
		return nil, true, nil
	}
	var docNode yaml.Node
	if err := yaml.Unmarshal([]byte(body), &docNode); err != nil {
		return nil, false, err
	}
	if docNode.Kind == 0 || len(docNode.Content) == 0 {
		return nil, true, nil
	}
	return docNode.Content[0], false, nil
}

// stripComments removes whole-line comments so a comment-only document reads as
// empty. It is only used for the empty-document check.
func stripComments(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// identityFromNode reads the manifest identity from a mapping root node.
func identityFromNode(root *yaml.Node) (Identity, bool) {
	if root == nil || root.Kind != yaml.MappingNode {
		return Identity{}, false
	}
	id := Identity{
		APIVersion: scalarOf(nodeMapGet(root, "apiVersion")),
		Kind:       scalarOf(nodeMapGet(root, "kind")),
	}
	md := nodeMapGet(root, "metadata")
	id.Name = scalarOf(nodeMapGet(md, "name"))
	id.Namespace = scalarOf(nodeMapGet(md, "namespace"))

	ok := id.APIVersion != "" && id.Kind != "" && id.Name != ""
	return id, ok
}

// hasDisallowed reports the first disallowed construct (anchor, alias, merge
// key, duplicate key, unusual tag) found in a node tree, walking without
// materializing aliases so an alias bomb cannot blow up here.
func hasDisallowed(n *yaml.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.Kind == yaml.AliasNode {
		return "alias", true
	}
	if n.Anchor != "" {
		return "anchor", true
	}
	if isUnusualTag(n.Tag) {
		return "unusual tag " + n.Tag, true
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return "merge key", true
			}
			if seen[key.Value] {
				return "duplicate key " + key.Value, true
			}
			seen[key.Value] = true
		}
	}
	for _, c := range n.Content {
		if reason, ok := hasDisallowed(c); ok {
			return reason, true
		}
	}
	return "", false
}

// isUnusualTag reports whether an explicit YAML tag is one the editor refuses to
// edit through: a local/custom tag (single "!") or binary. Core resolved tags
// such as !!str, !!int, !!bool and !!timestamp (from a plain date) are fine.
func isUnusualTag(tag string) bool {
	if tag == "" {
		return false
	}
	if strings.HasPrefix(tag, "!!") {
		return tag == "!!binary"
	}
	return strings.HasPrefix(tag, "!")
}

// nodeMapGet returns the value node for a key in a mapping node, or nil.
func nodeMapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarOf returns the value of a scalar node, or "".
func scalarOf(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// isSOPSFile reports whether a path is a SOPS-managed file by extension.
func isSOPSFile(path string) bool {
	return strings.HasSuffix(path, ".sops.yaml") || strings.HasSuffix(path, ".sops.yml")
}

// identityString renders an identity like "apps/v1/Deployment/default/app".
func identityString(id Identity) string {
	ns := id.Namespace
	if ns == "" {
		ns = "_cluster"
	}
	return fmt.Sprintf("%s/%s/%s/%s", id.APIVersion, id.Kind, ns, id.Name)
}

// diag is a small constructor for formatted diagnostics tied to a location.
func diag(level DiagnosticLevel, loc Location, format string, args ...any) Diagnostic {
	return Diagnostic{
		Level:         level,
		Path:          loc.Path,
		DocumentIndex: loc.DocumentIndex,
		Message:       fmt.Sprintf(format, args...),
	}
}

// diagR is diag with a structured reason code attached, so callers can classify
// the document without parsing the message text.
func diagR(level DiagnosticLevel, reason DiagReason, loc Location, format string, args ...any) Diagnostic {
	d := diag(level, loc, format, args...)
	d.Reason = reason
	return d
}
