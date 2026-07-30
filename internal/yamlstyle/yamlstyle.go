// SPDX-License-Identifier: Apache-2.0

/*
Package yamlstyle owns the one YAML serialization style this product writes to Git.

It exists because the product's entire output is a Git diff, and two encoders producing
two styles makes that diff unreadable. A resource created by the writer used to render its
sequences at the parent key's indentation (JSON→YAML, via sigs.k8s.io/yaml), while the
same resource EDITED IN PLACE re-encoded them two columns deeper (gopkg.in/yaml.v3). Both
are valid YAML and both round-trip, so nothing failed — but the first update after a create
rewrote every list line in the file to carry one changed field, which defeats the point of
mirroring into Git at all.

This package is deliberately tiny and deliberately dependency-free: it holds the style, not
the projection. What is clean enough to write is internal/sanitize's decision; the node tree
of an edited document is internal/git/manifestedit's. Both encode through here.

# Why sigs.k8s.io/yaml is still in the tree

Because it is not a second renderer of manifests, and consolidating it into this one would be
a bug rather than a cleanup. Every remaining call to it either PARSES (a decoder imposes no
style) or serializes a Go struct whose fields carry only JSON tags: the analyzer's report,
whose contract is that its YAML and its JSON name every field identically, and the in-memory
kustomization copies handed to kustomize during a render. Encoding those through yaml.v3
would emit Go field names instead of the API's, and none of those bytes are ever committed.

One style authority for what reaches Git; the JSON-tag round-trip stays where it belongs.
TestGitWritePathHasNoSecondEncoder holds the line.
*/
package yamlstyle

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Indent is the indentation every document this product writes uses. It matches common
// manifest style rather than yaml.v3's 4-space default.
//
// Two columns is also the only choice available: yaml.v3 always indents a sequence under
// its mapping key, so the style a create used to emit (sequence dashes at the key's own
// column, which is what yaml.v2 and therefore sigs.k8s.io/yaml produce) cannot be
// expressed by the encoder that edits documents in place. Aligning on this direction was
// forced by that, and it is the better direction anyway: it is what every file in a
// repository the operator has updated once already looks like.
const Indent = 2

// Encode serializes any value in the house style.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeTo(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NodeFor builds the node for one value, so a caller assembling a document in a chosen key
// order encodes each value with the same encoder that will write the result.
func NodeFor(v any) (*yaml.Node, error) {
	node := &yaml.Node{}
	if err := encodeInto(node, v); err != nil {
		return nil, err
	}
	return node, nil
}

// encodeTo writes v to buf in the house style.
func encodeTo(buf *bytes.Buffer, v any) error {
	return guard(func() error {
		enc := yaml.NewEncoder(buf)
		enc.SetIndent(Indent)
		if err := enc.Encode(v); err != nil {
			return err
		}
		return enc.Close()
	})
}

// encodeInto fills node from v, with the same panic containment as encodeTo.
func encodeInto(node *yaml.Node, v any) error {
	return guard(func() error { return node.Encode(v) })
}

// guard runs one encode, turning a yaml.v3 encoder panic into an error.
//
// This is not defensive boilerplate, it is the reason the seam is worth having. yaml.v3 raises
// "cannot marshal type: X" by panicking with a BARE STRING, which its own handleErr re-panics
// because it is not the library's error type — so an unencodable value crashes the caller's
// goroutine instead of failing its write. The JSON-based encoder this package replaced on the
// write path returned an error there, and a branch worker that returns an error retries and
// reports, while one that panics takes the process down.
//
// Nothing the API server sends can reach it (a decoded object holds only JSON types), so this
// is about the blast radius of a caller's mistake, not about production data.
func guard(fn func() error) error {
	var failure error
	func() {
		defer func() {
			if r := recover(); r != nil {
				failure = fmt.Errorf("yaml encode failed: %v", r)
			}
		}()
		failure = fn()
	}()
	return failure
}
