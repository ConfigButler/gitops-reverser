// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"encoding/json"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
)

// The source form: what the SOURCE DOCUMENT should hold, given what the cluster holds.
//
// The writer mirrors a live object into the file that produced it. Under kustomize that file
// is not what the cluster runs — the build stands between them — and mirroring the live object
// straight back writes THE BUILD'S OWN OUTPUT into the build's INPUT. Measured, on features we
// accept today:
//
//	kustomization.yaml:  labels: [{pairs: {env: prod}}] · commonAnnotations: {owner: platform}
//	deployment.yaml gains, on the next reconcile:  labels: {env: prod}, annotations: {owner: platform}
//
// Nothing changed in the cluster and nothing changed in the render; the source file simply
// absorbed the overlay's metadata as if the author had typed it. Do it once and the file is
// wrong; remove the kustomization later and the drift is permanent. The same shape, with a
// patch instead of a label, silently rewrites a base with one environment's values — which is
// why this is the gate on tolerating patches at all.
//
// The rule needs no model of any transformer, and it is the whole of this file:
//
//	WHERE THE LIVE OBJECT AND THE RENDER AGREE, THE SOURCE KEEPS ITS BYTES.
//	WHERE THEY DISAGREE, THE USER CHANGED SOMETHING, AND THAT IS WHAT WE WRITE.
//
// Agreement means the build already produces exactly what the cluster runs, so whatever the
// source says at that field is — by construction — the thing that produced it. Keeping it is
// not a heuristic; changing it would be writing a value we did not get from the user.
//
// Disagreement is the user's edit. It is written through to the source, and if a transformer or
// a patch owns that field the render will not reproduce it — which the render precondition
// (VerifyBatchRenders) catches, and refuses. So the two halves compose: this file makes an
// in-sync folder a no-op, and the oracle adjudicates everything else.
//
// See docs/design/support-boundary/render-root-scoping.md §6 and render-attribution.md §5.

// node is one field of a document, and whether it is there at all. Absence is a value here:
// "the build injected this key and the source does not carry it" is the most common thing this
// file has to say, and it can only be said if absence travels with the node.
type node struct {
	value   interface{}
	present bool
}

// absent is the zero node: the field is not in this view of the document.
func absent() node { return node{} }

// SourceFormRefusedError is the projection giving up rather than guessing. The build and the
// live object BOTH changed one list, and the source's elements cannot be paired with the
// render's, so there is no way to say which of the source's bytes the user meant to keep.
//
// Refusing is the only safe answer. Guessing an alignment writes one element's fields into
// another, and index alignment is not a safe guess: kustomize's strategic merge PREPENDS a
// container a patch adds (measured — the render is [sidecar, app] over a source of [app]), so
// source[0] is not render[0] in the one case where it matters most.
type SourceFormRefusedError struct {
	// Field is the path of the list that could not be aligned, as a user would read it.
	Field string
}

func (e *SourceFormRefusedError) Error() string {
	return "cannot place the edit: the build and the live object both changed " + e.Field +
		", whose elements carry no unique name to pair the source with the render by, " +
		"so pairing them by position could place one element's edit onto another"
}

// sourceForm computes the document the source file should hold: src, carrying the user's
// changes, never the build's.
//
// src is the document as Git holds it, rendered is what kustomize renders THAT document to, and
// live is the sanitized live object. All three describe one object, and every value in the
// result comes from src or from live — never from rendered, which is an output and must not
// become an input.
//
// A result that is not a document at all — which would mean the live object is not a mapping —
// falls back to live: there is nothing to restore, and today's write-through is what the rest of
// the writer already expects.
func sourceForm(src, rendered, live map[string]interface{}) (map[string]interface{}, error) {
	out, err := restore(
		node{value: src, present: src != nil},
		node{value: rendered, present: rendered != nil},
		node{value: live, present: live != nil},
		"",
	)
	if err != nil {
		return nil, err
	}
	if !out.present {
		return live, nil
	}
	obj, isDocument := deepCopyValue(out.value).(map[string]interface{})
	if !isDocument {
		return live, nil
	}
	return obj, nil
}

// restore is the rule, applied to one field.
func restore(src, rendered, live node, field string) (node, error) {
	switch {
	case equalNode(live, rendered):
		// The build already produces what the cluster runs. Whatever the source says here IS
		// what produced it, so the source keeps its bytes — or its ABSENCE, which is how an
		// injected label stays out of a file that never declared one.
		return src, nil
	case equalNode(src, rendered):
		// The build does not touch this field, so there is nothing standing between the source
		// and the cluster: the live value is the user's, and it is written through. This is
		// also what keeps the change a no-op for every document no transformer rewrites.
		return live, nil
	}

	// Both moved it. Decompose, so that a build-supplied field sitting inside a subtree the
	// user did change is still left to the source — the ordinary case of an image bump on a
	// container whose resources a patch pins.
	if renderedMap, ok := asMap(rendered); ok {
		if liveMap, isMap := asMap(live); isMap {
			if srcMap, srcIsMap := asMap(src); srcIsMap || !src.present {
				return restoreMap(srcMap, renderedMap, liveMap, field)
			}
		}
	}
	if renderedList, ok := asList(rendered); ok {
		if liveList, isList := asList(live); isList {
			if srcList, srcIsList := asList(src); srcIsList || !src.present {
				return restoreList(srcList, renderedList, liveList, field)
			}
		}
	}

	// A scalar the build and the user both wrote, or a field whose very shape changed. The
	// user's value goes in, and if the build owns the field the render will not reproduce it —
	// the render precondition refuses the flush and names the file. A guess here would be a
	// silent non-converging write; a write-through is a reported refusal.
	return live, nil
}

// restoreMap applies the rule key by key, over every key any of the three views carries.
func restoreMap(src, rendered, live map[string]interface{}, field string) (node, error) {
	out := make(map[string]interface{}, len(live))
	for _, key := range unionKeys3(src, rendered, live) {
		got, err := restore(at(src, key), at(rendered, key), at(live, key), join(field, key))
		if err != nil {
			return absent(), err
		}
		if got.present {
			out[key] = deepCopyValue(got.value)
		}
	}
	return node{value: out, present: true}, nil
}

// restoreList applies the rule element by element, pairing the three lists BY NAME.
//
// Name is the only pairing available, and it has to be verified rather than assumed: kustomize
// prepends a container a patch adds, so the source's element 0 is not the render's element 0.
// Where any of the three lists is not a set of uniquely-named maps, nothing is paired and the
// edit is refused (SourceFormRefusedError) — never aligned by position.
//
// The source's ORDER is the file's order, so it is the output's order: an element the user adds
// is appended, and an element the BUILD adds is dropped (its source node is absent, and the rule
// returns absence).
func restoreList(src, rendered, live []interface{}, field string) (node, error) {
	srcByName, srcNamed := byName(src)
	renderedByName, renderedNamed := byName(rendered)
	liveByName, liveNamed := byName(live)
	if !srcNamed || !renderedNamed || !liveNamed {
		return absent(), &SourceFormRefusedError{Field: field}
	}

	out := make([]interface{}, 0, len(live))
	emit := func(name string, srcNode node) error {
		got, err := restore(srcNode, named(renderedByName, name), named(liveByName, name), join(field, name))
		if err != nil {
			return err
		}
		if got.present {
			out = append(out, deepCopyValue(got.value))
		}
		return nil
	}

	for _, element := range src {
		if err := emit(nameOf(element), node{value: element, present: true}); err != nil {
			return absent(), err
		}
	}
	for _, element := range live {
		if name := nameOf(element); !hasName(srcByName, name) {
			if err := emit(name, absent()); err != nil {
				return absent(), err
			}
		}
	}
	return node{value: out, present: true}, nil
}

// byName indexes a list by its elements' name: field, reporting false unless EVERY element is a
// map carrying a non-empty, unique name. A list that fails this cannot be paired across the
// three views, and the edit is refused rather than aligned by position.
func byName(list []interface{}) (map[string]interface{}, bool) {
	out := make(map[string]interface{}, len(list))
	for _, element := range list {
		name := nameOf(element)
		if name == "" {
			return nil, false
		}
		if _, duplicate := out[name]; duplicate {
			return nil, false
		}
		out[name] = element
	}
	return out, true
}

func nameOf(element interface{}) string {
	m, isMap := element.(map[string]interface{})
	if !isMap {
		return ""
	}
	name, _ := m["name"].(string)
	return name
}

func named(byName map[string]interface{}, name string) node {
	value, found := byName[name]
	return node{value: value, present: found}
}

func hasName(byName map[string]interface{}, name string) bool {
	_, found := byName[name]
	return found
}

func at(m map[string]interface{}, key string) node {
	if m == nil {
		return absent()
	}
	value, found := m[key]
	return node{value: value, present: found}
}

func asMap(n node) (map[string]interface{}, bool) {
	if !n.present {
		return nil, false
	}
	m, ok := n.value.(map[string]interface{})
	return m, ok
}

func asList(n node) ([]interface{}, bool) {
	if !n.present {
		return nil, false
	}
	l, ok := n.value.([]interface{})
	return l, ok
}

// equalNode compares two views of one field. Absence is a value: two absences are equal, and an
// absence never equals a present field.
//
// Canonical JSON, not reflect.DeepEqual, and that is a landmine rather than a preference: the
// three views carry a number in three different Go types. kustomize hands back `int`, the API
// machinery uses `int64`, and sigs.k8s.io/yaml decodes the source file into `float64` — so
// DeepEqual calls an unchanged replica count changed, and every source file in the corpus would
// be rewritten by a projection that believed the user had scaled it.
func equalNode(a, b node) bool {
	if a.present != b.present {
		return false
	}
	if !a.present {
		return true
	}
	left, err := json.Marshal(a.value)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b.value)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// unionKeys3 is every key any of the three views carries, sorted, so the walk is deterministic.
func unionKeys3(a, b, c map[string]interface{}) []string {
	seen := make(map[string]struct{}, len(a)+len(b)+len(c))
	for _, m := range []map[string]interface{}{a, b, c} {
		for key := range m {
			seen[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// deepCopyValue copies a node out of the source document or the live object, so the result
// shares no structure with either and the caller may edit it freely (the image slots are set on
// it straight afterwards).
//
// It is only ever handed a value from src or live, never from the render — which is the whole
// point of the rule, and also why it is safe: a RENDERED object is not valid JSON-typed
// unstructured (kustomize's `int` makes runtime.DeepCopyJSONValue panic outright), while src
// and live both come out of a JSON decoder.
func deepCopyValue(value interface{}) interface{} {
	return runtime.DeepCopyJSONValue(value)
}

// join builds the field path a refusal names, as a user would read it.
func join(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}
