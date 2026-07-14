// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// TestRenderImage_MatchesKustomizeOnTheHardCases lived here: eleven hand-picked cases driven
// through BOTH kustomize and our own renderImage, required to agree. It is gone because
// renderImage is gone — there is no second opinion left to pin to the first.
//
// It is worth recording why the table was never enough, because it is the argument for the
// whole workstream. Its rows were the cases we THOUGHT of. B1 — that an images: entry name is
// a regex, so `- name: "ap."` matches `app` — was not among them, and shipped. A table of
// cases we thought of is not a substitute for making kustomize the arbiter at runtime, which
// is what the dye now does. See docs/design/support-boundary/render-attribution.md §6.

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

// PATCHES RUN FIRST, AND THE TRANSFORMERS WIN.
//
// This is the fact that makes "the patch asks for it, so edit the patch" wrong, and it is the one
// a future refactor will break. A patch that sets a field an images:/replicas: entry also governs
// is DEAD TEXT: kustomize applies the patch, then the transformers overwrite it, and the value the
// user reads in the patch file is not the value the cluster runs.
//
// It cannot be established by reading the patch — reading it tells you what the patch ASKS for,
// never what the build DOES — so it is pinned here as a render, against the library that decides
// it. The transformations annotation says the same thing in the same breath: PatchTransformer
// runs, and then the two transformers do.
func TestRender_ATransformerOverridesAPatchOnTheSameField(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "deployment.yaml", Content: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/example/app:1.0.0
`)},
		{Path: "patch.yaml", Content: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 7
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/example/app:patched
`)},
		{Path: "kustomization.yaml", Content: []byte(`resources:
  - deployment.yaml
patches:
  - path: patch.yaml
images:
  - name: ghcr.io/example/app
    newTag: 2.0.0
replicas:
  - name: web
    count: 3
`)},
	}

	rendered, err := renderRoot(files, ".")
	require.NoError(t, err)
	require.Len(t, rendered, 1, "a patch is a build input, not a resource: it renders no object of its own")

	object := rendered[0].Object.Object
	require.Equal(t, "ghcr.io/example/app:2.0.0",
		nestedOf(t, object, "spec", "template", "spec", "containers", "0", "image"),
		"the images: entry wins; the patch's :patched never reaches the cluster")
	require.Equal(t, 3, nestedOf(t, object, "spec", "replicas"),
		"the replicas: entry wins; the patch's 7 never reaches the cluster")

	var order []string
	for _, tr := range rendered[0].TransformedBy {
		order = append(order, tr.Kind)
	}
	require.Equal(t, []string{"PatchTransformer", "ReplicaCountTransformer", "ImageTagTransformer"}, order,
		"kustomize itself says which ran when, and the patch ran first")
}
