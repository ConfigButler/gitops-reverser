// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"regexp"
	"sort"

	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// The dye: which override entry supplied this value?
//
// A render does not say. `kustomize build` returns `web:2.0`; it never says where the `2.0`
// came from, and there is no field-level provenance anywhere in kustomize, at any visibility
// level — the image filter rewrites the field and records nothing. So attribution cannot be
// READ out of kustomize. It can only be inferred by QUESTIONING it: perturb an input, render,
// see what moves.
//
// The obvious perturbation — remove an entry and see what changes — is measurably wrong, and
// wrong on the most ordinary configuration there is. A base at `app:v1` under an overlay
// declaring `newTag: v1` is the state every repo is in the moment a release lands in both
// places; remove the entry and NOTHING MOVES, so removal concludes the source file supplies
// the tag. Write the user's next tag into the base, and the overlay overrides it straight back
// on every reconcile, forever. The cause is structural: removal probes the VALUE, and values
// collide.
//
// The dye is that same idea with the flaw removed. Write a unique nonce into every override
// entry, render ONCE, and read the nonces off the output: wherever a dye lands, THAT entry
// supplied THAT field. Absence is indistinguishable from "someone else wrote the same value";
// a nonce nothing else can produce is not. Ties resolve, the idempotent pin resolves, and the
// cost is one extra build per root rather than one per entry.
//
// See docs/design/support-boundary/render-attribution.md §3.

// The nonce alphabet is not a style choice, it is a CORRECTNESS REQUIREMENT, and it is the
// one thing here that will silently ruin a render if it is got wrong.
//
// kustomize does not validate newTag or digest at all — a 200-character tag renders straight
// through. But it MATCHES on the image with a regex over the whole string
// (api/internal/image/image.go):
//
//	"^" + name + "(:[a-zA-Z0-9_.{}-]*)?(@sha256:[a-zA-Z0-9_.{}-]*)?$"
//
// so a dye outside that charset leaves the image un-matchable and EVERY LATER ENTRY SILENTLY
// STOPS FIRING. No error; a different render. Measured: `newTag: zz/probe` renders, and kills
// the next entry. A digest without the mandatory `sha256:` prefix does the same.
const (
	// dyeTagPrefix keeps tag nonces inside [a-zA-Z0-9_.{}-].
	dyeTagPrefix = "grdye-t-"
	// dyeNamePrefix is for newName. A name is not matched by the tag charset — it is the
	// regex's literal prefix — but it must still be a plain image name: no ':' (a tag
	// separator), no '@' (a digest separator), no '/' (a registry separator).
	dyeNamePrefix = "grdye-n-"
	// dyeDigestPrefix carries the sha256: the regex REQUIRES, and stays alphanumeric after it.
	dyeDigestPrefix = "sha256:grdye"
	// dyeReplicaBase is a reserved count. Measured: it renders through untouched, and no
	// real deployment has two billion replicas.
	dyeReplicaBase = int64(2_000_000_000)
)

// dyeMark says what a nonce was minted for, so a dye read out of the tag position can be
// required to BE a tag dye rather than merely a nonce that happens to have landed there.
type dyeMark struct {
	image   *ImageOverride
	replica *ReplicaOverride
	field   string
}

// dyePlan is the counterfactual: the dyed kustomizations to render, and the table that reads
// the nonces back. One plan covers the whole scan, so it is built once and reused for every
// render root.
type dyePlan struct {
	// replace is the dyed kustomization content, by slash path — the overlay handed to
	// renderRootWith.
	replace map[string][]byte
	// byNonce maps a nonce back to the entry that carries it.
	byNonce map[string]dyeMark
	// byReplicaCount does the same for the reserved integer counts.
	byReplicaCount map[int64]dyeMark
	// namesDyed records whether newName was dyed. When a rename chain exists, it is not —
	// see dyeingNamesIsSafe — and a name change is then simply unattributable, which means
	// no entry edit, which means the oracle adjudicates whatever the source file can carry.
	namesDyed bool
}

// planDye mints a nonce for every override entry field that is DECLARED, and re-serialises
// each kustomization with the nonces in place.
//
// "Declared" matters: injecting a newTag into a newName-only entry would fabricate a supplier
// that does not exist, and the projection would then route a tag edit to an entry that never
// set a tag.
//
// The dye is applied to kustomize's own typed Kustomization and re-marshalled, exactly as
// withBuildMetadata already does, so YAML quoting is the encoder's problem rather than ours.
// It has to be: `newTag: y` is a YAML BOOLEAN, and kustomize's own Unmarshal rejects it.
func planDye(files []manifestedit.FileContent) *dyePlan {
	plan := &dyePlan{
		replace:        map[string][]byte{},
		byNonce:        map[string]dyeMark{},
		byReplicaCount: map[int64]dyeMark{},
		namesDyed:      dyeingNamesIsSafe(allImageOverrides(files)),
	}

	next := 0
	mint := func() int { next++; return next }

	for _, f := range sortedKustomizationFiles(files) {
		var k kustypes.Kustomization
		if err := k.Unmarshal(f.Content); err != nil {
			continue // an unparseable kustomization refuses the folder elsewhere; nothing to dye
		}
		k.FixKustomization()
		path := filepathToSlash(f.Path)

		dyed := plan.dyeImages(&k, path, mint)
		dyed = plan.dyeReplicas(&k, path, mint) || dyed
		if !dyed {
			continue
		}
		content, err := yaml.Marshal(&k)
		if err != nil {
			continue // cannot dye this file; its entries simply go unattributed
		}
		plan.replace[path] = content
	}
	return plan
}

// dyeImages replaces every DECLARED images: field with a nonce, in place, and records what each
// nonce was minted for. It reports whether anything was dyed.
func (p *dyePlan) dyeImages(k *kustypes.Kustomization, path string, mint func() int) bool {
	entries, ok := imageOverrides(k.Images, path)
	if !ok || len(entries) != len(k.Images) {
		return false
	}
	dyed := false
	for i := range k.Images {
		entry := &entries[i]
		if entry.HasNewTag {
			nonce := fmt.Sprintf("%s%04d", dyeTagPrefix, mint())
			p.byNonce[nonce] = dyeMark{image: entry, field: fieldNewTag}
			k.Images[i].NewTag = nonce
			dyed = true
		}
		if entry.HasDigest {
			nonce := fmt.Sprintf("%s%04d", dyeDigestPrefix, mint())
			p.byNonce[nonce] = dyeMark{image: entry, field: fieldDigest}
			k.Images[i].Digest = nonce
			dyed = true
		}
		if entry.HasNewName && p.namesDyed {
			nonce := fmt.Sprintf("%s%04d", dyeNamePrefix, mint())
			p.byNonce[nonce] = dyeMark{image: entry, field: fieldNewName}
			k.Images[i].NewName = nonce
			dyed = true
		}
	}
	return dyed
}

// dyeReplicas replaces every replicas: count with a reserved integer, in place.
func (p *dyePlan) dyeReplicas(k *kustypes.Kustomization, path string, mint func() int) bool {
	entries, ok := replicaOverrides(k.Replicas, path)
	if !ok || len(entries) != len(k.Replicas) {
		return false
	}
	for i := range k.Replicas {
		count := dyeReplicaBase + int64(mint())
		p.byReplicaCount[count] = dyeMark{replica: &entries[i], field: fieldCount}
		k.Replicas[i].Count = count
	}
	return len(k.Replicas) > 0
}

// The entry fields the projection can write, and the dye can therefore attribute.
const (
	fieldNewName = "newName"
	fieldNewTag  = "newTag"
	fieldDigest  = "digest"
	fieldCount   = "count"
)

// dyeingNamesIsSafe reports whether dyeing newName can be trusted in this tree.
//
// A dye is sound exactly when the dyed field is a PURE SINK — never an input to a matcher.
// newTag, digest and replicas[].count are sinks: nothing selects on them. newName IS NOT. It
// is the join key for every later entry:
//
//	images: [{name: app, newName: renamed}, {name: renamed, newTag: "4.0"}]
//	undyed        -> renamed:4.0
//	newName dyed  -> grdye-n-0001:v1     # entry 2 stopped matching. the render changed shape.
//
// The condition is exact and needs no model of kustomize's matching: dyeing a newName can only
// change a matching decision if some OTHER entry's name: matches it. And "matches" is asked of
// kustomize's own compiled pattern rather than of string equality, because an entry's name is a
// REGULAR EXPRESSION — `name: "rename."` matches `renamed` without being equal to it.
//
// Where no rename chain exists — essentially every real repository — names are dyed. Where one
// does, they are not, and a name change is simply not attributed. That is the correct fallback:
// no attribution, never a guess.
func dyeingNamesIsSafe(entries []ImageOverride) bool {
	for i := range entries {
		if !entries[i].HasNewName {
			continue
		}
		for j := range entries {
			if i == j {
				continue
			}
			pattern, err := regexp.Compile(imageNamePattern(entries[j].Name))
			if err != nil {
				return false // an uncompilable name refuses the folder anyway; do not dye into it
			}
			if pattern.MatchString(entries[i].NewName) {
				return false
			}
		}
	}
	return true
}

// allImageOverrides is every images: entry in the scan, which is the scope the rename-chain
// guard is asked over. Scoping it per render root would dye more, and the entries a root
// cannot reach cost nothing to be conservative about.
func allImageOverrides(files []manifestedit.FileContent) []ImageOverride {
	var out []ImageOverride
	for _, f := range sortedKustomizationFiles(files) {
		var k kustypes.Kustomization
		if err := k.Unmarshal(f.Content); err != nil {
			continue
		}
		k.FixKustomization()
		if entries, ok := imageOverrides(k.Images, filepathToSlash(f.Path)); ok {
			out = append(out, entries...)
		}
	}
	return out
}

// sortedKustomizationFiles keeps nonce minting deterministic: the same tree always produces
// the same dyes, so a render is reproducible and a diff of two runs is empty.
func sortedKustomizationFiles(files []manifestedit.FileContent) []manifestedit.FileContent {
	var out []manifestedit.FileContent
	for _, f := range files {
		if isKustomizationFile(f.Path) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
