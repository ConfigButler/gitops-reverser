// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Field patches are the editing primitive behind subresource audit resolution
// (docs/design/manifest/version2/scale-subresource-audit-rehydration.md). A
// mutating subresource such as deployments/scale does not carry a full parent
// object, only a bounded set of changed field paths ("spec.replicas: 3"). Rather
// than hydrate the parent, carry those assignments to Git and set exactly them on
// the already committed manifest, leaving every other byte untouched.
//
// This is the mirror image of the package's whole-object merge. The merge owns
// every field, so a field absent from the desired projection is deleted (the
// production API-first truth). A field patch owns ONLY its assigned paths, so the
// same merge sets the assigned fields and leaves everything else — including
// fields it never mentioned — exactly as Git holds them. The two helpers here are
// the entire surface: PartialDesired builds the partial Comparison.Desired, and
// OwnsAssignedPaths builds the per-patch Comparison.Options.Owns. Nothing else in
// the Decide/Apply path changes, so a field patch inherits the snapshot-drift
// guard, the encrypted-document refusal, and the formatting preservation for free.

// FieldAssignment is one (path, value) assignment of a field patch: set the node
// at Path to Value, leaving every other field in the document untouched. Path is
// from the document root, e.g. {"spec","replicas"}. Value is a JSON-native
// unstructured value (string, int64, float64, bool, nil, map[string]interface{},
// []interface{}) — exactly what decoding an audit body as unstructured yields.
type FieldAssignment struct {
	Path  []string
	Value any
}

// PartialDesired builds the Comparison.Desired for a field patch: an object
// carrying only the parent identity plus each assignment's value at its path.
// Identity is included so the whole-object merge does not see apiVersion/kind/
// metadata as "absent from desired" — those are not owned by the patch, so they
// would be left regardless, but carrying them keeps Decide's identity snapshot
// meaningful and the no-op comparison honest.
//
// Assignments must have non-empty, disjoint paths; an empty path or a path that
// descends through a value an earlier assignment set as a scalar is a programming
// error and returns one.
func PartialDesired(id Identity, assignments []FieldAssignment) (*unstructured.Unstructured, error) {
	if id.APIVersion == "" || id.Kind == "" {
		return nil, fmt.Errorf("partial desired requires APIVersion and Kind, got %+v", id)
	}
	obj := map[string]interface{}{
		"apiVersion": id.APIVersion,
		"kind":       id.Kind,
	}
	if meta := identityMetadata(id); len(meta) > 0 {
		obj["metadata"] = meta
	}
	for _, a := range assignments {
		if len(a.Path) == 0 {
			return nil, errors.New("field assignment has an empty path")
		}
		if err := setPath(obj, a.Path, a.Value); err != nil {
			return nil, err
		}
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// identityMetadata returns the metadata sub-map for an identity, omitting an empty
// namespace so a cluster-scoped object does not gain a spurious namespace key.
func identityMetadata(id Identity) map[string]interface{} {
	meta := map[string]interface{}{}
	if id.Name != "" {
		meta["name"] = id.Name
	}
	if id.Namespace != "" {
		meta["namespace"] = id.Namespace
	}
	return meta
}

// setPath sets value at a nested key path, creating intermediate maps as needed.
// It does not deep-copy or validate the value (unlike unstructured.SetNestedField,
// which rejects non-JSON-native scalars such as a plain int), so a caller can pass
// values straight through. Descending through a non-map intermediate means two
// assignments overlap, which a field patch forbids: it returns an error rather
// than clobbering the earlier assignment.
func setPath(root map[string]interface{}, path []string, value any) error {
	cur := root
	for i, key := range path {
		if i == len(path)-1 {
			cur[key] = value
			return nil
		}
		switch next := cur[key].(type) {
		case nil:
			child := map[string]interface{}{}
			cur[key] = child
			cur = child
		case map[string]interface{}:
			cur = next
		default:
			return fmt.Errorf("assignment path %v overlaps an earlier assignment at %q", path, key)
		}
	}
	return nil
}

// OwnsAssignedPaths returns the Comparison.Options.Owns predicate for a field
// patch: it owns exactly the assigned paths and their descendants. Because the
// merge consults ownership only to decide whether a Git field absent from desired
// should be deleted, owning just the assigned subtrees means the patch can replace
// an assigned field (including pruning sub-keys of a map-valued assignment) while
// never deleting any field outside an assignment. A scalar assignment has no
// descendants, so it cannot delete anything at all — it only overwrites its leaf.
//
// This predicate is always derived from the assignments, never caller-supplied:
// a field patch's ownership is a property of the patch, not a tunable. That is the
// one sanctioned non-nil Owns (the field-ownership spike forbids ownership as
// configuration); here it is scoped to a single edit and not exposed as a knob.
func OwnsAssignedPaths(assignments []FieldAssignment) func(FieldPath) bool {
	owned := make([][]string, len(assignments))
	for i, a := range assignments {
		owned[i] = append([]string(nil), a.Path...)
	}
	return func(path FieldPath) bool {
		for _, prefix := range owned {
			if pathHasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
}

// pathHasPrefix reports whether path is prefix or a descendant of it.
func pathHasPrefix(path FieldPath, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

// PatchFields applies a field patch to one document inside a file: it sets the
// assigned paths on the document for id and leaves every other field and document
// byte-for-byte identical. It is the field-patch analog of PatchDocument, wiring
// PartialDesired and OwnsAssignedPaths through the same Decide + Apply path, so it
// inherits the snapshot guard, the encrypted-document refusal, and formatting
// preservation. opts.Owns is always overwritten with the patch's own ownership;
// the caller injects only opts.Render (for the replace fallback) and opts.ListMatch.
//
// A skip (the document is missing, encrypted, or non-editable) surfaces as an
// EditSkipped result with a diagnostic, exactly like PatchDocument — the caller
// decides whether that is "no parent in Git, drop" or "unsafe, drop".
func PatchFields(
	content []byte,
	documentIndex int,
	id Identity,
	assignments []FieldAssignment,
	opts EditOptions,
) (EditResult, []Diagnostic) {
	desired, err := PartialDesired(id, assignments)
	if err != nil {
		return EditResult{Mode: EditSkipped}, []Diagnostic{{Level: DiagError, Message: err.Error()}}
	}
	opts.Owns = OwnsAssignedPaths(assignments)
	return PatchDocument(content, documentIndex, desired, opts)
}
