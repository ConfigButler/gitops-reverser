// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"reflect"
	"sort"
	"strings"

	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file decodes kustomization.yaml with kustomize's own type
// (sigs.k8s.io/kustomize/api/types.Kustomization) instead of a hand-written walk
// over map[string]interface{}, and derives the unsupported-feature set by
// reflecting over that type's fields.
//
// The point is not brevity. It is that the check becomes exhaustive by
// construction: a kustomization field the operator has never heard of lands
// outside supportedKustomizationFields and refuses the folder, instead of being
// silently tolerated. The hand-written deny-list could only refuse what someone
// had remembered to add to it, and it had holes — see supportedKustomizationFields.
//
// See docs/design/support-boundary/kustomize-support-boundary.md §7.

// supportedKustomizationFields names the struct fields of kustypes.Kustomization
// that the operator models. Every other field, present and non-zero, refuses the
// folder by its own name.
//
// Inverting the old hand-written deny-list into an allowlist over kustomize's own
// type closed two holes it had, each of which let a kustomization render
// differently from what we believed:
//
//   - vars — a source document containing $(SOME_VAR) renders to the substituted
//     value. We mirrored that value straight back over the variable in the source
//     file. Silent corruption, in a folder we accepted.
//   - validators — plugin code. Arbitrary code is an unknowable render.
//
// Neither was in the deny-list, and neither could have been missed here: they are
// fields on kustomize's type, and anything not named below is refused. When
// kustomize grows a field, it lands outside this set and refuses rather than being
// silently tolerated.
//
// Deprecated spellings (bases, imageTags) are absent on purpose: FixKustomization
// folds them into resources/images before this map is consulted, exactly as the
// builder does.
func supportedKustomizationFields() map[string]struct{} {
	return map[string]struct{}{
		"TypeMeta": {}, // apiVersion / kind
		"MetaData": {}, // the kustomization's own metadata; renders nothing

		// The modelled subset: the contextual-namespace walk plus the two
		// edit-through channels.
		"Namespace": {},
		"Resources": {},
		"Images":    {},
		"Replicas":  {},

		// TOLERATED, NOT AUTHORED. A patch is read-only context: kustomize applies it, we
		// mirror what it renders, and nothing is ever routed INTO it. That is a weaker claim
		// than the four above, and it is the whole of what "tolerate" means — see
		// patchRefusals for the shapes that are still refused by name.
		//
		// It is only safe because the projection leaves every field the BUILD supplies to the
		// build (sourceForm): a patched base is no longer something the writer can absorb one
		// environment's values into. Tolerating patches without that is silent corruption, and
		// no re-render can catch it — the patch re-imposes its value, so the render comes out
		// identical either way.
		"Patches": {},

		// These inject metadata into every rendered object. They used to leak into mirrored
		// source files as drift — the writer mirrored the live object, injected labels and
		// all, back into the file the overlay renders. That is fixed at the source: the
		// projection leaves every field the BUILD supplies to the build (sourceForm), so an
		// injected label is no longer something the file can absorb.
		"CommonLabels":      {},
		"Labels":            {},
		"CommonAnnotations": {},
		"BuildMetadata":     {},

		"GeneratorOptions": {}, // inert on its own; every generator is refused below
		"SortOptions":      {}, // output ordering only
	}
}

// featureUnparseable is reported when kustomize's own decoder rejects the file.
// Unmarshal disallows unknown fields, so this also covers a key kustomize itself
// would refuse to build: if we cannot read it, we cannot vouch for what it renders.
const featureUnparseable = "unparseable"

// featureRemoteBase is reported for a resources/bases entry that is not a local
// path. It is the one piece of kustomize semantics we must keep owning: kustomize
// resolves a remote base by shelling out to `git fetch`, and it does so under
// LoadRestrictionsRootOnly and under an in-memory filesystem alike (both measured).
// Detecting it ourselves, before any build is invoked, is what keeps "we never
// fetch a remote base" true.
const featureRemoteBase = "remote-base"

const (
	featureMalformedImages   = "malformed-images"
	featureMalformedReplicas = "malformed-replicas"
)

// The patch shapes that are refused BY NAME. `patches:` is tolerated in exactly one shape — a
// `path:` to a sparse KRM document inside the scanned tree — and everything else says so rather
// than falling through into a folder we would then mishandle.
//
// The three of them are not arbitrary. Each is a different kind of thing wearing the same key:
//
//   - an INLINE patch is bytes in the kustomization, so there is no document to retain as build
//     context and no file an authoring step could ever edit;
//   - a JSON6902 patch is not a sparse KRM document at all — it is a list of `op`/`path`/`value`
//     operations, and a file full of them would otherwise be indexed as a broken manifest;
//   - a path leaving the scanned tree is a file we never read, so we cannot know what it does.
//
// The deprecated spellings need no entry here, and that was MEASURED rather than assumed:
// FixKustomization folds `bases` into `resources` and `imageTags` into `images`, but it does NOT
// fold `patchesStrategicMerge` or `patchesJson6902` into `Patches`. They stay in their own fields,
// land outside supportedKustomizationFields, and refuse the folder under their own names — which
// is what we want, and which a kustomize bump could change. TestParse_DeprecatedPatchSpellings
// pins it.
const (
	featurePatchInline      = "patches-inline"
	featurePatchJSON6902    = "patches-json6902"
	featurePatchOutsideTree = "patches-outside-tree"
)

// parseKustomization decodes one kustomization.yaml and reports every feature the
// operator does not model, sorted. An empty slice means the file is fully modelled.
// The doc is returned even when unsupported: callers keep it (so it never acts as a
// namespace source) rather than dropping it.
//
// tree is the scanned file set, needed because a `patches:` entry names a FILE and what that file
// holds decides whether we can tolerate it. A nil tree means the caller has no file set, and every
// patch path is then refused as unreadable rather than assumed benign.
func parseKustomization(content []byte, path string, tree map[string][]byte) (*kustomizationDoc, []string) {
	doc := &kustomizationDoc{path: path}

	// Unmarshal then FixKustomization is exactly what kustomize's own loader does
	// (internal/target/kusttarget.go: load), so we model the kustomization the
	// builder will actually see: bases folded into resources, imageTags into
	// images, the deprecated generator spellings normalised.
	//
	// The loader's CheckEmpty/EnforceFields validations are deliberately not run
	// here: they decide whether a build would succeed, which is a different
	// question from whether we can model the render, and adding them would refuse
	// more than this change intends to.
	var k kustypes.Kustomization
	if err := k.Unmarshal(content); err != nil {
		doc.unsupported = true
		doc.features = []string{featureUnparseable}
		return doc, doc.features
	}
	k.FixKustomization()

	doc.namespace = strings.TrimSpace(k.Namespace)
	doc.resources = trimmedEntries(k.Resources)

	features := map[string]struct{}{}
	for _, name := range unmodelledFields(k) {
		features[name] = struct{}{}
	}
	if hasRemoteResource(doc.resources) {
		features[featureRemoteBase] = struct{}{}
	}

	var ok bool
	if doc.images, ok = imageOverrides(k.Images, path); !ok {
		features[featureMalformedImages] = struct{}{}
	}
	if doc.replicas, ok = replicaOverrides(k.Replicas, path); !ok {
		features[featureMalformedReplicas] = struct{}{}
	}
	doc.patches = patchPaths(k.Patches, slashDir(path), tree)
	for _, refusal := range patchRefusals(k.Patches, slashDir(path), tree) {
		features[refusal] = struct{}{}
	}

	out := make([]string, 0, len(features))
	for f := range features {
		out = append(out, f)
	}
	sort.Strings(out)
	doc.unsupported = len(out) > 0
	doc.features = out
	return doc, out
}

// patchRefusals names every patch entry the operator will not tolerate. See the feature constants
// for why each shape is its own answer rather than a generic "unsupported".
func patchRefusals(entries []kustypes.Patch, dir string, tree map[string][]byte) []string {
	var out []string
	for _, entry := range entries {
		switch {
		case strings.TrimSpace(entry.Patch) != "":
			// Inline bytes, and this is also where an inline JSON6902 op list arrives —
			// `patches: [{patch: "- op: replace ...", target: {...}}]` decodes into exactly
			// this field, so refusing Patch outright refuses both spellings at once.
			out = append(out, featurePatchInline)
		case !patchFileIsSparseKRM(entry.Path, dir, tree):
			// The file is missing, unreadable, escapes the tree, or is not a KRM document —
			// a JSON6902 op list being the shape that most looks like a patch and least is one.
			out = append(out, patchPathRefusal(entry.Path, dir, tree))
		}
	}
	return out
}

// patchPathRefusal distinguishes "we cannot read that file" from "that file is not a patch we can
// tolerate", because they are different things for a user to fix.
func patchPathRefusal(entryPath, dir string, tree map[string][]byte) string {
	if resolvePatchPath(entryPath, dir, tree) == "" {
		return featurePatchOutsideTree
	}
	return featurePatchJSON6902
}

// patchPaths is the set of files this kustomization reads as patches — build inputs, never
// resources. They are retained outside the managed model exactly as kustomization.yaml is: a
// strategic-merge patch IS a KRM document, so nothing else stops the store from indexing it as a
// manifest, mirroring a live object over it, or sweeping it away as an orphan.
//
// That a patch produces no object of its own is not our claim — it is the RENDER's: a patch file
// never appears as a rendered object's origin. TestRetain_PatchFileIsNeverARenderOrigin pins it.
func patchPaths(entries []kustypes.Patch, dir string, tree map[string][]byte) []string {
	var out []string
	for _, entry := range entries {
		if strings.TrimSpace(entry.Patch) != "" {
			continue // inline: no file to retain, and refused anyway
		}
		if !patchFileIsSparseKRM(entry.Path, dir, tree) {
			continue // refused; retaining it would hide the very file the refusal names
		}
		out = append(out, resolvePatchPath(entry.Path, dir, tree))
	}
	sort.Strings(out)
	return out
}

// patchFileIsSparseKRM reports whether the file a patches: entry names is one we can tolerate: a
// readable document inside the scanned tree carrying an apiVersion and a kind.
//
// A sparse strategic-merge patch is a KRM document with most of its fields missing, so apiVersion
// + kind is the whole test — the fields it does carry are the patch. A JSON6902 op list decodes as
// a YAML SEQUENCE, so it fails this and is refused by name.
func patchFileIsSparseKRM(entryPath, dir string, tree map[string][]byte) bool {
	resolved := resolvePatchPath(entryPath, dir, tree)
	if resolved == "" {
		return false
	}
	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := yaml.Unmarshal(tree[resolved], &doc); err != nil {
		return false
	}
	return doc.APIVersion != "" && doc.Kind != ""
}

// resolvePatchPath resolves a patches: entry's path against the kustomization's own directory,
// returning "" when it is empty, remote, escapes the scanned tree, or names no file in it.
func resolvePatchPath(entryPath, dir string, tree map[string][]byte) string {
	entryPath = strings.TrimSpace(entryPath)
	if entryPath == "" || isRemoteResource(entryPath) {
		return ""
	}
	resolved := cleanJoin(dir, entryPath)
	if resolved == "" {
		return ""
	}
	if _, found := tree[resolved]; !found {
		return ""
	}
	return resolved
}

// kustomizationDecodeError returns kustomize's own decode error for a file it
// cannot build, or "" when it decodes. It is what makes an `unparseable` refusal
// actionable: "your kustomization.yaml is a Flux Kustomization CR" and "resources:
// is a string, not a list" are both unparseable, and the user needs to know which.
func kustomizationDecodeError(content []byte) string {
	var k kustypes.Kustomization
	if err := k.Unmarshal(content); err != nil {
		return err.Error()
	}
	return ""
}

// unmodelledFields returns the yaml key of every non-zero Kustomization field that
// is not in supportedKustomizationFields — the features that refuse the folder,
// named as the user wrote them.
func unmodelledFields(k kustypes.Kustomization) []string {
	var out []string
	modelledFields := supportedKustomizationFields()
	v := reflect.ValueOf(k)
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if _, modelled := modelledFields[field.Name]; modelled {
			continue
		}
		if v.Field(i).IsZero() {
			continue
		}
		out = append(out, kustomizationFieldKey(field))
	}
	return out
}

// kustomizationFieldKey is the yaml key a user writes for a struct field
// ("configMapGenerator"), so a refusal names the line in their file.
func kustomizationFieldKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" || tag == "-" {
		return f.Name
	}
	return tag
}

// imageOverrides converts kustomize's parsed images: entries into the writer's
// model. ok is false when an entry is present but not one we can route an edit
// through, which refuses the folder rather than misunderstanding it silently.
//
// TagSuffix is rejected on purpose. We do not model it, and kustomize's type would
// happily decode it — accepting it would mean writing a file we believe renders one
// way while kustomize renders it another. (The old hand-written key check refused it
// as an unknown key; this preserves that, deliberately.)
func imageOverrides(entries []kustypes.Image, source string) ([]ImageOverride, bool) {
	if len(entries) == 0 {
		return nil, true
	}
	out := make([]ImageOverride, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.TagSuffix) != "" {
			return nil, false
		}
		out = append(out, ImageOverride{
			Source:     source,
			Index:      i,
			Name:       e.Name,
			NewName:    e.NewName,
			NewTag:     e.NewTag,
			Digest:     e.Digest,
			HasNewName: e.NewName != "",
			HasNewTag:  e.NewTag != "",
			HasDigest:  e.Digest != "",
		})
	}
	return out, true
}

// replicaOverrides converts kustomize's parsed replicas: entries into the writer's
// model. A negative count is not a render kustomize would produce, so it is refused.
func replicaOverrides(entries []kustypes.Replica, source string) ([]ReplicaOverride, bool) {
	if len(entries) == 0 {
		return nil, true
	}
	out := make([]ReplicaOverride, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Name) == "" || e.Count < 0 {
			return nil, false
		}
		out = append(out, ReplicaOverride{Source: source, Index: i, Name: e.Name, Count: e.Count})
	}
	return out, true
}

// trimmedEntries returns the graph entries the walker follows, dropping blanks.
// bases: is already folded into resources: by FixKustomization.
func trimmedEntries(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		for _, e := range list {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

// parseKustomizations reads every kustomization.yaml into a kustomizationDoc keyed
// by its directory. An unparseable kustomization, or one using an unsupported
// feature, is kept but marked unsupported so it never acts as a namespace source.
//
// It is the ONE place a kustomization is judged, and it is file-aware because it has to be: a
// `patches:` entry names a file, and what that file holds — a sparse KRM document, or a JSON6902
// op list, or nothing at all — is what decides whether the folder can be tolerated. Every consumer
// (the acceptance gate, the repo scan, the namespace walk) reads the doc this produces, so no two
// of them can drift on what "unsupported" means.
func parseKustomizations(files []manifestedit.FileContent) map[string]*kustomizationDoc {
	tree := contentByPath(files)
	out := map[string]*kustomizationDoc{}
	for _, f := range files {
		if !isKustomizationFile(f.Path) {
			continue
		}
		doc, _ := parseKustomization(f.Content, filepathToSlash(f.Path), tree)
		out[slashDir(f.Path)] = doc
	}
	return out
}

// contentByPath indexes the scan by slash path, so a kustomization can be judged against the
// files it names.
func contentByPath(files []manifestedit.FileContent) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[filepathToSlash(f.Path)] = f.Content
	}
	return out
}

// patchFilesOf is every file any kustomization in the scan reads as a patch. They are build
// inputs, and the store retains them outside the managed model rather than treating a sparse
// patch as a manifest it may mirror over or sweep away.
func patchFilesOf(kusts map[string]*kustomizationDoc) map[string]struct{} {
	out := map[string]struct{}{}
	for _, doc := range kusts {
		if doc.unsupported {
			continue // a refused kustomization's patches are not build context, they are the refusal
		}
		for _, path := range doc.patches {
			out[path] = struct{}{}
		}
	}
	return out
}
