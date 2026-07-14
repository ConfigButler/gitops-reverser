// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// The layout corpus barely exercises images:, so on its own it cannot license
// deleting our re-implemented image transformer. This table does: it drives the
// cases the re-implementation actually has to get right — chained renames, digest
// precedence, a registry port that looks like a tag — through BOTH kustomize and
// renderImage, and requires them to agree byte for byte.
//
// Every row is a claim about kustomize's semantics that our code depends on. If
// kustomize ever changes one, this test fails instead of the operator silently
// writing a file that renders to something else.

func TestRenderImage_MatchesKustomizeOnTheHardCases(t *testing.T) {
	cases := []struct {
		name   string
		source string // the container image in the source Deployment
		images string // the images: block of the overlay kustomization
	}{
		{
			name:   "newTag only",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newTag: \"2.0\"\n",
		},
		{
			name:   "newName only",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newName: ghcr.io/org/web-hardened\n",
		},
		{
			name:   "newName and newTag in one entry",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newName: ghcr.io/org/hardened\n    newTag: \"3.0\"\n",
		},
		{
			name:   "digest replaces the tag",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    digest: sha256:abc123\n",
		},
		{
			// kustomize's own doc on types.Image: "If digest is present NewTag value
			// is ignored." Our renderImage must not disagree.
			name:   "digest wins over newTag in the same entry",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newTag: \"9.9\"\n    digest: sha256:abc123\n",
		},
		{
			// The chain interaction that makes a naive inversion wrong: the first
			// entry renames, and only then does the second entry's name match.
			name:   "a rename makes a later entry match",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newName: ghcr.io/org/renamed\n" +
				"  - name: ghcr.io/org/renamed\n    newTag: \"4.0\"\n",
		},
		{
			name:   "the last matching entry wins",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/web\n    newTag: \"2.0\"\n" +
				"  - name: ghcr.io/org/web\n    newTag: \"5.0\"\n",
		},
		{
			name:   "an entry that matches nothing changes nothing",
			source: "ghcr.io/org/web:1.0",
			images: "  - name: ghcr.io/org/other\n    newTag: \"7.0\"\n",
		},
		{
			// The parseImageRef edge case: the colon in a registry port is not a tag
			// separator.
			name:   "a registry port is not a tag",
			source: "localhost:5000/org/web:1.0",
			images: "  - name: localhost:5000/org/web\n    newTag: \"2.0\"\n",
		},
		{
			name:   "an untagged source image",
			source: "ghcr.io/org/web",
			images: "  - name: ghcr.io/org/web\n    newTag: \"2.0\"\n",
		},
		{
			name:   "a source image that already carries a digest",
			source: "ghcr.io/org/web@sha256:oldoldold",
			images: "  - name: ghcr.io/org/web\n    newTag: \"2.0\"\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := imageFixture(tc.source, tc.images)

			// What kustomize actually renders.
			rendered, err := renderRoot(files, ".")
			require.NoError(t, err)
			require.Len(t, rendered, 1)
			slots := collectContainerSlots(rendered[0].Object.Object)
			require.Len(t, slots, 1)
			kustomizeSays := slots[0].image

			// What our re-implemented chain renders.
			kusts := parseKustomizations(files)
			doc := kusts["."]
			require.NotNil(t, doc)
			ours, _ := renderImage(parseImageRef(tc.source), doc.images)

			require.Equal(t, kustomizeSays, ours.String(),
				"kustomize renders %q; renderImage renders %q", kustomizeSays, ours.String())
		})
	}
}

// imageFixture is a one-file render root: a Deployment with one container, plus a
// kustomization carrying the images: block under test.
func imageFixture(sourceImage, imagesBlock string) []manifestedit.FileContent {
	deployment := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: app
spec:
  template:
    spec:
      containers:
        - name: web
          image: %s
`, sourceImage)

	kustomization := "resources:\n  - deployment.yaml\nimages:\n" + imagesBlock

	return []manifestedit.FileContent{
		{Path: "deployment.yaml", Content: []byte(deployment)},
		{Path: "kustomization.yaml", Content: []byte(kustomization)},
	}
}
