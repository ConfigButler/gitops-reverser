// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The corpus tests pin override-chain attribution on the example folders: a
// single-root overlay attaches its entries, a nested base composes
// innermost-first, and two roots with differing chains attach nothing and emit
// the ambiguity diagnostic. See
// docs/design/gitops-api/f1-images-replicas-edit-through.md.

func corpusDeployment(t *testing.T, store *ManifestStore, ns string) *DocumentModel {
	t.Helper()
	dm := store.ByManifestIdentity[manifestedit.Identity{
		APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "web",
	}]
	if dm == nil {
		t.Fatalf("Deployment web should be indexed under namespace %q", ns)
	}
	return dm
}

func TestKustomizeOverridesCorpus_ImagesOverlay(t *testing.T) {
	store := corpusStore(t, "supported/images-overlay")
	dm := corpusDeployment(t, store, "app")
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
	dm := corpusDeployment(t, store, "app")
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

func TestKustomizeOverridesCorpus_AmbiguousImages(t *testing.T) {
	store := corpusStore(t, "unsupported/ambiguous-images")
	dm := corpusDeployment(t, store, "app")
	if dm.Overrides != nil {
		t.Errorf("distinct chains from two roots must attach no overrides, got %+v", dm.Overrides)
	}
	if !hasOverrideAmbiguityDiag(store) {
		t.Errorf("want an %s diagnostic", reasonAmbiguousOverrides)
	}
}

// TestKustomizeOverridesNestedBaseIsNotARoot pins the render-root rule: a base
// referenced by another kustomization is not walked as its own root, so the
// nested layout yields ONE chain (base+parent composed), not two conflicting ones.
func TestKustomizeOverridesNestedBaseIsNotARoot(t *testing.T) {
	store := corpusStore(t, "supported/replicas-overlay")
	for _, d := range store.Diagnostics {
		if d.Reason == reasonAmbiguousOverrides {
			t.Fatalf("nested base must not create an ambiguous chain: %s", d.Message)
		}
	}
}

// TestKustomizationOverrideParsing pins the malformed-overrides boundary: a
// kustomization whose images:/replicas: cannot be parsed is unsupported (it
// would fail kustomize build, and we can no longer vouch for the render), while
// well-formed entries keep the folder supported.
func TestKustomizationOverrideParsing(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		unsupported bool
	}{
		{"well-formed images", "images:\n  - name: a/b\n    newTag: \"1.2\"\n", false},
		{"well-formed replicas", "replicas:\n  - name: web\n    count: 3\n", false},
		{"empty images list", "images: []\n", false},
		{"images not a list", "images: oops\n", true},
		{"images entry not a map", "images:\n  - just-a-string\n", true},
		{"images entry missing name", "images:\n  - newTag: \"1.2\"\n", true},
		{"images unknown key", "images:\n  - name: a/b\n    newtag: \"1.2\"\n", true},
		{"non-string newTag", "images:\n  - name: a/b\n    newTag: 1.29\n", true},
		{"blank newName", "images:\n  - name: a/b\n    newName: \"\"\n", true},
		{"replicas count string", "replicas:\n  - name: web\n    count: \"3\"\n", true},
		{"replicas count fractional", "replicas:\n  - name: web\n    count: 2.5\n", true},
		{"replicas count negative", "replicas:\n  - name: web\n    count: -1\n", true},
		{"replicas unknown key", "replicas:\n  - name: web\n    count: 3\n    kind: Deployment\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kustomizationUsesUnsupportedFeature([]byte(tc.content)); got != tc.unsupported {
				t.Errorf("kustomizationUsesUnsupportedFeature = %v, want %v", got, tc.unsupported)
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
