// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// The corpus-wide invariant that licenses deleting the re-implemented transformers.
//
// This test used to render every corpus image through our own renderImage chain and require it
// to equal kustomize's. That comparison is gone with the code it compared — there is no second
// opinion left to check against the first. What replaces it is stronger, and it is the property
// the deleted code kept violating:
//
//	AN IN-SYNC FOLDER MUST PROJECT TO A COMPLETE NO-OP.
//
// Take what kustomize renders and hand it back as the live object — the folder is by definition
// already converged — then run the projection. It must route NO entry edits and it must hand
// back the source document unchanged. Any disagreement between what we think a folder renders
// to and what it actually renders shows up here as a phantom edit or a rewritten source file,
// on every render root of every fixture in both corpora.
//
// That is exactly the shape of #231: a digest entry clears the tag, we thought it did not, and
// on a perfectly in-sync folder the projection "helpfully" rewrote the tag out of the source
// manifest. This test fails on that. The old one could not — it compared our belief to our
// belief.

func TestProjection_InSyncCorpusFolderIsANoOp(t *testing.T) {
	roots := allCorpusRenderRoots(t)
	require.NotEmpty(t, roots, "no render roots found — the test would prove nothing")

	checked, skipped := 0, 0
	for _, root := range roots {
		t.Run(root.name, func(t *testing.T) {
			rendered, err := renderRoot(root.files, root.dir)
			if err != nil {
				// A folder we refuse (remote base, generators, patches, plugins) need not
				// render: the gate refuses it and the writer never sees it.
				skipped++
				t.Skipf("not renderable, and refused by the acceptance gate: %v", err)
			}
			chains, _ := renderChains(root.files, parseKustomizations(root.files))

			for _, ro := range rendered {
				if ro.OriginPath == "" {
					continue // a generated resource; generators are refused
				}
				src := sourceDocFor(t, root.files, ro)
				if src == nil {
					continue // renamed by a transformer we refuse; not a supported shape
				}
				attribution, ambiguous := ourAttributionFor(chains, ro)
				if ambiguous || attribution == nil {
					continue // nothing is routed through it, so there is no claim to check
				}
				assertInSyncIsANoOp(t, ro, src, attribution)
				checked++
			}
		})
	}
	t.Logf("checked %d rendered documents for no-op projection (%d roots skipped as refused)",
		checked, skipped)
}

// assertInSyncIsANoOp hands kustomize's own render back as the live object — the folder is by
// definition converged — and requires the projection to route nothing and to leave the source
// document's images exactly as they are.
func assertInSyncIsANoOp(
	t *testing.T,
	ro renderedObject,
	src *unstructured.Unstructured,
	attribution *RenderedOverrides,
) {
	t.Helper()
	where := ro.OriginPath + " " + ro.Object.GetKind() + "/" + ro.Object.GetName()

	out, edits := SplitDesiredForOverrides(src.Object, asLiveObject(t, ro.Object), attribution)

	require.Empty(t, edits,
		"%s: the folder is already in sync, so nothing may be routed to an entry", where)

	for _, slot := range collectImageSlots(out.Object) {
		want := sourceImageAt(src.Object, slot.key)
		if want == "" {
			continue // the live object has a slot the source does not; not our claim
		}
		require.Equal(t, want, slot.image,
			"%s: an in-sync folder must hand back the SOURCE image untouched at %q", where, slot.key)
	}
}

// asLiveObject turns a RENDERED object into the shape a live one actually has.
//
// This is not a formality. A rendered object is NOT a valid unstructured: kustomize hands
// numbers back as Go `int`, and DeepCopyJSON accepts only the JSON types, so
// unstructured.DeepCopy PANICS on one outright ("cannot deep copy int"). The API server hands
// out JSON, so a real live object has int64 — round-tripping through JSON is what makes this
// fixture faithful rather than merely non-crashing.
//
// The same landmine sits under any code that reads a number off a rendered object with the
// standard helpers: unstructured.NestedInt64 reports found=FALSE on a rendered spec.replicas,
// and silently gives you zero. See renderedReplicaCount, which is why the projection does not
// use it.
func asLiveObject(t *testing.T, rendered *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	encoded, err := json.Marshal(rendered.Object)
	require.NoError(t, err)
	var obj map[string]interface{}
	require.NoError(t, json.Unmarshal(encoded, &obj))
	return &unstructured.Unstructured{Object: obj}
}

// sourceImageAt is the image the SOURCE document holds at a slot, or "" when it has none.
func sourceImageAt(src map[string]interface{}, key string) string {
	for _, slot := range collectImageSlots(src) {
		if slot.key == key {
			return slot.image
		}
	}
	return ""
}

// ourAttributionFor is what the store attributes to a document, and whether it was found
// ambiguous (reached by more than one render root with differing answers, which we refuse to
// route through).
func ourAttributionFor(
	chains map[chainKey]*overrideAssignment,
	ro renderedObject,
) (*RenderedOverrides, bool) {
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
	return a.rendered, false
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
