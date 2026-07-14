// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This is the differential test that licenses deleting the re-implemented
// transformers: for every kustomize render root in both corpora, the image our
// renderImage chain produces must be byte-for-byte the image kustomize produces.
//
// It used to also compare the override CHAIN against kustomize's transformations
// annotation. That assertion has done its job and is gone: the chain is now READ
// from that annotation (override_chain.go), so comparing the two would be comparing
// kustomize to itself.

func TestRenderRoot_ImagesAgreeWithKustomize(t *testing.T) {
	roots := allCorpusRenderRoots(t)
	require.NotEmpty(t, roots, "no render roots found — the test would prove nothing")

	compared, skipped := 0, 0
	for _, root := range roots {
		t.Run(root.name, func(t *testing.T) {
			rendered, err := renderRoot(root.files, root.dir)
			if err != nil {
				// A folder we refuse (remote base, generators, patches, plugins)
				// need not render: the gate refuses it and the writer never sees it.
				skipped++
				t.Skipf("not renderable, and refused by the acceptance gate: %v", err)
			}

			kusts := parseKustomizations(root.files)
			chains, _ := renderChains(root.files, kusts) // once per fixture, not once per object
			for _, ro := range rendered {
				if ro.OriginPath == "" {
					continue // a generated resource; generators are refused
				}
				src := sourceDocFor(t, root.files, ro)
				if src == nil {
					continue // renamed by a transformer we refuse; not a supported shape
				}
				chain, ambiguous := ourChainFor(chains, ro)
				if ambiguous {
					continue // we route nothing through it; there is no claim to check
				}
				compared += assertImagesMatchKustomize(t, ro, src, chain)
			}
		})
	}
	t.Logf("compared %d rendered images against the hand-rolled chain (%d roots skipped as refused)",
		compared, skipped)
}

// assertImagesMatchKustomize renders each source container image through our own
// chain and requires it to equal what kustomize actually produced. Returns the
// number of images compared.
func assertImagesMatchKustomize(
	t *testing.T,
	ro renderedObject,
	src *unstructured.Unstructured,
	chain *KustomizeOverrides,
) int {
	t.Helper()
	var entries []ImageOverride
	if chain != nil {
		entries = chain.Images
	}
	ours := map[string]string{}
	for _, slot := range collectContainerSlots(src.Object) {
		got, _ := renderImage(parseImageRef(slot.image), entries)
		ours[slot.key] = got.String()
	}

	compared := 0
	for _, slot := range collectContainerSlots(ro.Object.Object) {
		got, known := ours[slot.key]
		if !known {
			continue
		}
		want := slot.image // kustomize's render is the expected truth
		compared++
		require.Equal(t, want, got,
			"%s container %s: kustomize renders %q, our chain renders %q",
			ro.OriginPath, slot.key, want, got)
	}
	return compared
}

// ourChainFor is the override chain the store attributes to a document, and whether
// it was found ambiguous (reached by more than one render root with differing
// chains, which we refuse to route through).
func ourChainFor(
	chains map[chainKey]*overrideAssignment,
	ro renderedObject,
) (*KustomizeOverrides, bool) {
	a := chains[chainKey{
		originPath: ro.OriginPath,
		kind:       ro.Object.GetKind(),
		name:       ro.Object.GetName(),
	}]
	if a == nil {
		return nil, false
	}
	if a.ambiguous() {
		return nil, true
	}
	return a.overrides, false
}

// sourceDocFor finds the document in the origin file that produced a rendered
// object, matched on kind + name (a transformer that rewrites either is refused).
func sourceDocFor(
	t *testing.T,
	files []manifestedit.FileContent,
	ro renderedObject,
) *unstructured.Unstructured {
	t.Helper()
	for _, f := range files {
		if filepathToSlash(f.Path) != ro.OriginPath {
			continue
		}
		for _, chunk := range bytes.Split(f.Content, []byte("\n---")) {
			obj := map[string]interface{}{}
			if err := yaml.Unmarshal(chunk, &obj); err != nil || len(obj) == 0 {
				continue
			}
			u := &unstructured.Unstructured{Object: obj}
			if u.GetKind() == ro.Object.GetKind() && u.GetName() == ro.Object.GetName() {
				return u
			}
		}
	}
	return nil
}

type corpusRoot struct {
	name  string
	dir   string
	files []manifestedit.FileContent
}

// allCorpusRenderRoots collects every render root from both corpora: the layout
// corpus (real-world repo shapes) and the contextual-namespace fixtures (which are
// what pin the override projection today).
func allCorpusRenderRoots(t *testing.T) []corpusRoot {
	t.Helper()
	var out []corpusRoot
	out = append(out, renderRootsUnder(t, filepath.Join("..", "..", "test", "fixtures", "gitops-layouts"), 2)...)
	out = append(out, renderRootsUnder(t, filepath.Join("testdata", "contextual-namespace"), 2)...)
	return out
}

// renderRootsUnder treats every directory at the given depth as one fixture (its
// own scan root) and enumerates the render roots inside it.
func renderRootsUnder(t *testing.T, corpus string, depth int) []corpusRoot {
	t.Helper()
	var out []corpusRoot
	for _, fixtureDir := range dirsAtDepth(t, corpus, depth) {
		files := readYAMLTree(t, fixtureDir)
		if len(files) == 0 {
			continue
		}
		for _, dir := range renderRoots(parseKustomizations(files)) {
			out = append(out, corpusRoot{
				name: strings.ReplaceAll(filepath.Join(filepath.Base(filepath.Dir(fixtureDir)),
					filepath.Base(fixtureDir), dir), string(filepath.Separator), "/"),
				dir:   dir,
				files: files,
			})
		}
	}
	return out
}

// dirsAtDepth lists the directories exactly depth levels below root.
func dirsAtDepth(t *testing.T, root string, depth int) []string {
	t.Helper()
	dirs := []string{root}
	for range depth {
		var next []string
		for _, d := range dirs {
			entries, err := os.ReadDir(d)
			require.NoError(t, err)
			for _, e := range entries {
				if e.IsDir() {
					next = append(next, filepath.Join(d, e.Name()))
				}
			}
		}
		dirs = next
	}
	return dirs
}

// readYAMLTree reads every YAML file under root as scan-root-relative FileContent.
func readYAMLTree(t *testing.T, root string) []manifestedit.FileContent {
	t.Helper()
	var out []manifestedit.FileContent
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err //nolint:wrapcheck // test helper
		}
		if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err //nolint:wrapcheck // test helper
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err //nolint:wrapcheck // test helper
		}
		out = append(out, manifestedit.FileContent{Path: filepath.ToSlash(rel), Content: content})
		return nil
	})
	require.NoError(t, err)
	return out
}
