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

// SplitDesiredForOverrides maps the live desired object back through the
// override chain. It returns the object the source document should be compared
// against (a copy of desired with override-produced values restored to their
// source form) plus the entry edits for values whose supplier is an override
// entry. Anything it cannot route safely — a component removal an entry
// supplies, conflicting values for one entry field, or a simulated render that
// would not reproduce live — falls back to the unmodified live value
// (today's write-through), never to a guess.
//
// gitRaw is the source document parsed as JSON-typed maps (sigs.k8s.io/yaml);
// desired is the sanitized projection the writer would otherwise compare. The
// returned object is always a copy; desired is never mutated.
func SplitDesiredForOverrides(
	gitRaw map[string]interface{},
	desired *unstructured.Unstructured,
	ov *KustomizeOverrides,
) (*unstructured.Unstructured, []OverrideEdit) {
	if ov == nil || desired == nil || gitRaw == nil {
		return desired, nil
	}
	out := desired.DeepCopy()
	edits := projectImages(gitRaw, out, ov.Images)
	edits = append(edits, projectReplicas(gitRaw, out, ov.Replicas)...)
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

// imageSuppliers records which override entry last supplied each component of a
// rendered image; nil means the source file supplies it.
type imageSuppliers struct {
	name   *ImageOverride
	tag    *ImageOverride
	digest *ImageOverride
}

// renderImage runs the override chain over a source image, kustomize-style:
// each entry whose name matches the image's CURRENT name rewrites the
// components it declares, in chain order.
//
// Tag and digest are MUTUALLY EXCLUSIVE, and this is the part that is easy to get
// wrong — we did. Quoting kustomize's own image transformer
// (filters/imagetag/updater.go, SetImageValue):
//
//	// overriding tag or digest will replace both original tag and digest values
//	case NewTag != "" && Digest != "": tag = NewTag; digest = Digest
//	case NewTag != "":                 tag = NewTag; digest = ""
//	case Digest != "":                 tag = "";     digest = Digest
//
// Setting the two components independently makes us believe a folder renders to
// `web:1.0@sha256:abc` where kustomize renders `web@sha256:abc`. The projection
// then reads the difference as a user removing the tag and rewrites the tag out of
// the source file — silent corruption, on every reconcile. Pinned against a real
// `kustomize build` by TestRenderImage_MatchesKustomizeOnTheHardCases.
func renderImage(src imageRef, entries []ImageOverride) (imageRef, imageSuppliers) {
	cur := src
	var sup imageSuppliers
	for i := range entries {
		e := &entries[i]
		if e.Name != cur.name {
			continue
		}
		if e.HasNewName {
			cur.name = e.NewName
			sup.name = e
		}
		switch {
		case e.HasNewTag && e.HasDigest:
			cur.tag, cur.digest = e.NewTag, e.Digest
			sup.tag, sup.digest = e, e
		case e.HasNewTag:
			cur.tag, cur.digest = e.NewTag, ""
			sup.tag, sup.digest = e, e
		case e.HasDigest:
			cur.tag, cur.digest = "", e.Digest
			sup.tag, sup.digest = e, e
		}
	}
	return cur, sup
}

// containerSlot is one container-shaped list item holding an image, addressed
// by its list path plus container name so the live and Git objects align.
type containerSlot struct {
	key   string
	item  map[string]interface{}
	image string
}

func isContainerListKey(k string) bool {
	switch k {
	case "containers", "initContainers", "ephemeralContainers":
		return true
	default:
		return false
	}
}

// collectContainerSlots walks the object for container lists at any depth,
// mirroring the builtin image transformer's generic traversal. Slots are sorted
// by key for deterministic edit output.
func collectContainerSlots(obj map[string]interface{}) []containerSlot {
	var out []containerSlot
	var walk func(prefix string, v interface{})
	walk = func(prefix string, v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, val := range t {
				p := prefix + "/" + k
				if isContainerListKey(k) {
					if list, ok := val.([]interface{}); ok {
						out = append(out, containerSlotsOf(p, list)...)
						continue
					}
				}
				walk(p, val)
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

func containerSlotsOf(listPath string, list []interface{}) []containerSlot {
	var out []containerSlot
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
		out = append(out, containerSlot{key: listPath + "\x00" + name, item: m, image: image})
	}
	return out
}

// slotPlan is the per-container outcome of the inversion: the image the source
// file should hold, the live image it must render to, and the entry edits that
// make the render true.
type slotPlan struct {
	slot      containerSlot
	fileImage string
	live      imageRef
	edits     []OverrideEdit
}

// projectImages inverts the image chain for every container the live object and
// the Git document share. It mutates out's container images to their
// source-file form and returns the entry edits — or routes nothing (leaving the
// live values in out) when the inversion is unsafe: conflicting edits to one entry
// field, or a removal an entry supplies.
//
// Nothing here is trusted. The proposal it produces is put to kustomize before it can
// become a commit (VerifyWriteProposal), so this only has to be a candidate that is
// usually right — and routing nothing is always a legal answer, because the re-render
// then adjudicates whatever the source document alone can carry.
func projectImages(
	gitRaw map[string]interface{},
	out *unstructured.Unstructured,
	entries []ImageOverride,
) []OverrideEdit {
	if len(entries) == 0 {
		return nil
	}
	gitImages := map[string]string{}
	for _, s := range collectContainerSlots(gitRaw) {
		gitImages[s.key] = s.image
	}

	var plans []slotPlan
	for _, slot := range collectContainerSlots(out.Object) {
		src, exists := gitImages[slot.key]
		if !exists {
			continue // a new container writes through; the supplier rule converges it later
		}
		plan, routable := invertImage(slot, src, entries)
		if !routable {
			return nil // one unroutable container abandons routing for the object
		}
		plans = append(plans, plan)
	}
	edits, ok := collectConsistentEdits(plans)
	if !ok {
		return nil
	}
	for _, p := range plans {
		p.slot.item["image"] = p.fileImage
	}
	return edits
}

// invertImage computes one container's source-file image and entry edits.
// routable is false when a live change cannot be expressed on the existing
// entries (a component removal whose supplier is an entry).
func invertImage(slot containerSlot, src string, entries []ImageOverride) (slotPlan, bool) {
	srcRef := parseImageRef(src)
	rendered, sup := renderImage(srcRef, entries)
	live := parseImageRef(slot.image)
	plan := slotPlan{slot: slot, live: live}
	if rendered == live {
		plan.fileImage = src
		return plan, true
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
		if sup.name != nil {
			route(sup.name, "newName", live.name)
		} else {
			newSrc.name = live.name
		}
	}
	if live.tag != rendered.tag {
		if !routeComponent(sup.tag, declaresNewTag, live.tag,
			func(v string) { newSrc.tag = v },
			func(e *ImageOverride, v string) { route(e, "newTag", v) }) {
			return plan, false
		}
	}
	if live.digest != rendered.digest {
		if !routeComponent(sup.digest, declaresDigest, live.digest,
			func(v string) { newSrc.digest = v },
			func(e *ImageOverride, v string) { route(e, "digest", v) }) {
			return plan, false
		}
	}
	plan.fileImage = newSrc.String()
	return plan, true
}

// declaresNewTag / declaresDigest report whether an entry actually carries the key
// the writer would have to set. An entry can GOVERN a component without declaring
// it: a digest entry clears the tag, and a newTag entry clears the digest.
func declaresNewTag(e *ImageOverride) bool { return e.HasNewTag }
func declaresDigest(e *ImageOverride) bool { return e.HasDigest }

// routeComponent decides where one changed image component (tag or digest) goes:
// into the source file when no entry supplies it, onto the supplying entry when
// that entry declares the key, or nowhere at all. It reports false when the change
// is unroutable, which abandons routing for the whole object (write-through).
//
// The two unroutable cases are worth naming:
//
//   - a REMOVAL of a component an entry supplies — there is no way to say "no tag"
//     on an entry that sets one; and
//   - a change to a component an entry governs but does not declare — a digest entry
//     clears the tag, so setting a tag has no key to land in, and writing it into the
//     source file would be undone by the very next render.
func routeComponent(
	sup *ImageOverride,
	declares func(*ImageOverride) bool,
	live string,
	setSource func(string),
	route func(*ImageOverride, string),
) bool {
	switch {
	case sup == nil:
		setSource(live) // the source file supplies it; the change flows into the file
	case live == "":
		return false
	case !declares(sup):
		return false
	default:
		route(sup, live)
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

// replicaKinds are the kinds the builtin replica transformer touches.
func isReplicaKind(kind string) bool {
	switch kind {
	case "Deployment", "ReplicaSet", "StatefulSet":
		return true
	default:
		return false
	}
}

// projectReplicas inverts the replicas transformer: when an entry pins this
// document's replica count, the source form of spec.replicas is restored on out
// (including its absence — the transformer creates the field) and a count edit
// is emitted only when live diverges from the pinned count.
func projectReplicas(
	gitRaw map[string]interface{},
	out *unstructured.Unstructured,
	entries []ReplicaOverride,
) []OverrideEdit {
	if len(entries) == 0 || !isReplicaKind(out.GetKind()) {
		return nil
	}
	sup := replicaSupplier(entries, out.GetName())
	if sup == nil {
		return nil
	}
	liveCount, liveHas, err := unstructured.NestedInt64(out.Object, "spec", "replicas")
	if err != nil || !liveHas {
		return nil
	}

	restoreSourceReplicas(gitRaw, out)
	if liveCount == sup.Count {
		return nil
	}
	return []OverrideEdit{{
		KustomizationPath: sup.Source,
		Edit: manifestedit.KustomizationEdit{
			Section:    manifestedit.KustomizationSectionReplicas,
			EntryIndex: sup.Index,
			EntryName:  sup.Name,
			Field:      "count",
			Value:      strconv.FormatInt(liveCount, 10),
		},
	}}
}

// replicaSupplier is the last entry in the chain matching the document's name —
// the one whose count the render ends up with.
func replicaSupplier(entries []ReplicaOverride, name string) *ReplicaOverride {
	var sup *ReplicaOverride
	for i := range entries {
		if entries[i].Name == name {
			sup = &entries[i]
		}
	}
	return sup
}

// ReplicaCountEdit returns the entry edit that absorbs a live replica count for
// the document, when its override chain governs spec.replicas. The writer's
// field-patch path (the /scale subresource) uses it to route a scale to the
// kustomization entry instead of writing the count into the source manifest.
func ReplicaCountEdit(dm *DocumentModel, count int64) (OverrideEdit, bool) {
	if dm == nil || dm.Overrides == nil || !isReplicaKind(dm.ManifestIdentity.Kind) {
		return OverrideEdit{}, false
	}
	sup := replicaSupplier(dm.Overrides.Replicas, dm.ManifestIdentity.Name)
	if sup == nil {
		return OverrideEdit{}, false
	}
	return OverrideEdit{
		KustomizationPath: sup.Source,
		Edit: manifestedit.KustomizationEdit{
			Section:    manifestedit.KustomizationSectionReplicas,
			EntryIndex: sup.Index,
			EntryName:  sup.Name,
			Field:      "count",
			Value:      strconv.FormatInt(count, 10),
		},
	}, true
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
