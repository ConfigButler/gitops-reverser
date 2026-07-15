// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// This file turns a pair of renders — the real one and the dyed one — into the answer the
// projection needs: for this document, what does kustomize render each image and replica
// count to, and WHICH ENTRY supplied it.
//
// Both halves come from the renderer. The VALUES are read off the real render, so what we
// believe a folder renders to is what kustomize says it renders to. The SUPPLIERS are read
// off the dyed render, so which entry owns a field is observed rather than re-derived.
// Nothing here re-implements a transformer, which is the whole point: the transformers we
// used to re-implement are where every shipped bug in this area came from.
//
// See docs/design/support-boundary/render-attribution.md §3.

// RenderedOverrides is what kustomize renders one document to, plus the override entry
// behind each override-produced value. A nil supplier means THE SOURCE DOCUMENT supplies
// that value — so an edit to it belongs in the file, not in an entry.
type RenderedOverrides struct {
	// Object is the whole object kustomize renders this document to. It is what the live
	// object is compared against, and it is the reason the writer can tell a value the BUILD
	// supplied from a value the USER set without modelling a single transformer: where the
	// live object and the render agree, the source keeps its bytes (see sourceForm).
	//
	// It is JSON-normalised, because a rendered object is not valid unstructured: kustomize
	// hands numbers back as Go `int`, which makes DeepCopyJSON panic outright.
	Object map[string]interface{}
	// Images is keyed by image slot (the container list path plus the container name), so
	// the live object, the Git document and the render all address the same field.
	Images map[string]RenderedImage
	// Replicas is set only when the document actually renders a spec.replicas — which is
	// kustomize's decision, not ours.
	Replicas *RenderedReplicas
}

// RenderedImage is one image slot: what it renders to, and who supplied each component.
type RenderedImage struct {
	// Rendered is the image kustomize produces for this slot.
	Rendered string
	// Name, Tag and Digest are the entries supplying each component, or nil when the
	// source document does. They are the dye's answer, and the reason renderImage is gone.
	Name, Tag, Digest *ImageOverride
}

// RenderedReplicas is the rendered spec.replicas and the entry that pinned it (nil when the
// source document supplies the count).
type RenderedReplicas struct {
	Rendered int64
	Entry    *ReplicaOverride
}

// attributeRoot pairs one root's real render with its dyed render and reads the dyes off it.
//
// BASELINE FIRST, THEN DYE. If the dyed build fails where the real one succeeded, the dye has
// perturbed something that is not a pure sink — a replacements: block consuming the image as
// a SOURCE will do exactly this — and the honest answer is that we cannot attribute this root.
// The fallback is NO ATTRIBUTION, and it is never another heuristic: not renderImage, not
// leave-one-out, not "probably the last matching entry". Those are the silent-corruption paths
// this design exists to delete, and the moment the renderer says "I cannot tell you" is the
// worst possible moment to start guessing. With no attribution nothing routes to an entry, the
// proposal falls back to what the source document alone can carry, and the verification
// re-render adjudicates it — which, for a field an entry governs, means a refused flush. That
// is the correct outcome, and it is reported rather than absorbed.
//
// Objects are aligned BY POSITION, never by name: a generated name can carry a content hash
// that drifts between two builds. The keys are then required to agree at every position, so a
// misalignment refuses attribution instead of silently attributing one object's dyes to
// another.
func attributeRoot(plain, dyed []renderedObject, dyeErr error, plan *dyePlan) map[chainKey]*RenderedOverrides {
	if dyeErr != nil || len(plain) != len(dyed) {
		return nil
	}
	out := make(map[chainKey]*RenderedOverrides, len(plain))
	for i := range plain {
		key := renderedKey(plain[i])
		if key != renderedKey(dyed[i]) {
			return nil // the two builds disagree on what they rendered; attribute nothing
		}
		if plain[i].OriginPath == "" {
			continue // generated: no source document to route an edit into
		}
		if attribution := readDyes(plain[i], dyed[i], plan); attribution != nil {
			out[key] = attribution
		}
	}
	return out
}

func renderedKey(o renderedObject) chainKey {
	return chainKey{originPath: o.OriginPath, kind: o.Object.GetKind(), name: o.Object.GetName()}
}

// readDyes reads the nonces out of ONE document's image slots and replica count, and keeps the
// rendered object itself.
//
// Only the fields being attributed are read FOR ATTRIBUTION. The whole output is never grepped
// for a nonce: vars and replacements can carry a dyed value into args, env, or ConfigMap data,
// and a dye found there says nothing about who supplies the image.
//
// The rendered object is kept whole, though, and that is a different question from attribution:
// the writer does not need to know WHO supplied a field in order to know THAT the build did —
// it only has to compare the live object against the render (see sourceForm). So a document with
// no image slot and no replica count still gets a RenderedOverrides: it has no entry to route
// to, but the build may still have written into it, and the source must be protected from that.
func readDyes(plain, dyed renderedObject, plan *dyePlan) *RenderedOverrides {
	out := &RenderedOverrides{
		Object: jsonNormalised(plain.Object.Object),
		Images: map[string]RenderedImage{},
	}

	dyedSlots := map[string]imageSlot{}
	for _, s := range collectImageSlots(dyed.Object.Object) {
		dyedSlots[s.key] = s
	}
	for _, slot := range collectImageSlots(plain.Object.Object) {
		image := RenderedImage{Rendered: slot.image}
		if probe, found := dyedSlots[slot.key]; found {
			ref := parseImageRef(probe.image)
			image.Name = markedImage(plan, ref.name, fieldNewName)
			image.Tag = markedImage(plan, ref.tag, fieldNewTag)
			image.Digest = markedImage(plan, ref.digest, fieldDigest)
		}
		out.Images[slot.key] = image
	}

	// spec.replicas is read off the object rather than gated on a list of kinds we believe
	// the transformer touches. kustomize's fieldspec is the authority: if a dyed count came
	// out here, an entry governs this field, whatever the kind is. That is how
	// ReplicationController — which our own isReplicaKind forgot — attributes for free.
	if count, ok := renderedReplicaCount(plain.Object.Object); ok {
		replicas := &RenderedReplicas{Rendered: count}
		if dyedCount, found := renderedReplicaCount(dyed.Object.Object); found {
			if mark, isDye := plan.byReplicaCount[dyedCount]; isDye && mark.field == fieldCount {
				replicas.Entry = mark.replica
			}
		}
		out.Replicas = replicas
	}

	if out.Object == nil {
		return nil // an object we cannot even normalise is one we must not compare against
	}
	return out
}

// jsonNormalised round-trips a RENDERED object through JSON.
//
// It is not tidiness. A rendered object is not a valid unstructured: kustomize hands numbers
// back as Go `int`, and DeepCopyJSON accepts only the JSON types, so unstructured.DeepCopy
// PANICS on one outright ("cannot deep copy int"). The API server hands out JSON, so the live
// object this is compared against has int64 — and the comparison has to be made between two
// values a JSON encoder agrees on, not between two Go types that happen to hold the same number.
func jsonNormalised(obj map[string]interface{}) map[string]interface{} {
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

// markedImage looks a candidate nonce up, and requires it to have been minted FOR THE FIELD IT
// WAS FOUND IN. A tag dye surfacing in the name position is not attribution, it is a signal
// that something we do not model moved the value, so it is not treated as a supplier.
func markedImage(plan *dyePlan, value, field string) *ImageOverride {
	if value == "" {
		return nil
	}
	mark, found := plan.byNonce[value]
	if !found || mark.field != field {
		return nil
	}
	return mark.image
}

// renderedReplicaCount reads spec.replicas off a RENDERED object.
//
// unstructured.NestedInt64 is deliberately not used, and this is a landmine rather than a
// preference: kustomize hands numbers back as Go `int`, so NestedInt64 returns found=FALSE on
// a rendered spec.replicas and the caller silently reads zero. Measured.
func renderedReplicaCount(obj map[string]interface{}) (int64, bool) {
	spec, ok := obj["spec"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	switch n := spec["replicas"].(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// fingerprintRendered reduces an attribution to a comparable string, so two render roots
// reaching one document can be compared for agreement exactly as their chains are. This is the
// sharper question the fan-in check was always trying to ask: do two roots attribute this
// field to DIFFERENT entries?
//
// It covers the whole RENDERED OBJECT as well as the entries, and that is load-bearing rather
// than thorough. Two overlays that render one base document to different objects — one patching
// an env var, the other not — agree on every images:/replicas: entry and would fingerprint
// identically. The writer would then compare a live object from one environment against the
// other environment's render and call the difference a user edit. Asking whether the two roots
// RENDER THE SAME OBJECT is the question that has to be asked, and the answer marks the document
// ambiguous: no attribution, and the fan-in precondition refuses any write to it.
func fingerprintRendered(rd *RenderedOverrides) string {
	if rd == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(hashObject(rd.Object))
	b.WriteByte('\x03')
	b.WriteString(fingerprintEntries(rd))
	return b.String()
}

// hashObject digests a rendered object. json.Marshal sorts map keys, so it is key-order
// independent, and the digest keeps the fingerprint small enough to be a map key.
func hashObject(obj map[string]interface{}) string {
	encoded, err := json.Marshal(obj)
	if err != nil {
		return "\x00unhashable"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// fingerprintEntries reduces the per-field ATTRIBUTION — which entry supplied which value — to a
// comparable string.
func fingerprintEntries(rd *RenderedOverrides) string {
	keys := make([]string, 0, len(rd.Images))
	for k := range rd.Images {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		img := rd.Images[k]
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(img.Rendered)
		b.WriteByte(0)
		b.WriteString(entryRef(img.Name) + entryRef(img.Tag) + entryRef(img.Digest))
		b.WriteByte(1)
	}
	if rd.Replicas != nil {
		b.WriteString(strconv.FormatInt(rd.Replicas.Rendered, 10))
		b.WriteByte(0)
		if e := rd.Replicas.Entry; e != nil {
			b.WriteString(e.Source + ":" + strconv.Itoa(e.Index))
		}
	}
	return b.String()
}

func entryRef(e *ImageOverride) string {
	if e == nil {
		return "-;"
	}
	return e.Source + ":" + strconv.Itoa(e.Index) + ";"
}
