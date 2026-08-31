// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the post-scan layout resolution: what the folder's shape implies about where
// new documents go and whether they carry their own namespace, computed from a scan and from
// nothing else.
//
// It reports the ladder rung placement WILL take rather than the one it took, which is the
// whole point: placement only ever affects documents that do not exist yet, so nothing is
// observable by looking at the folder, and a user declaring a target against a real repository
// otherwise has no way to see what the operator would do until it has already done it. A
// suspended target plus this resolution is the dry run.
//
// Nothing here depends on a placement having happened. See
// docs/layout/model.md § "status.placement, and the post-scan pass".

// LayoutReason is the resolved shape of a GitTarget folder, and it is a condition reason on
// LayoutResolved rather than a status field: every consumer in this ecosystem already reads
// reasons from conditions, and a bespoke field would be a second place to look.
type LayoutReason string

const (
	// LayoutSingleKustomization is a folder governed by exactly one supported, writable
	// kustomization. A new document lands beside it and joins its resources:.
	LayoutSingleKustomization LayoutReason = "SingleKustomization"
	// LayoutAmbiguous is a folder covering more than one render root. Placement declines to
	// pick one rather than guessing, so the folder is not a write partition: point the target
	// at a leaf instead. See docs/layout/shapes/README.md § "Why only a leaf can be a
	// kustomize target".
	LayoutAmbiguous LayoutReason = "Ambiguous"
	// LayoutNone is a folder with no supported kustomization at all. New documents land at a
	// declared template's path, or at the built-in canonical path.
	LayoutNone LayoutReason = "None"
)

// LayoutResolution is what one scan resolved about a folder's layout.
type LayoutResolution struct {
	// Reason is the verdict, projected onto the LayoutResolved condition.
	Reason LayoutReason
	// RenderRoot is the governing kustomization's directory relative to the write scope, "."
	// for the scope itself. Empty for every reason but LayoutSingleKustomization.
	RenderRoot string
	// RenderRoots is every writable render root the scan found, sorted. It is what makes an
	// Ambiguous message able to name the folders it actually covers instead of only counting
	// them.
	RenderRoots []string
	// SerializeNamespace is whether a new document in this folder carries its own
	// metadata.namespace. Nil when the folder resolves no single answer — under Ambiguous the
	// question is decided per document, by whichever root governs it.
	SerializeNamespace *bool
	// Examples is where a representative object of a few types would land, capped by
	// layoutExampleCap. Illustrative, never a tally.
	Examples []LayoutExample
}

// LayoutExample is one illustrative destination.
type LayoutExample struct {
	// Type is the placement type key ("{group}/{version}/{resource}").
	Type string
	// Path is where a new object of that type would be written, relative to the write scope.
	Path string
	// Source is the ladder rung that produced Path.
	Source PlacementSource
}

// layoutExampleCap is the fixed size of the examples list. Three is enough to show a declared
// type, a sensitive type and the fallback, and a fixed cap is what keeps the stanza bounded
// however many types a target watches — the same reason status.streams is counts.
const layoutExampleCap = 3

// layoutExampleNamespace is the namespace the illustrative requests are resolved for. It has to
// be a real namespace rather than empty, because the namespace is what a "{namespace}" template
// renders and what the inherited-namespace rule compares against.
const layoutExampleNamespace = "example"

// ResolveLayout resolves a folder's layout from a scan.
//
// writeScope is the write jail relative to the scanned root — empty for a self-contained
// subtree, non-empty only when render-root scoping anchored the scan past spec.path into a base
// the folder renders. Read scope is wider than write scope, always, so a base above the jail is
// read to render the folder and is never a candidate root for it.
//
// exampleTypes are placement type keys to illustrate, in priority order; the first
// layoutExampleCap that resolve are kept. Passing none still produces one example for the
// fallback rung, because "where would an object with no byType entry go" is the question the
// ladder is mostly asked.
func ResolveLayout(
	store *ManifestStore,
	policy *PlacementPolicy,
	writeScope string,
	exampleTypes []string,
) LayoutResolution {
	roots := writableRenderRoots(store, writeScope)
	resolution := LayoutResolution{RenderRoots: roots}

	switch {
	case len(roots) == 0:
		resolution.Reason = LayoutNone
		// No kustomization governs anything here, so nothing downstream supplies a namespace
		// and every namespaced document has to carry its own.
		resolution.SerializeNamespace = boolPtr(true)
	case len(roots) == 1:
		resolution.Reason = LayoutSingleKustomization
		resolution.RenderRoot = relativeToScope(roots[0], writeScope)
		// The root's namespace: transformer is the supplier, exactly as
		// namespaceIsInheritedFromContext decides it per document: a root that sets one supplies
		// it, and a root that sets none leaves the document to carry its own.
		resolution.SerializeNamespace = boolPtr(store.Kustomizations[roots[0]].Namespace == "")
	default:
		resolution.Reason = LayoutAmbiguous
		// Deliberately nil: with several roots the answer differs per document, and asserting a
		// folder-wide one would be the same guess the Ambiguous verdict exists to refuse.
	}

	resolution.Examples = layoutExamples(store, policy, writeScope, exampleTypes)
	return resolution
}

// writableRenderRoots is the predicate resolveKustomizeRoot resolves against, lifted out so the
// reported layout and the taken layout cannot drift: LayoutResolved must describe the rung
// placement WILL take, and the only way to guarantee that is for both to ask one function.
//
// A root is a supported kustomization inside the write jail. Under render-root scoping the scan
// also holds the read-only bases the folder renders; those are not candidates, which is what
// lets an overlay resolve to its own single root instead of counting its base as a second one.
func writableRenderRoots(store *ManifestStore, writeScope string) []string {
	roots := make([]string, 0, len(store.Kustomizations))
	for dir, k := range store.Kustomizations {
		if k.Unsupported {
			continue
		}
		if writeScope != "" && !pathWithin(slashDir(k.Path), writeScope) {
			continue
		}
		roots = append(roots, dir)
	}
	sort.Strings(roots)
	return roots
}

// relativeToScope expresses a scanned-root-relative directory relative to the write jail, so a
// reported renderRoot is relative to spec.path exactly as placement paths are documented to be.
func relativeToScope(dir, writeScope string) string {
	if writeScope == "" {
		return orDot(dir)
	}
	if dir == writeScope {
		return "."
	}
	return orDot(trimScopePrefix(dir, writeScope))
}

func trimScopePrefix(dir, writeScope string) string {
	prefix := writeScope + "/"
	if len(dir) > len(prefix) && dir[:len(prefix)] == prefix {
		return dir[len(prefix):]
	}
	return dir
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// layoutExamples resolves the illustrative destinations through the real ladder — LocateNew,
// the same call the writer makes — so an example can never claim a destination the writer would
// not choose. A type whose placement is refused (a sensitive type onto an occupied path, say)
// contributes no example rather than a wrong one.
func layoutExamples(
	store *ManifestStore,
	policy *PlacementPolicy,
	writeScope string,
	exampleTypes []string,
) []LayoutExample {
	requested := exampleTypes
	if len(requested) == 0 {
		requested = []string{PlacementTypeKey("", "v1", "configmaps")}
	}

	examples := make([]LayoutExample, 0, layoutExampleCap)
	for _, key := range requested {
		if len(examples) == layoutExampleCap {
			break
		}
		id, ok := ParsePlacementTypeKey(key)
		if !ok {
			continue
		}
		req := PlacementRequest{
			Identifier: types.NewResourceIdentifier(
				id.Group, id.Version, id.Resource, layoutExampleNamespace, "example"),
			WriteScope: writeScope,
		}
		result, err := LocateNew(store, policy, req)
		if err != nil {
			continue
		}
		examples = append(examples, LayoutExample{
			Type:   key,
			Path:   relativeToScope(path.Clean(result.Path), writeScope),
			Source: result.Source,
		})
	}
	return examples
}

// ParsePlacementTypeKey is the inverse of PlacementTypeKey: it reads a
// "{group}/{version}/{resource}" key, or a core "{version}/{resource}" one, back into its
// parts. The second return is false for anything else, so a malformed byType key illustrates
// nothing rather than illustrating a type nobody named.
func ParsePlacementTypeKey(key string) (schema.GroupVersionResource, bool) {
	// A core key is "{version}/{resource}"; a grouped one is "{group}/{version}/{resource}".
	const coreParts, groupedParts = 2, 3
	switch parts := strings.Split(strings.TrimSpace(key), "/"); len(parts) {
	case coreParts:
		if parts[0] == "" || parts[1] == "" {
			return schema.GroupVersionResource{}, false
		}
		return schema.GroupVersionResource{Version: parts[0], Resource: parts[1]}, true
	case groupedParts:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return schema.GroupVersionResource{}, false
		}
		return schema.GroupVersionResource{Group: parts[0], Version: parts[1], Resource: parts[2]}, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

func boolPtr(v bool) *bool { return &v }

// IssueAmbiguousLayout marks a GitTarget folder covering more than one render root. It is a
// property of the OBSERVED folder rather than of the spec, so no CEL rule and no admission
// check can reach it — the folder is only ambiguous once the operator has read it.
const IssueAmbiguousLayout IssueKind = "ambiguous-layout"

// AmbiguousLayoutRefusal is the write-path form of the Ambiguous verdict: a folder covering
// several render roots is refused rather than silently placed into whichever one an arbitrary
// rule picks.
//
// It refuses at the WRITE rather than at the GitTarget's Validated gate, and the difference is
// recoverability. Validated is evaluated before the data plane exists, so a target failing it
// never registers a worker, never scans, and could therefore never observe that the folder had
// been fixed — the refusal would be permanent, and for a target that had never scanned it could
// never fire in the first place. Refusing here keeps the target declared and scanning: the
// periodic resync re-reads the folder, so splitting the target down to a leaf overlay clears the
// refusal the same way fixing any other unsupported content does.
//
// It returns no issue for a folder that is not ambiguous, so the caller can raise it
// unconditionally.
func AmbiguousLayoutRefusal(resolution LayoutResolution, specPath string) []AcceptanceIssue {
	if resolution.Reason != LayoutAmbiguous {
		return nil
	}
	return []AcceptanceIssue{{
		Kind: IssueAmbiguousLayout,
		Path: orDot(specPath),
		Message: fmt.Sprintf(
			"%s covers %d kustomize render roots (%s), so there is no single one to place new "+
				"documents into; point the GitTarget at one of them instead",
			orDot(specPath), len(resolution.RenderRoots), strings.Join(resolution.RenderRoots, ", ")),
		// The repository author fixes it, and one GitTarget edit does: it is a scoping mistake,
		// not a support boundary. See docs/layout/shapes/README.md, "Why only a leaf can be a
		// kustomize target".
		Solvable: true,
		Actor:    ActorRepositoryAuthor,
	}}
}
