// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"math"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file implements the model half of the kustomize images/replicas
// edit-through): parsing the two supported override transformers out of
// kustomization.yaml and attributing an unambiguous override chain to every
// resource file a render root reaches. The write-side projection consumes the
// attached KustomizeOverrides. See
// docs/design/support-boundary/finished/images-and-replicas-edit-through.md.

// reasonAmbiguousOverrides marks a build-time diagnostic for a resource file that
// more than one render root reaches with differing override chains. The store
// attaches no overrides in that case — the writer falls back to plain in-place
// patching (today's write-through) rather than guessing which chain governs.
const reasonAmbiguousOverrides manifestedit.DiagReason = "ambiguous-kustomize-overrides"

// ImageOverride is one parsed images: entry, carrying the kustomization file it
// came from so the writer knows which file to edit. The Has* booleans record key
// presence: the writer only ever updates a field the entry already declares.
type ImageOverride struct {
	// Source is the kustomization file path (slash) that declares the entry.
	Source string
	// Index is the entry's position within its file's images: sequence, so the
	// writer can pin the exact entry even when two entries share a name.
	Index int
	// Name matches an image whose name equals it at that point in the build chain.
	Name string
	// NewName / NewTag / Digest replace the matched image's components; each is
	// meaningful only when its Has* flag is set.
	NewName string
	NewTag  string
	Digest  string
	// HasNewName / HasNewTag / HasDigest record which keys the entry declares.
	HasNewName bool
	HasNewTag  bool
	HasDigest  bool
}

// ReplicaOverride is one parsed replicas: entry, carrying its source
// kustomization file. It applies to spec.replicas of a Deployment, ReplicaSet, or
// StatefulSet whose metadata.name equals Name.
type ReplicaOverride struct {
	// Source is the kustomization file path (slash) that declares the entry.
	Source string
	// Index is the entry's position within its file's replicas: sequence.
	Index int
	// Name matches the target document's metadata.name.
	Name string
	// Count is the replica count the entry pins.
	Count int64
}

// KustomizeOverrides is the flattened, unambiguous override chain governing a
// document: every images:/replicas: entry from the kustomizations along the
// single reference path root→file, in build order (innermost kustomization's
// entries first — kustomize renders bases before applying a parent's
// transformers). Nil on a DocumentModel means no chain, or an ambiguous one.
type KustomizeOverrides struct {
	Images   []ImageOverride
	Replicas []ReplicaOverride
}

// hasOnlyKeys reports whether the entry map uses only the exact key set
// kustomize's typed entry accepts. An unknown key means we can no longer vouch
// for the render (kustomize itself rejects it), so the kustomization is refused
// as malformed rather than silently misunderstood.
func hasOnlyKeys(m map[string]interface{}, keys ...string) bool {
	for k := range m {
		known := false
		for _, want := range keys {
			if k == want {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

// parseImageOverrides parses the images: field of a kustomization. ok is false
// when the field is present but not structurally sound (not a list of maps, a
// missing/empty name, a non-string or empty component, an unknown key) — the
// caller marks the kustomization unsupported, because a folder we cannot parse is
// a folder we cannot claim to understand.
func parseImageOverrides(raw map[string]interface{}, source string) ([]ImageOverride, bool) {
	v, present := raw["images"]
	if !present || isEmptyValue(v) {
		return nil, true
	}
	list, isList := v.([]interface{})
	if !isList {
		return nil, false
	}
	out := make([]ImageOverride, 0, len(list))
	for i, item := range list {
		m, isMap := item.(map[string]interface{})
		if !isMap || !hasOnlyKeys(m, "name", "newName", "newTag", "digest") {
			return nil, false
		}
		entry := ImageOverride{Source: source, Index: i}
		var ok bool
		if entry.Name, ok = requiredString(m, "name"); !ok {
			return nil, false
		}
		if entry.NewName, entry.HasNewName, ok = optionalString(m, "newName"); !ok {
			return nil, false
		}
		if entry.NewTag, entry.HasNewTag, ok = optionalString(m, "newTag"); !ok {
			return nil, false
		}
		if entry.Digest, entry.HasDigest, ok = optionalString(m, "digest"); !ok {
			return nil, false
		}
		out = append(out, entry)
	}
	return out, true
}

// parseReplicaOverrides parses the replicas: field of a kustomization. ok is
// false when the field is present but malformed (see parseImageOverrides).
func parseReplicaOverrides(raw map[string]interface{}, source string) ([]ReplicaOverride, bool) {
	v, present := raw["replicas"]
	if !present || isEmptyValue(v) {
		return nil, true
	}
	list, isList := v.([]interface{})
	if !isList {
		return nil, false
	}
	out := make([]ReplicaOverride, 0, len(list))
	for i, item := range list {
		m, isMap := item.(map[string]interface{})
		if !isMap || !hasOnlyKeys(m, "name", "count") {
			return nil, false
		}
		entry := ReplicaOverride{Source: source, Index: i}
		var ok bool
		if entry.Name, ok = requiredString(m, "name"); !ok {
			return nil, false
		}
		if entry.Count, ok = integerField(m, "count"); !ok || entry.Count < 0 {
			return nil, false
		}
		out = append(out, entry)
	}
	return out, true
}

func requiredString(m map[string]interface{}, key string) (string, bool) {
	s, isStr := m[key].(string)
	if !isStr || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// optionalString reads an optional string key, returning (value, present, ok):
// present reports whether the key exists, ok is false for a non-string or blank
// value (declared-but-empty is a broken transform, not an unset one).
func optionalString(m map[string]interface{}, key string) (string, bool, bool) {
	v, exists := m[key]
	if !exists {
		return "", false, true
	}
	s, isStr := v.(string)
	if !isStr || strings.TrimSpace(s) == "" {
		return "", false, false
	}
	return s, true, true
}

// integerField reads a whole-number field. sigs.k8s.io/yaml decodes YAML numbers
// as float64 (via JSON), so an integral float is accepted; anything else is not.
func integerField(m map[string]interface{}, key string) (int64, bool) {
	switch n := m[key].(type) {
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

// overrideAssignment collects, per resource file, the distinct override chains
// the render roots reach it with. Exactly one distinct chain attaches its
// flattened overrides; more than one (with any overrides at stake) is the
// ambiguous case the store refuses to route.
type overrideAssignment struct {
	chainKeys    map[string]struct{}
	overrides    *KustomizeOverrides
	anyOverrides bool
}

func (a *overrideAssignment) ambiguous() bool {
	return a != nil && len(a.chainKeys) > 1 && a.anyOverrides
}

// kustomizeOverrideAssignments walks every render root — a kustomization no other
// kustomization references, i.e. the directory a human would `kustomize build` —
// and records, per resource file, the kustomization chain along the reference
// path. Unlike the namespace walk (which treats every namespace-bearing
// kustomization as a root and refuses parent/child conflicts), a referenced base
// is not a root here: its transformers compose with its parent's, innermost
// first, exactly as kustomize applies them.
//
// Cycle protection is on the CURRENT PATH, not per walk: a diamond (one root
// reaching a shared base through two overlays) must record both paths so their
// differing chains trip the ambiguity refusal, while a true reference cycle
// still terminates. Real kustomize rejects the diamond outright (duplicate
// resources), so ambiguity — never silent first-path attribution — is the
// honest outcome.
func kustomizeOverrideAssignments(
	kusts map[string]*kustomizationDoc,
	resourceFiles map[string]struct{},
) map[string]*overrideAssignment {
	out := map[string]*overrideAssignment{}
	for _, rootDir := range renderRoots(kusts) {
		root := kusts[rootDir]
		if root == nil || root.unsupported {
			continue
		}
		onPath := map[string]struct{}{}
		var stack []*kustomizationDoc
		var walk func(dir string, cur *kustomizationDoc)
		walk = func(dir string, cur *kustomizationDoc) {
			if cur == nil || cur.unsupported {
				return
			}
			if _, cycling := onPath[dir]; cycling {
				return
			}
			onPath[dir] = struct{}{}
			stack = append(stack, cur)
			for _, entry := range cur.resources {
				target := cleanJoin(dir, entry)
				switch {
				case target == "":
					// empty, or escapes the scanned root: contributes no chain.
				case mapHasKey(resourceFiles, target):
					recordOverrideChain(out, target, stack)
				default:
					walk(target, kusts[target])
				}
			}
			stack = stack[:len(stack)-1]
			delete(onPath, dir)
		}
		walk(rootDir, root)
	}
	return out
}

// renderRoots returns the kustomization directories no other kustomization in
// the subtree references — the directories a build would be invoked on — in
// sorted order for deterministic walks.
func renderRoots(kusts map[string]*kustomizationDoc) []string {
	referenced := map[string]struct{}{}
	for dir, k := range kusts {
		for _, entry := range k.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, ok := kusts[target]; ok {
				referenced[target] = struct{}{}
			}
		}
	}
	roots := make([]string, 0, len(kusts))
	for dir := range kusts {
		if _, ok := referenced[dir]; !ok {
			roots = append(roots, dir)
		}
	}
	sort.Strings(roots)
	return roots
}

// recordOverrideChain records one root→file chain. The walk descends root-first,
// so the stack is outermost-first; build order (innermost kustomization's
// transformers first) is its reverse.
func recordOverrideChain(out map[string]*overrideAssignment, file string, stack []*kustomizationDoc) {
	chain := make([]*kustomizationDoc, len(stack))
	for i, k := range stack {
		chain[len(stack)-1-i] = k
	}
	keys := make([]string, len(chain))
	for i, k := range chain {
		keys[i] = k.path
	}
	key := strings.Join(keys, "\x00")

	a := out[file]
	if a == nil {
		a = &overrideAssignment{chainKeys: map[string]struct{}{}}
		out[file] = a
	}
	flat := flattenOverrideChain(chain)
	if _, seen := a.chainKeys[key]; !seen {
		a.chainKeys[key] = struct{}{}
		if a.overrides == nil {
			a.overrides = flat
		}
	}
	if flat != nil {
		a.anyOverrides = true
	}
}

// flattenOverrideChain concatenates the chain's entries in build order. Nil when
// no kustomization in the chain declares any override.
func flattenOverrideChain(chain []*kustomizationDoc) *KustomizeOverrides {
	var ov KustomizeOverrides
	for _, k := range chain {
		ov.Images = append(ov.Images, k.images...)
		ov.Replicas = append(ov.Replicas, k.replicas...)
	}
	if len(ov.Images) == 0 && len(ov.Replicas) == 0 {
		return nil
	}
	return &ov
}

// resolveOverrides returns the overrides to attach to a document in the given
// file, plus an ambiguity diagnostic when distinct chains with overrides at
// stake reach it. Attribution is purely structural (no API source needed), so it
// also works in structure-only analysis.
func resolveOverrides(
	loc manifestedit.Location,
	assignments map[string]*overrideAssignment,
) (*KustomizeOverrides, *manifestedit.Diagnostic) {
	a := assignments[filepathToSlash(loc.Path)]
	if a == nil {
		return nil, nil
	}
	if a.ambiguous() {
		return nil, &manifestedit.Diagnostic{
			Level:  manifestedit.DiagWarning,
			Reason: reasonAmbiguousOverrides,
			Message: "multiple render roots reach this file with different images/replicas override chains; " +
				"refusing to route edits through any of them",
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		}
	}
	return a.overrides, nil
}

// OverridesAmbiguousAt reports whether the store refused to route a kustomize override chain
// for a document in the file at the given base-relative (slash) path, because more than one
// render path reaches it with override entries at stake (reasonAmbiguousOverrides). It is the
// store-side signal for the writer's write-fan-in precondition: editing such a file in place
// would write a live change through into source context shared by multiple render roots — the
// one edit the write-fan-in = 1 invariant forbids — so the flush is refused rather than
// corrupting what another root renders. Derived from the build-time diagnostics the store
// already carries, so it needs no extra per-file state.
func (s *ManifestStore) OverridesAmbiguousAt(rel string) bool {
	want := filepathToSlash(rel)
	for i := range s.Diagnostics {
		d := s.Diagnostics[i]
		if d.Reason == reasonAmbiguousOverrides && filepathToSlash(d.Path) == want {
			return true
		}
	}
	return false
}
