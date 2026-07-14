// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file is the projection half of the images/replicas edit-through (see
// overrides.go for the model half):
// given the source document as Git holds it, the live desired object, and the
// governing override chain, it splits the live state into what the SOURCE FILE
// should hold and what the KUSTOMIZATION ENTRIES should hold — "the edit lands
// where the value lives". See
// docs/design/support-boundary/finished/images-and-replicas-edit-through.md.

// OverrideEdit routes one live-value change to a field of an existing
// kustomization override entry.
type OverrideEdit struct {
	// KustomizationPath is the kustomization file (slash, relative to the
	// GitTarget subtree) declaring the entry.
	KustomizationPath string
	// Edit is the bounded scalar update the manifestedit editor applies.
	Edit manifestedit.KustomizationEdit
}

// SplitDesiredForOverrides maps the live desired object back through what kustomize actually
// renders. It returns the object the source document should be compared against — a copy of
// desired with every override-produced value restored to its SOURCE form, so the file keeps
// its bytes — plus the entry edits for the values an override entry supplies.
//
// It is driven by RenderedOverrides, which carries both halves of the answer straight from
// the renderer: what each field renders to, and which entry supplied it (read off a dyed
// counterfactual build). Nothing in here re-implements a kustomize transformer, which is the
// entire point of this workstream — every shipped bug in this area came from the
// re-implementation, and all of them are deleted with it.
//
// Anything it cannot route safely — a component removal an entry supplies, a component a
// sibling entry clears, or two containers demanding different values for one entry field —
// routes NOTHING and leaves the live value in place. That is not a guess and not a fallback to
// another heuristic: the proposal then has to survive the verification re-render, which for a
// field an entry governs it will not, so it becomes a reported refusal rather than a commit
// that quietly never converges.
//
// gitRaw is the source document parsed as JSON-typed maps (sigs.k8s.io/yaml); desired is the
// sanitized projection the writer would otherwise compare. The returned object is always a
// copy; desired is never mutated.
func SplitDesiredForOverrides(
	gitRaw map[string]interface{},
	desired *unstructured.Unstructured,
	rendered *RenderedOverrides,
) (*unstructured.Unstructured, []OverrideEdit) {
	if rendered == nil || desired == nil || gitRaw == nil {
		return desired, nil
	}
	out := desired.DeepCopy()
	edits := projectImages(gitRaw, out, rendered.Images)
	edits = append(edits, projectReplicas(gitRaw, out, rendered.Replicas)...)
	return out, edits
}

// imageRef is an image reference split into its three overridable components.
type imageRef struct {
	name   string
	tag    string
	digest string
}

// parseImageRef splits name[:tag][@digest]. A colon inside the registry host
// (e.g. localhost:5000/app) is not a tag separator.
func parseImageRef(s string) imageRef {
	ref := imageRef{}
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		ref.digest = rest[i+1:]
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		ref.tag = rest[i+1:]
		rest = rest[:i]
	}
	ref.name = rest
	return ref
}

func (r imageRef) String() string {
	out := r.name
	if r.tag != "" {
		out += ":" + r.tag
	}
	if r.digest != "" {
		out += "@" + r.digest
	}
	return out
}

// imageSlot is one image-bearing field of an object, addressed by its list path plus the
// item's name so the live object, the Git document and the render all address the same one.
// set writes a new image back into whichever shape the field has.
type imageSlot struct {
	key   string
	image string
	set   func(string)
}

func isContainerListKey(k string) bool {
	switch k {
	case "containers", "initContainers", "ephemeralContainers":
		return true
	default:
		return false
	}
}

// collectImageSlots walks the object for every field that can hold an image.
//
// Which fields those are was MEASURED against kustomize, not derived from its fieldspecs,
// and the two surprises are both in here:
//
//   - volumes[].image.reference — an OCI volume source. kustomize REWRITES it (measured), and
//     the old collector did not look at it, so the rendered value was written back into the
//     source document as if the user had typed it.
//   - ephemeralContainers — kustomize does NOT rewrite them (measured), so no dye ever lands
//     here and no entry is ever credited with the value. They are still collected, because the
//     SOURCE document owns them and an edit to one belongs in the file. That is the dye doing
//     the fieldspec's job: we no longer have to know which fields kustomize touches, only to
//     look at where its dyes came out.
//
// Slots are sorted by key so edit output is deterministic.
func collectImageSlots(obj map[string]interface{}) []imageSlot {
	var out []imageSlot
	var walk func(prefix string, v interface{})
	walk = func(prefix string, v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, val := range t {
				p := prefix + "/" + k
				list, isList := val.([]interface{})
				switch {
				case isList && isContainerListKey(k):
					out = append(out, containerImageSlots(p, list)...)
				case isList && k == "volumes":
					out = append(out, volumeImageSlots(p, list)...)
				default:
					walk(p, val)
				}
			}
		case []interface{}:
			for i, item := range t {
				walk(fmt.Sprintf("%s/%d", prefix, i), item)
			}
		}
	}
	walk("", obj)
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// containerImageSlots reads containers[].image — a plain string field.
func containerImageSlots(listPath string, list []interface{}) []imageSlot {
	var out []imageSlot
	for _, item := range list {
		m, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		name, _ := m["name"].(string)
		image, hasImage := m["image"].(string)
		if name == "" || !hasImage {
			continue
		}
		out = append(out, imageSlot{
			key:   listPath + "\x00" + name,
			image: image,
			set:   func(v string) { m["image"] = v },
		})
	}
	return out
}

// volumeImageSlots reads volumes[].image.reference — a nested field, and the one the old
// collector missed. A volume with no image (a configMap or emptyDir) simply has no slot.
func volumeImageSlots(listPath string, list []interface{}) []imageSlot {
	var out []imageSlot
	for _, item := range list {
		m, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		name, _ := m["name"].(string)
		img, hasImage := m["image"].(map[string]interface{})
		if name == "" || !hasImage {
			continue
		}
		reference, hasReference := img["reference"].(string)
		if !hasReference {
			continue
		}
		out = append(out, imageSlot{
			key:   listPath + "\x00" + name,
			image: reference,
			set:   func(v string) { img["reference"] = v },
		})
	}
	return out
}

// slotPlan is the per-slot outcome of the inversion: the image the source file should
// hold, and the entry edits that make the render come out as live.
type slotPlan struct {
	slot      imageSlot
	fileImage string
	edits     []OverrideEdit
}

// projectImages inverts the image transformers for every slot the live object and the Git
// document share. It rewrites out's images to their SOURCE-FILE form and returns the entry
// edits — or routes nothing when the inversion is unsafe.
//
// Nothing here re-derives what kustomize does any more. The rendered value comes from the
// renderer and the supplier comes from the dye, so the two questions that used to be answered
// by a hand-written transformer — "what does this folder render to" and "who supplied it" —
// are now both answered by kustomize.
//
// And nothing here is trusted. The proposal is put to kustomize before it can become a commit
// (VerifyBatchRenders), so this only has to be a candidate that is usually right. Routing
// nothing is always a legal answer: the proposal then falls back to whatever the source
// document alone can carry, and the re-render adjudicates it.
func projectImages(
	gitRaw map[string]interface{},
	out *unstructured.Unstructured,
	rendered map[string]RenderedImage,
) []OverrideEdit {
	if len(rendered) == 0 {
		return nil
	}
	gitImages := map[string]string{}
	for _, s := range collectImageSlots(gitRaw) {
		gitImages[s.key] = s.image
	}

	var plans []slotPlan
	for _, slot := range collectImageSlots(out.Object) {
		src, inGit := gitImages[slot.key]
		render, isRendered := rendered[slot.key]
		if !inGit || !isRendered {
			continue // a new container writes through; the supplier rule converges it later
		}
		plan, routable := invertImage(slot, src, render)
		if !routable {
			return nil // one unroutable slot abandons routing for the whole object
		}
		plans = append(plans, plan)
	}
	edits, ok := collectConsistentEdits(plans)
	if !ok {
		return nil
	}
	for _, p := range plans {
		p.slot.set(p.fileImage)
	}
	return edits
}

// invertImage computes one slot's source-file image and entry edits, given what kustomize
// renders it to and which entry supplied each component.
//
// routable is false when a live change cannot be expressed on the entries that exist, which
// abandons routing for the whole object.
func invertImage(slot imageSlot, src string, render RenderedImage) (slotPlan, bool) {
	srcRef := parseImageRef(src)
	rendered := parseImageRef(render.Rendered)
	live := parseImageRef(slot.image)

	plan := slotPlan{slot: slot, fileImage: src}
	if rendered == live {
		return plan, true // the folder already renders to live; the file keeps its bytes
	}

	newSrc := srcRef
	route := func(entry *ImageOverride, field, value string) {
		plan.edits = append(plan.edits, OverrideEdit{
			KustomizationPath: entry.Source,
			Edit: manifestedit.KustomizationEdit{
				Section:    manifestedit.KustomizationSectionImages,
				EntryIndex: entry.Index,
				EntryName:  entry.Name,
				Field:      field,
				Value:      value,
			},
		})
	}

	if live.name != rendered.name {
		switch {
		case render.Name != nil:
			route(render.Name, fieldNewName, live.name)
		default:
			newSrc.name = live.name
		}
	}
	if live.tag != rendered.tag {
		if !routeComponent(render.Tag, render.Digest, live.tag,
			func(v string) { newSrc.tag = v },
			func(e *ImageOverride, v string) { route(e, fieldNewTag, v) }) {
			return plan, false
		}
	}
	if live.digest != rendered.digest {
		if !routeComponent(render.Digest, render.Tag, live.digest,
			func(v string) { newSrc.digest = v },
			func(e *ImageOverride, v string) { route(e, fieldDigest, v) }) {
			return plan, false
		}
	}
	plan.fileImage = newSrc.String()
	return plan, true
}

// routeComponent decides where one changed image component (tag or digest) goes: onto the
// entry that supplies it, into the source file when no entry does, or nowhere at all.
//
// TAG AND DIGEST ARE MUTUALLY EXCLUSIVE IN KUSTOMIZE, and that is what `sibling` is for. From
// its own image transformer (filters/imagetag/updater.go, SetImageValue):
//
//	case NewTag != "" && Digest != "": tag = NewTag; digest = Digest
//	case NewTag != "":                 tag = NewTag; digest = ""     // a tag entry CLEARS the digest
//	case Digest != "":                 tag = "";     digest = Digest // a digest entry CLEARS the tag
//
// So an entry can GOVERN a component it does not declare. When a digest entry has cleared the
// tag, no dye lands in the tag — nothing supplies it — but writing a tag into the source file
// would be wiped by the very next render. The dye cannot see that on its own; the sibling
// component's supplier is what reveals it, and it is the bug (#231) that corrupted real source
// files by rewriting a tag out of them.
//
// The two unroutable cases:
//
//   - a REMOVAL of a component an entry supplies — there is no way to say "no tag" on an
//     entry that sets one;
//   - a change to a component the SIBLING entry clears — nowhere to land, and the file would
//     be overridden straight back.
func routeComponent(
	supplier *ImageOverride,
	sibling *ImageOverride,
	live string,
	setSource func(string),
	route func(*ImageOverride, string),
) bool {
	switch {
	case supplier != nil && live == "":
		return false // cannot express "no tag" on an entry that sets one
	case supplier != nil:
		route(supplier, live)
	case sibling != nil:
		return false // the sibling component's entry clears this one; the file cannot own it
	default:
		setSource(live) // the source document supplies it; the change flows into the file
	}
	return true
}

// collectConsistentEdits dedupes the per-container edits, refusing when two
// containers demand different values for the same entry field. Output order is
// deterministic (path, section, index, field).
func collectConsistentEdits(plans []slotPlan) ([]OverrideEdit, bool) {
	type key struct {
		path    string
		section string
		index   int
		field   string
	}
	byKey := map[key]OverrideEdit{}
	for _, p := range plans {
		for _, e := range p.edits {
			k := key{e.KustomizationPath, e.Edit.Section, e.Edit.EntryIndex, e.Edit.Field}
			if prev, seen := byKey[k]; seen && prev.Edit.Value != e.Edit.Value {
				return nil, false
			}
			byKey[k] = e
		}
	}
	out := make([]OverrideEdit, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.KustomizationPath != b.KustomizationPath {
			return a.KustomizationPath < b.KustomizationPath
		}
		if a.Edit.Section != b.Edit.Section {
			return a.Edit.Section < b.Edit.Section
		}
		if a.Edit.EntryIndex != b.Edit.EntryIndex {
			return a.Edit.EntryIndex < b.Edit.EntryIndex
		}
		return a.Edit.Field < b.Edit.Field
	})
	return out, true
}

// projectReplicas inverts the replica transformer: when an entry pins this document's replica
// count, the source form of spec.replicas is restored on out (including its ABSENCE — the
// transformer creates the field) and a count edit is emitted only when live diverges from the
// pinned count.
//
// There is no list of kinds here any more, and that is a bug fix rather than a tidy-up. We
// used to gate this on isReplicaKind — Deployment, ReplicaSet, StatefulSet — while kustomize's
// fieldspec is Deployment, ReplicaSet, StatefulSet AND ReplicationController. A scale on an RC
// governed by a replicas: entry was written into the source document, where the transformer
// overrode it right back: non-converging drift, silently, forever. The dye ends the argument:
// if a dyed count came out of this object, an entry governs the field, whatever the kind is.
// kustomize's fieldspec is the authority, and we no longer keep a second opinion about it.
func projectReplicas(
	gitRaw map[string]interface{},
	out *unstructured.Unstructured,
	rendered *RenderedReplicas,
) []OverrideEdit {
	if rendered == nil || rendered.Entry == nil {
		return nil // no entry supplies the count; a scale flows into the source document
	}
	liveCount, liveHas, err := unstructured.NestedInt64(out.Object, "spec", "replicas")
	if err != nil || !liveHas {
		return nil
	}

	restoreSourceReplicas(gitRaw, out)
	if liveCount == rendered.Rendered {
		return nil // the folder already renders to live
	}
	return []OverrideEdit{replicaCountEdit(rendered.Entry, liveCount)}
}

// ReplicaCountEdit returns the entry edit that absorbs a live replica count for the document,
// when a replicas: entry supplies spec.replicas. The writer's field-patch path (the /scale
// subresource) uses it to route a scale onto the entry instead of writing the count into the
// source manifest, where the transformer would override it back.
func ReplicaCountEdit(dm *DocumentModel, count int64) (OverrideEdit, bool) {
	if dm == nil || dm.Rendered == nil || dm.Rendered.Replicas == nil || dm.Rendered.Replicas.Entry == nil {
		return OverrideEdit{}, false
	}
	return replicaCountEdit(dm.Rendered.Replicas.Entry, count), true
}

func replicaCountEdit(entry *ReplicaOverride, count int64) OverrideEdit {
	return OverrideEdit{
		KustomizationPath: entry.Source,
		Edit: manifestedit.KustomizationEdit{
			Section:    manifestedit.KustomizationSectionReplicas,
			EntryIndex: entry.Index,
			EntryName:  entry.Name,
			Field:      fieldCount,
			Value:      strconv.FormatInt(count, 10),
		},
	}
}

// sourceReplicaCount reads spec.replicas off a source KRM document. This is a
// manifest field, not kustomize configuration — kustomize's types cover the
// kustomization.yaml, not the documents it renders — so the decoding lives here.
// sigs.k8s.io/yaml decodes YAML numbers as float64 (via JSON), so an integral
// float is accepted; anything else is not a replica count.
func sourceReplicaCount(spec map[string]interface{}) (int64, bool) {
	switch n := spec["replicas"].(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

// restoreSourceReplicas puts spec.replicas back to the source document's form on
// the desired copy: the source's own value when it has one, absent when the
// source omits it (the transformer supplies the field either way).
func restoreSourceReplicas(gitRaw map[string]interface{}, out *unstructured.Unstructured) {
	spec, _ := gitRaw["spec"].(map[string]interface{})
	if src, ok := sourceReplicaCount(spec); ok {
		_ = unstructured.SetNestedField(out.Object, src, "spec", "replicas")
		return
	}
	unstructured.RemoveNestedField(out.Object, "spec", "replicas")
}
