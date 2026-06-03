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

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PatchDocument updates one document inside a file to match the desired object,
// touching only what changed and leaving every other document byte-for-byte
// identical. It is a thin wrapper over Decide + Apply.
//
// The desired object must already be the clean Git projection: this package is
// mechanism, not policy, so it never sanitizes internally. The caller passes the
// projected object and injects the canonical renderer (opts.Render), used for the
// whole-document replace fallback.
func PatchDocument(
	content []byte,
	documentIndex int,
	desired *unstructured.Unstructured,
	opts EditOptions,
) (EditResult, []Diagnostic) {
	git, _ := NewDocument(content, documentIndex)
	c := Comparison{Git: git, Desired: desired, Options: opts}
	return Apply(c, Decide(c))
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
