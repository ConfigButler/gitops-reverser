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
	"bytes"
	"reflect"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// PatchDocument updates one document inside a file to match the desired object,
// touching only what changed and leaving every other document byte-for-byte
// identical. The desired object is sanitized first, so Git converges to the
// clean GitOps projection (a stray resourceVersion in Git is removed, not kept).
func PatchDocument(
	content []byte,
	documentIndex int,
	desired *unstructured.Unstructured,
) (EditResult, []Diagnostic) {
	docs := splitDocuments(string(content))

	if documentIndex < 0 || documentIndex >= len(docs) {
		return EditResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{{Level: DiagError, DocumentIndex: documentIndex, Message: "document index out of range"}}
	}

	loc := Location{DocumentIndex: documentIndex}
	target := docs[documentIndex].body

	root, empty, err := decodeDoc(target)
	if err != nil {
		return skip(content, loc, "invalid YAML: %v", err)
	}
	if empty {
		return skip(content, loc, "empty document, nothing to patch")
	}
	if reason, bad := hasDisallowed(root); bad {
		return skip(content, loc, "ignored: %s is not editable", reason)
	}
	if root.Kind != yaml.MappingNode {
		return wholeReplace(docs, documentIndex, desired, loc)
	}

	desiredClean := sanitize.Sanitize(desired).Object

	// No-op vs cleaning: compare the raw Git document to the clean projection.
	// Equal means a true no-op (preserve bytes); different means a change or a
	// dirty field to clean out.
	var rawObj map[string]interface{}
	if err := yaml.Unmarshal([]byte(target), &rawObj); err == nil {
		if reflect.DeepEqual(normalizeJSON(rawObj), normalizeJSON(desiredClean)) {
			return EditResult{Content: content, Mode: EditNoChange}, nil
		}
	}

	changed, ok := mergeMapping(root, desiredClean)
	if !ok {
		return wholeReplace(docs, documentIndex, desired, loc)
	}
	if !changed {
		return EditResult{Content: content, Mode: EditNoChange}, nil
	}

	encoded, err := encodeNode(root)
	if err != nil {
		return wholeReplace(docs, documentIndex, desired, loc)
	}

	docs[documentIndex].body = string(encoded)
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditPatched}, nil
}

// wholeReplace re-renders the target document canonically as a fallback.
func wholeReplace(
	docs []rawDoc,
	documentIndex int,
	desired *unstructured.Unstructured,
	loc Location,
) (EditResult, []Diagnostic) {
	rendered, err := sanitize.MarshalToOrderedYAML(sanitize.Sanitize(desired))
	if err != nil {
		return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditSkipped},
			[]Diagnostic{diag(DiagError, loc, "cannot render document: %v", err)}
	}
	docs[documentIndex].body = string(rendered)
	return EditResult{Content: []byte(joinDocuments(docs)), Mode: EditWholeReplace},
		[]Diagnostic{diag(DiagWarning, loc, "field-level preservation not possible, replaced whole document")}
}

// yamlIndent is the indentation used when re-encoding an edited document. It
// matches common manifest style rather than yaml.v3's 4-space default.
const yamlIndent = 2

// encodeNode serializes a node with two-space indentation.
func encodeNode(node *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(yamlIndent)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// skip returns an unchanged result with a diagnostic.
func skip(content []byte, loc Location, format string, args ...any) (EditResult, []Diagnostic) {
	return EditResult{Content: content, Mode: EditSkipped}, []Diagnostic{diag(DiagWarning, loc, format, args...)}
}
