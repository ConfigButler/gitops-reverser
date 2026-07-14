// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// A kustomization.yaml is untrusted input: it comes out of a user's repository, and the
// renderer we now hand it to is a library we do not own. These are the tests for what it
// does with content nobody would write on purpose — and for the one shape that makes a
// folder INVISIBLE to the refusal path rather than merely broken.

// An images: entry's name: is a regular expression, and kustomize compiles it while
// discarding the compile error, then dereferences the nil *Regexp. `- name: "ngin["` does
// not fail the build; it panics inside it. We must refuse before krusty ever sees it.
func TestRenderRoot_InvalidImageNameIsRefusedBeforeTheBuild(t *testing.T) {
	files := imageFixture("nginx:v1", "  - name: \"ngin[\"\n    newTag: \"2.0\"\n")

	rendered, err := renderRoot(files, ".") // must not panic

	require.Nil(t, rendered)
	require.ErrorIs(t, err, errInvalidImageName)
	require.Contains(t, err.Error(), "images[0].name")
}

// A name that IS a valid regex still builds — kustomize's matching is regex matching, and
// refusing more than kustomize refuses would refuse folders that deploy in production.
func TestRenderRoot_ValidRegexImageNameStillBuilds(t *testing.T) {
	files := imageFixture("nginx:v1", "  - name: \"ngin.\"\n    newTag: \"2.0\"\n")

	rendered, err := renderRoot(files, ".")

	require.NoError(t, err)
	require.Len(t, rendered, 1)
	slots := collectContainerSlots(rendered[0].Object.Object)
	require.Len(t, slots, 1)
	// Measured against kustomize: `ngin.` matches `nginx`. Our own renderImage compares
	// names for equality and does NOT — see docs/design/support-boundary/render-attribution.md.
	require.Equal(t, "nginx:2.0", slots[0].image)
}

// The net under krusty: whatever panics in there, the caller gets an error and the process
// keeps its footing. Driven straight at build(), because the refusal above means the panic
// we know about can no longer reach it.
func TestBuild_PanicBecomesAnError(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	require.NoError(t, fSys.WriteFile("/scan/kustomization.yaml",
		[]byte("resources:\n  - deployment.yaml\nimages:\n  - name: \"ngin[\"\n    newTag: \"2.0\"\n")))
	require.NoError(t, fSys.WriteFile("/scan/deployment.yaml", []byte(
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n"+
			"      containers:\n        - name: web\n          image: nginx:v1\n")))

	resMap, err := build(fSys, "/scan") // must not panic

	require.Nil(t, resMap)
	require.ErrorIs(t, err, errBuildPanicked)
}

// A cycle has no render root — every directory in it is referenced by another — so a walk
// over renderRoots alone builds nothing there, records no failure, and the folder passes
// silently. That is the dangerous direction: no build means no chain, no chain means no
// ambiguity, and no ambiguity means the write-fan-in guard never fires on a folder
// kustomize cannot build at all.
func TestRenderChains_CycleIsBuiltAndRefused(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "a/kustomization.yaml", Content: []byte("resources:\n  - ../b\n  - cm.yaml\n")},
		{Path: "a/cm.yaml", Content: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")},
		{Path: "b/kustomization.yaml", Content: []byte("resources:\n  - ../a\n")},
	}
	kusts := parseKustomizations(files)
	require.Empty(t, renderRoots(kusts), "a cycle has no render root; that is the whole problem")

	chains, failed := renderChains(files, kusts)

	require.Empty(t, chains)
	require.Len(t, failed, 1, "the cycle must produce exactly one refusal, not one per member")
	require.Contains(t, failed["a/kustomization.yaml"], "cycle detected",
		"kustomize is the one that gets to call it a cycle; we only make sure it is asked")
}

// The representative built for a cycle is deterministic (sorted first), so the refusal
// names the same file on every reconcile instead of flapping between them.
func TestRenderTargets_CycleRepresentativeIsDeterministic(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "z/kustomization.yaml", Content: []byte("resources:\n  - ../m\n")},
		{Path: "m/kustomization.yaml", Content: []byte("resources:\n  - ../z\n")},
		{Path: "standalone/kustomization.yaml", Content: []byte("resources:\n  - cm.yaml\n")},
		{Path: "standalone/cm.yaml", Content: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n")},
	}
	kusts := parseKustomizations(files)

	for range 5 {
		require.Equal(t, []string{"standalone", "m"}, renderTargets(kusts),
			"real roots first, then one representative per rootless component")
	}
}

// kustomize builds ONE ImageTagTransformer PER images: ENTRY and stamps them all with the
// same origin, so a kustomization with three entries leaves three byte-identical
// transformation records on every object in the build. Each record would otherwise
// contribute that file's whole entry list again, tripling the chain.
func TestChainOf_DuplicateTransformationRecordsDoNotDuplicateEntries(t *testing.T) {
	files := imageFixture("app:v1",
		"  - name: app\n    newTag: v2\n  - name: other\n    newTag: v3\n  - name: third\n    newTag: v4\n")
	rendered, err := renderRoot(files, ".")
	require.NoError(t, err)
	require.Len(t, rendered, 1)
	require.Len(t, rendered[0].TransformedBy, 3,
		"kustomize records one transformation per images: entry — if this changes, the dedupe below is why")

	chain := chainOf(rendered[0], parseKustomizations(files))

	require.NotNil(t, chain)
	require.Len(t, chain.Images, 3, "three entries, not three records × three entries")
}
