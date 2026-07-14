// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"reflect"
	"sort"
	"strings"

	kustypes "sigs.k8s.io/kustomize/api/types"

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

		// Tolerated exactly as before this change, so it stays a refactor. These
		// inject metadata into rendered objects and therefore leak into mirrored
		// source files as drift — a known defect, tracked separately, deliberately
		// not altered here.
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

// parseKustomization decodes one kustomization.yaml and reports every feature the
// operator does not model, sorted. An empty slice means the file is fully modelled.
// The doc is returned even when unsupported: callers keep it (so it never acts as a
// namespace source) rather than dropping it.
func parseKustomization(content []byte, path string) (*kustomizationDoc, []string) {
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
		return doc, []string{featureUnparseable}
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

	out := make([]string, 0, len(features))
	for f := range features {
		out = append(out, f)
	}
	sort.Strings(out)
	doc.unsupported = len(out) > 0
	return doc, out
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
func parseKustomizations(files []manifestedit.FileContent) map[string]*kustomizationDoc {
	out := map[string]*kustomizationDoc{}
	for _, f := range files {
		if !isKustomizationFile(f.Path) {
			continue
		}
		doc, _ := parseKustomization(f.Content, filepathToSlash(f.Path))
		out[slashDir(f.Path)] = doc
	}
	return out
}

// kustomizationUsesUnsupportedFeature reports whether a kustomization.yaml uses a
// feature outside the modelled subset — the predicate the acceptance gate uses to
// refuse the folder at the retention site.
func kustomizationUsesUnsupportedFeature(content []byte) bool {
	_, features := parseKustomization(content, "")
	return len(features) > 0
}

// unsupportedKustomizeFeatures names the features a kustomization declares that the
// operator does not model, for the repo scan's per-candidate refusal detail. It is
// the same parse the acceptance gate runs, so the two cannot drift.
func unsupportedKustomizeFeatures(content []byte) []string {
	_, features := parseKustomization(content, "")
	return features
}
