// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"

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
