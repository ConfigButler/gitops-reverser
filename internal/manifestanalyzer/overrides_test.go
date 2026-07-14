// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The corpus tests pin override-chain attribution on the example folders: a
// single-root overlay attaches its entries, a nested base composes
// innermost-first, and two roots with differing chains attach nothing and emit
// the ambiguity diagnostic. See
// docs/design/support-boundary/finished/images-and-replicas-edit-through.md.

func corpusDeployment(t *testing.T, store *ManifestStore) *DocumentModel {
	t.Helper()
	dm := store.ByManifestIdentity[manifestedit.Identity{
		APIVersion: "apps/v1", Kind: "Deployment", Namespace: "app", Name: "web",
	}]
	if dm == nil {
		t.Fatalf("Deployment web should be indexed under namespace app")
	}
	return dm
}

func TestKustomizeOverridesCorpus_ImagesOverlay(t *testing.T) {
	store := corpusStore(t, "supported/images-overlay")
	dm := corpusDeployment(t, store)
	if dm.Overrides == nil || len(dm.Overrides.Images) != 1 || len(dm.Overrides.Replicas) != 0 {
		t.Fatalf("want exactly one image override, got %+v", dm.Overrides)
	}
	img := dm.Overrides.Images[0]
	if img.Source != "kustomization.yaml" || img.Name != "ghcr.io/example/podinfo" {
		t.Errorf("entry source/name = %q/%q, want kustomization.yaml/ghcr.io/example/podinfo",
			img.Source, img.Name)
	}
	if !img.HasNewTag || img.NewTag != "6.5.0" || img.HasNewName || img.HasDigest {
		t.Errorf("entry should declare only newTag 6.5.0, got %+v", img)
	}
	if hasOverrideAmbiguityDiag(store) {
		t.Errorf("single-root overlay must not be ambiguous")
	}
}

func TestKustomizeOverridesCorpus_ReplicasOverlayChain(t *testing.T) {
	store := corpusStore(t, "supported/replicas-overlay")
	dm := corpusDeployment(t, store)
	if dm.Overrides == nil || len(dm.Overrides.Images) != 1 || len(dm.Overrides.Replicas) != 1 {
		t.Fatalf("want one image + one replica override from the chain, got %+v", dm.Overrides)
	}
	// Build order: the base's own entries come before the referencing root's.
	img := dm.Overrides.Images[0]
	if img.Source != "base/kustomization.yaml" {
		t.Errorf("innermost (base) images entry should come first, got source %q", img.Source)
	}
	if !img.HasNewName || img.NewName != "ghcr.io/example/podinfo-mirror" {
		t.Errorf("base entry should declare newName, got %+v", img)
	}
	rep := dm.Overrides.Replicas[0]
	if rep.Source != "kustomization.yaml" || rep.Name != "web" || rep.Count != 3 {
		t.Errorf("replica entry = %+v, want web:3 from kustomization.yaml", rep)
	}
}

// A diamond under ONE render root (root → a → base, root → b → base) must
// TestKustomizeOverridesCorpus_DiamondIsUnbuildable pins the diamond's fate.
//
// A diamond — one render root reaching a shared base through two overlays — is not
// merely ambiguous to us: kustomize REFUSES to build it ("may not add resource with
// an already registered id"), which means Flux cannot deploy the folder either. So
// the folder is refused, and no document in it is routable.
//
// This used to be accepted and merely guarded: the hand-written walk recorded both
// paths and attached no overrides, leaving the write-fan-in precondition to refuse
// the write later. Refusing the unbuildable folder up front is strictly stronger,
// and it is what the renderer tells us for free.
func TestKustomizeOverridesCorpus_DiamondIsUnbuildable(t *testing.T) {
	store := corpusStore(t, "unsupported/diamond-images")
	dm := corpusDeployment(t, store)
	if dm.Overrides != nil {
		t.Errorf("a folder kustomize cannot build must attach no overrides, got %+v", dm.Overrides)
	}
	if !hasRenderFailure(store) {
		t.Errorf("want a %s diagnostic naming kustomize's build error", reasonRenderFailed)
	}
}

// TestAccept_UnbuildableRenderRootRefusesTheFolder is the other half: the diamond is
// not merely unrouted, it is REFUSED. A folder whose render root kustomize cannot
// build is one no GitOps controller can deploy, and one whose renders we cannot
// reason about — so the operator will not write into it.
func TestAccept_UnbuildableRenderRootRefusesTheFolder(t *testing.T) {
	fsys := os.DirFS(filepath.Join("testdata", "contextual-namespace", "unsupported", "diamond-images"))
	_, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})
	if acc.Accepted {
		t.Fatalf("a render root kustomize cannot build must refuse the folder")
	}
	found := false
	for _, iss := range acc.Issues {
		if iss.Kind == IssueUnsupportedKustomize {
			found = true
		}
	}
	if !found {
		t.Errorf("want an %s issue, got %+v", IssueUnsupportedKustomize, acc.Issues)
	}
}

func TestKustomizeOverridesCorpus_AmbiguousImages(t *testing.T) {
	store := corpusStore(t, "unsupported/ambiguous-images")
	dm := corpusDeployment(t, store)
	if dm.Overrides != nil {
		t.Errorf("distinct chains from two roots must attach no overrides, got %+v", dm.Overrides)
	}
	if !hasOverrideAmbiguityDiag(store) {
		t.Errorf("want an %s diagnostic", reasonAmbiguousOverrides)
	}
}

// TestOverridesAmbiguousAt pins the store-side signal the writer's write-fan-in
// precondition consults: the shared document TWO render roots reach with differing
// chains reports ambiguous, a build directive / unknown path does not, and a store
// with no ambiguous chain never reports it.
//
// The fixture is ambiguous-images, not the diamond: the diamond does not build at
// all now, so it is refused before any write is planned, while ambiguous-images
// builds cleanly from both roots and disagrees — which is exactly the shape the
// fan-in guard exists for.
func TestOverridesAmbiguousAt(t *testing.T) {
	shared := corpusStore(t, "unsupported/ambiguous-images")
	if !shared.OverridesAmbiguousAt("shared.yaml") {
		t.Errorf("the document two roots reach with differing chains must report ambiguous")
	}
	if shared.OverridesAmbiguousAt("kustomization.yaml") {
		t.Errorf("a build directive is not an ambiguous managed write path")
	}
	if shared.OverridesAmbiguousAt("no/such/file.yaml") {
		t.Errorf("an unknown path must not report ambiguous")
	}
	clean := corpusStore(t, "supported/images-overlay")
	if clean.OverridesAmbiguousAt("deployment.yaml") {
		t.Errorf("a single unambiguous chain must never report ambiguous")
	}
}

// hasRenderFailure reports whether the store recorded a kustomize build failure.
func hasRenderFailure(store *ManifestStore) bool {
	for _, d := range store.Diagnostics {
		if d.Reason == reasonRenderFailed {
			return true
		}
	}
	return false
}

func TestKustomizeOverridesNestedBaseIsNotARoot(t *testing.T) {
	store := corpusStore(t, "supported/replicas-overlay")
	for _, d := range store.Diagnostics {
		if d.Reason == reasonAmbiguousOverrides {
			t.Fatalf("nested base must not create an ambiguous chain: %s", d.Message)
		}
	}
}

// TestKustomizationOverrideParsing pins the malformed-overrides boundary: a
// kustomization whose images:/replicas: kustomize itself cannot decode is
// unsupported (it would fail kustomize build, and we can no longer vouch for the
// render), while entries kustomize accepts keep the folder supported.
//
// Three of these cases changed when the hand-written key check was replaced by
// kustomize's own type, and each was verified against a real `kustomize build`:
// we now agree with the renderer where we previously did not. The two marked
// "kustomize honours" were refused before — folders Flux renders happily.
func TestKustomizationOverrideParsing(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		unsupported bool
	}{
		{"well-formed images", "images:\n  - name: a/b\n    newTag: \"1.2\"\n", false},
		{"well-formed replicas", "replicas:\n  - name: web\n    count: 3\n", false},
		{"empty images list", "images: []\n", false},
		// kustomize decodes into a typed struct via JSON, so a key differing only in
		// case still binds: `newtag` sets NewTag, and the build applies it. Refusing
		// it would refuse a folder that renders.
		{"kustomize honours a case-variant key", "images:\n  - name: a/b\n    newtag: \"1.2\"\n", false},
		// An empty component is not a declared-but-broken transform; kustomize skips
		// it and applies the rest of the entry.
		{
			"kustomize honours a blank newName",
			"images:\n  - name: a/b\n    newName: \"\"\n    newTag: \"1.2\"\n",
			false,
		},
		{"images not a list", "images: oops\n", true},
		{"images entry not a map", "images:\n  - just-a-string\n", true},
		{"images entry missing name", "images:\n  - newTag: \"1.2\"\n", true},
		{"images entry genuinely unknown key", "images:\n  - name: a/b\n    bogus: \"1.2\"\n", true},
		// tagSuffix is a real kustomize field we do not model. Decoding it would
		// silently ignore a transform kustomize applies, so it refuses the folder.
		{"images tagSuffix is not modelled", "images:\n  - name: a/b\n    tagSuffix: -rc1\n", true},
		{"non-string newTag", "images:\n  - name: a/b\n    newTag: 1.29\n", true},
		{"replicas count string", "replicas:\n  - name: web\n    count: \"3\"\n", true},
		{"replicas count fractional", "replicas:\n  - name: web\n    count: 2.5\n", true},
		{"replicas count negative", "replicas:\n  - name: web\n    count: -1\n", true},
		{"replicas unknown key", "replicas:\n  - name: web\n    count: 3\n    kind: Deployment\n", true},
		// Holes the hand-written deny-list had: neither was on it, and both change
		// the render. They are refused now because they are not in the modelled set.
		{
			"vars",
			"vars:\n  - name: NS\n    objref:\n      kind: Service\n      name: web\n      apiVersion: v1\n",
			true,
		},
		{"validators (plugin code)", "validators:\n  - check.yaml\n", true},
		// imageTags is the deprecated spelling of images, and FixKustomization folds
		// it into images exactly as the builder does — so it is supported, and its
		// override is seen, where before it was silently ignored.
		{"imageTags is normalised into images", "imageTags:\n  - name: a/b\n    newTag: \"1.2\"\n", false},
		{"bases is normalised into resources", "bases:\n  - ../base\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, features := parseKustomization([]byte(tc.content), "kustomization.yaml", nil)
			if got := len(features) > 0; got != tc.unsupported {
				t.Errorf("unsupported = %v (%v), want %v", got, features, tc.unsupported)
			}
		})
	}
}

func corpusStore(t *testing.T, dir string) *ManifestStore {
	t.Helper()
	mapper := typeset.NewSnapshotRegistry(sampleClusterSnapshot())
	fsys := os.DirFS(filepath.Join("testdata", "contextual-namespace", dir))
	return BuildStore(context.Background(), fsys, mapper)
}

func hasOverrideAmbiguityDiag(store *ManifestStore) bool {
	for _, d := range store.Diagnostics {
		if d.Reason == reasonAmbiguousOverrides {
			return true
		}
	}
	return false
}

// A document can render an image while NO images:/replicas: entry governs it: the chain is
// nil, but the attribution is not, because the projection still needs to know what the folder
// renders that slot to. The two are different questions, and anyOverrides has to ask both --
// otherwise two roots disagreeing only on the attribution would diverge in the fingerprint
// while ambiguous() stayed false, leaving the document silently un-routed with no diagnostic
// and no fan-in refusal.
//
// Requested in review on #233.
func TestRecord_AttributionWithoutAChainIsStillSomethingAtStake(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "deployment.yaml", Content: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: app:v1
`)},
		// No images:, no replicas: -- nothing governs this document.
		{Path: "kustomization.yaml", Content: []byte("resources:\n  - deployment.yaml\n")},
	}

	assignments, failed := renderChains(files, parseKustomizations(files))
	require.Empty(t, failed)

	a := assignments[chainKey{originPath: "deployment.yaml", kind: "Deployment", name: "web"}]
	require.NotNil(t, a)
	require.Nil(t, a.overrides, "no entry governs it, so there is no chain")
	require.NotNil(t, a.rendered, "but the folder still renders its image, and the projection reads that")
	require.True(t, a.anyOverrides,
		"an attribution with no chain is still something an edit could be routed through")

	// And with nothing supplying the image, an edit to it flows into the source file.
	require.Empty(t, a.rendered.Images["/spec/template/spec/containers\x00app"].Tag,
		"no entry supplies the tag")
}
