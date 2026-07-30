// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/yamlstyle"
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

// encodeNode serializes a node in the house style.
//
// The style lives in [yamlstyle] rather than here because the writer's create path encodes
// through the same package: an UPDATE that re-indents what a CREATE wrote makes the mirror's
// git diff unreadable, and one shared constant is what stops the two drifting again.
func encodeNode(node *yaml.Node) ([]byte, error) {
	return yamlstyle.Encode(node)
}
