// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// TOLERATING A PATCH IS NOT AUTHORING ONE, and these are the two halves of that sentence.
//
// Tolerate: the folder is accepted, the render is mirrored, and the patch file is read-only build
// context — never a manifest, never mirrored over, never swept.
//
// Author: still refused. Nothing is routed INTO a patch, so an edit to a field a patch owns has
// nowhere to land, and the re-render refuses the flush rather than writing a value the build will
// override straight back.

const simplePatchYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 4
`

// patchedTree is the shape this milestone accepts: one base, one strategic-merge patch named by
// path, nothing else.
func patchedTree(kustomization string) []manifestedit.FileContent {
	return []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("ghcr.io/example/app:1.0.0", "1")),
		file("patch.yaml", simplePatchYAML),
		file("kustomization.yaml", kustomization),
	}
}

func featuresOf(t *testing.T, files []manifestedit.FileContent) []string {
	t.Helper()
	doc := parseKustomizations(files)["."]
	require.NotNil(t, doc, "the fixture must carry a kustomization at its root")
	return doc.features
}

// The one shape we tolerate: a path to a sparse KRM document inside the tree.
func TestPatches_ASimpleStrategicMergePatchIsTolerated(t *testing.T) {
	files := patchedTree("resources:\n  - deployment.yaml\npatches:\n  - path: patch.yaml\n")

	require.Empty(t, featuresOf(t, files), "a patch by path is read-only context, not a refusal")

	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())
	require.True(t, AcceptStructureOnly(store).Accepted, "and the folder is adopted")
}

// Every other shape is refused BY NAME. "unsupported" on its own is not something a user can act
// on; "your patch is inline, and we can only read one from a file" is.
func TestPatches_EveryOtherShapeIsRefusedByName(t *testing.T) {
	cases := []struct {
		name    string
		files   []manifestedit.FileContent
		refusal string
	}{{
		name: "inline strategic-merge patch",
		files: patchedTree(`resources:
  - deployment.yaml
patches:
  - patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: web
      spec:
        replicas: 4
`),
		refusal: featurePatchInline,
	}, {
		// A JSON6902 patch hides inside the SAME key as a strategic merge, and decodes into the
		// same struct. Refusing an inline `patch:` outright is what catches it.
		name: "inline JSON6902 op list",
		files: patchedTree(`resources:
  - deployment.yaml
patches:
  - target:
      kind: Deployment
      name: web
    patch: |-
      - op: replace
        path: /spec/replicas
        value: 4
`),
		refusal: featurePatchInline,
	}, {
		// And by path it is a YAML SEQUENCE, not a KRM document — so it carries no apiVersion and
		// no kind, and it would otherwise be indexed as a broken manifest.
		name: "JSON6902 op list by path",
		files: []manifestedit.FileContent{
			file("deployment.yaml", deploymentSource("ghcr.io/example/app:1.0.0", "1")),
			file("patch.yaml", "- op: replace\n  path: /spec/replicas\n  value: 4\n"),
			file("kustomization.yaml", `resources:
  - deployment.yaml
patches:
  - path: patch.yaml
    target:
      kind: Deployment
      name: web
`),
		},
		refusal: featurePatchJSON6902,
	}, {
		name:    "a path naming no file in the tree",
		files:   patchedTree("resources:\n  - deployment.yaml\npatches:\n  - path: nowhere.yaml\n"),
		refusal: featurePatchOutsideTree,
	}, {
		name:    "a path escaping the scanned tree",
		files:   patchedTree("resources:\n  - deployment.yaml\npatches:\n  - path: ../shared/patch.yaml\n"),
		refusal: featurePatchOutsideTree,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, []string{tc.refusal}, featuresOf(t, tc.files))

			store := BuildStoreFromFiles(context.Background(), tc.files, nil, WriterAllowlist())
			require.False(t, AcceptStructureOnly(store).Accepted, "and the folder is refused")
		})
	}
}

// MEASURED, NOT ASSUMED. FixKustomization folds `bases` into `resources` and `imageTags` into
// `images` — so it is easy to believe it folds the deprecated patch spellings into `patches` too,
// and to then tolerate them by accident. It does not: they stay in their own fields, land outside
// the modelled set, and refuse the folder under their own names.
//
// This is the test that fails if a kustomize bump ever starts folding them, which would otherwise
// silently widen what we accept to shapes we have never looked at.
func TestPatches_DeprecatedSpellingsAreNotFoldedIntoPatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		refusal string
	}{{
		name:    "patchesStrategicMerge",
		body:    "patchesStrategicMerge:\n  - patch.yaml\n",
		refusal: "patchesStrategicMerge",
	}, {
		name: "patchesJson6902",
		body: `patchesJson6902:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: web
    path: patch.yaml
`,
		refusal: "patchesJson6902",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			files := patchedTree("resources:\n  - deployment.yaml\n" + tc.body)
			require.Equal(t, []string{tc.refusal}, featuresOf(t, files),
				"the deprecated spelling must refuse under its OWN name, not arrive disguised as patches:")
		})
	}
}

// A PATCH FILE IS A BUILD INPUT, NOT A MANIFEST.
//
// It is a KRM document — apiVersion, kind, metadata.name — so every other part of the store would
// happily treat it as one: index it, match a live object to it, mirror a whole Deployment over the
// sparse patch, or sweep it away as an orphan nothing in the cluster answers to. It is retained
// instead, exactly as kustomization.yaml is.
func TestPatches_ThePatchFileIsRetainedNotManaged(t *testing.T) {
	files := patchedTree("resources:\n  - deployment.yaml\npatches:\n  - path: patch.yaml\n")
	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())

	require.NotContains(t, store.FilesByPath, "patch.yaml",
		"a managed patch file is a live object waiting to be written over it")
	require.Contains(t, store.FilesByPath, "deployment.yaml", "the base is still managed")

	var retained []string
	for _, rd := range store.Retained {
		retained = append(retained, rd.Location.Path)
		require.Empty(t, rd.Identity.Name,
			"a patch must not be retained WITH an identity: that is the mixed-file refusal, "+
				"and it would refuse the folder for containing exactly what it should")
	}
	require.ElementsMatch(t, []string{"kustomization.yaml", "patch.yaml"}, retained)
}

// And that a patch is not a resource is not OUR claim — it is the RENDER's. kustomize produces one
// object here, from the base; the patch file never appears as an origin. If that ever stopped
// being true, retaining it would be hiding a resource.
func TestPatches_ThePatchFileIsNeverARenderOrigin(t *testing.T) {
	files := patchedTree("resources:\n  - deployment.yaml\npatches:\n  - path: patch.yaml\n")

	rendered, err := renderRoot(files, ".")
	require.NoError(t, err)
	require.Len(t, rendered, 1)
	require.Equal(t, "deployment.yaml", rendered[0].OriginPath)
}

// A duplicate of the base, dressed as a patch, is still refused — retention follows the
// kustomization's `patches:` entries, not a filename convention, so nothing can smuggle a manifest
// out of the managed set by being called a patch.
func TestPatches_AnUnreferencedPatchLookingFileIsStillAManifest(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("ghcr.io/example/app:1.0.0", "1")),
		file("patch.yaml", simplePatchYAML), // named by nothing
		file("kustomization.yaml", "resources:\n  - deployment.yaml\n"),
	}
	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())

	require.Contains(t, store.FilesByPath, "patch.yaml",
		"nothing reads this file as a patch, so it is what it looks like: a manifest")
	require.False(t, AcceptStructureOnly(store).Accepted,
		"and it duplicates the base's identity, which is refused")
}
