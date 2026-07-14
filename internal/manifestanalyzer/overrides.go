// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
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

// overrideAssignment collects, per resource file, the distinct override chains
// the render roots reach it with. Exactly one distinct chain attaches its
// flattened overrides; more than one (with any overrides at stake) is the
// ambiguous case the store refuses to route.
type overrideAssignment struct {
	chainKeys    map[string]struct{}
	overrides    *KustomizeOverrides
	rendered     *RenderedOverrides
	anyOverrides bool
}

func (a *overrideAssignment) ambiguous() bool {
	return a != nil && len(a.chainKeys) > 1 && a.anyOverrides
}

// resolveOverrides returns the overrides to attach to a document in the given
// file, plus an ambiguity diagnostic when distinct chains with overrides at
// stake reach it. Attribution is purely structural (no API source needed), so it
// also works in structure-only analysis.
func resolveOverrides(
	loc manifestedit.Location,
	id manifestedit.Identity,
	assignments map[chainKey]*overrideAssignment,
) (*KustomizeOverrides, *RenderedOverrides, *manifestedit.Diagnostic) {
	a := assignments[chainKey{
		originPath: filepathToSlash(loc.Path),
		kind:       id.Kind,
		name:       id.Name,
	}]
	if a == nil {
		return nil, nil, nil
	}
	if a.ambiguous() {
		return nil, nil, &manifestedit.Diagnostic{
			Level:  manifestedit.DiagWarning,
			Reason: reasonAmbiguousOverrides,
			Message: "multiple render roots reach this file with different images/replicas override chains; " +
				"refusing to route edits through any of them",
			Path:          loc.Path,
			DocumentIndex: loc.DocumentIndex,
		}
	}
	return a.overrides, a.rendered, nil
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
