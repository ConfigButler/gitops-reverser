// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// The source form, driven by ground truth: build a real tree, render it with kustomize, and hand
// the render back as the live object. The folder is then converged BY CONSTRUCTION, so anything
// the projection wants to write into the source file is a phantom — a value the build produced,
// being mirrored back into the build's own input.
//
// That is not a hypothetical failure. Two of these fixtures fail on the code as it stood: one
// with a feature we ACCEPT today (labels/commonAnnotations), one with the patches this workstream
// is about. They are the same bug.

// sourceDoc is the one document these fixtures manage: the base the overlay renders.
const sourceDoc = "deployment.yaml"

// renderedAsLive is the whole trick: kustomize's own output, in the shape a live object has. The
// folder is then converged by construction, and anything the projection writes is a phantom.
func renderedAsLive(t *testing.T, files []manifestedit.FileContent) map[string]interface{} {
	t.Helper()
	rendered, err := renderRoot(files, ".")
	require.NoError(t, err, "the fixture must be a folder kustomize can build")
	for _, ro := range rendered {
		if ro.OriginPath == sourceDoc {
			return asLiveObject(t, ro.Object).Object
		}
	}
	t.Fatalf("kustomize rendered nothing from %s", sourceDoc)
	return nil
}

// requireSourceUntouched is the no-op claim: the projected document IS the source document.
func requireSourceUntouched(t *testing.T, files []manifestedit.FileContent, out *unstructured.Unstructured) {
	t.Helper()
	var src map[string]interface{}
	require.NoError(t, yaml.Unmarshal(contentOf(t, files, sourceDoc), &src))
	require.Equal(t, normaliseForCompare(t, src), normaliseForCompare(t, out.Object),
		"an in-sync folder must hand the source document back byte-for-byte")
}

// THE SHIPPED BUG. labels: and commonAnnotations: are on the supported list today, and they inject
// metadata into every rendered object. Mirroring the live object straight back wrote that metadata
// into the source file, as if the author had typed it — measured, on an accepted folder, with
// nothing changed in the cluster and nothing changed in the render.
func TestSourceForm_InjectedMetadataStaysOutOfTheSource(t *testing.T) {
	files := []manifestedit.FileContent{
		file(sourceDoc, deploymentSource("ghcr.io/example/app:1.0.0", "1")),
		file("kustomization.yaml", `resources:
  - deployment.yaml
labels:
  - pairs:
      env: prod
    includeSelectors: false
commonAnnotations:
  owner: platform
images:
  - name: ghcr.io/example/app
    newTag: 2.0.0
`),
	}
	out, edits := splitFixture(t, files, sourceDoc,
		renderedAsLive(t, files))

	require.Empty(t, edits)
	requireSourceUntouched(t, files, out)
	require.NotContains(t, out.GetLabels(), "env", "the overlay's label is the BUILD's, not the file's")
	require.NotContains(t, out.GetAnnotations(), "owner")
}

// The same bug with a patch instead of a label, which is the one that matters: the value the
// overlay pins is one ENVIRONMENT's, and baking it into the base rewrites what every other
// environment starts from. The render is identical afterwards, so no re-render can catch it —
// only refusing to write it can.
func TestSourceForm_PatchedFieldStaysOutOfTheSource(t *testing.T) {
	files := patchedFolder()
	out, edits := splitFixture(t, files, sourceDoc,
		renderedAsLive(t, files))

	require.Empty(t, edits)
	requireSourceUntouched(t, files, out)
	require.Equal(t, "50m", cpuRequestOf(t, out.Object), "the patch's 200m is the overlay's value")
}

// A field no transformer and no patch touches is the user's, and it is written through exactly as
// before. This is the half of the rule that keeps the change a no-op for every ordinary document.
func TestSourceForm_UngovernedFieldIsWrittenThrough(t *testing.T) {
	files := patchedFolder()
	live := renderedAsLive(t, files)
	setNested(t, live, int64(9080), "spec", "template", "spec", "containers", "0", "ports", "0", "containerPort")

	out, edits := splitFixture(t, files, sourceDoc, live)

	require.Empty(t, edits)
	require.Equal(t, int64(9080), nestedOf(t, out.Object,
		"spec", "template", "spec", "containers", "0", "ports", "0", "containerPort"),
		"a port nothing in the build touches is the user's edit, and belongs in the file")
	require.Equal(t, "50m", cpuRequestOf(t, out.Object),
		"and the patched CPU is still the overlay's, in the same container the user did edit")
}

// The composition that has to work, and the reason the rule cannot stop at whole subtrees: the
// user bumps an image that an images: ENTRY governs, inside a container whose CPU a PATCH governs.
// The tag goes to the entry, the CPU stays in the overlay, and the source file keeps every byte.
func TestSourceForm_ImageBumpRoutesAndLeavesThePatchedFieldAlone(t *testing.T) {
	files := patchedFolder()
	live := renderedAsLive(t, files)
	setNested(t, live, "ghcr.io/example/app:3.0.0", "spec", "template", "spec", "containers", "0", "image")

	out, edits := splitFixture(t, files, sourceDoc, live)

	require.Len(t, edits, 1, "the tag is supplied by the entry, so the edit belongs on the entry")
	require.Equal(t, "3.0.0", edits[0].Edit.Value)
	require.Equal(t, fieldNewTag, edits[0].Edit.Field)
	requireSourceUntouched(t, files, out)
}

// A container the PATCH adds is not the source's, and it must not be written into it. This is
// where aligning the source with the render by POSITION would corrupt the file rather than merely
// leak into it: kustomize PREPENDS the added container, so the source's element 0 is the render's
// element 1 (measured).
func TestSourceForm_BuildAddedContainerIsNotBakedIntoTheSource(t *testing.T) {
	files := []manifestedit.FileContent{
		file(sourceDoc, deploymentSource("ghcr.io/example/app:1.0.0", "1")),
		file("patch.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: sidecar
          image: ghcr.io/example/sidecar:1.0.0
`),
		file("kustomization.yaml", `resources:
  - deployment.yaml
patches:
  - path: patch.yaml
images:
  - name: ghcr.io/example/app
    newTag: 2.0.0
`),
	}
	live := renderedAsLive(t, files)
	setNested(t, live, "ghcr.io/example/app:3.0.0", "spec", "template", "spec", "containers", "1", "image")

	out, edits := splitFixture(t, files, sourceDoc, live)

	require.Len(t, edits, 1, "the app's tag still routes to its entry")
	require.Equal(t, "3.0.0", edits[0].Edit.Value)
	requireSourceUntouched(t, files, out)
	containers := nestedOf(t, out.Object, "spec", "template", "spec", "containers")
	require.Len(t, containers, 1, "the sidecar belongs to the overlay; the base never declared it")
}

// And where the pairing cannot be made, the edit is REFUSED rather than guessed. args: is a list
// of scalars: if the build rewrote it and the user rewrote it too, there is no honest way to say
// which of the source's elements survive.
func TestSourceForm_UnpairableListRefusesTheEdit(t *testing.T) {
	files := []manifestedit.FileContent{
		file(sourceDoc, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/example/app:1.0.0
          args: ["--from-the-base"]
`),
		file("patch.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          args: ["--from-the-patch"]
`),
		file("kustomization.yaml", "resources:\n  - deployment.yaml\npatches:\n  - path: patch.yaml\n"),
	}
	live := renderedAsLive(t, files)
	setNested(t, live, []interface{}{"--the-user-changed-it"},
		"spec", "template", "spec", "containers", "0", "args")

	var gitRaw map[string]interface{}
	require.NoError(t, yaml.Unmarshal(contentOf(t, files, sourceDoc), &gitRaw))
	chains, _ := renderChains(files, parseKustomizations(files))
	rendered := chains[chainKey{originPath: sourceDoc, kind: "Deployment", name: "web"}].rendered

	_, _, err := SplitDesiredForOverrides(gitRaw, &unstructured.Unstructured{Object: live}, rendered)

	var refused *SourceFormRefusedError
	require.ErrorAs(t, err, &refused)
	require.Contains(t, refused.Field, "args")
}

// patchedFolder is the shape tolerating patches would newly accept: one root, one base document,
// one strategic-merge patch that pins a CPU request, and an images: entry over the top.
func patchedFolder() []manifestedit.FileContent {
	return []manifestedit.FileContent{
		file(sourceDoc, `apiVersion: apps/v1
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
          ports:
            - containerPort: 8080
          resources:
            requests:
              cpu: 50m
`),
		file("patch.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 200m
`),
		file("kustomization.yaml", `resources:
  - deployment.yaml
patches:
  - path: patch.yaml
images:
  - name: ghcr.io/example/app
    newTag: 2.0.0
`),
	}
}

func cpuRequestOf(t *testing.T, obj map[string]interface{}) string {
	t.Helper()
	value := nestedOf(t, obj, "spec", "template", "spec", "containers", "0", "resources", "requests", "cpu")
	cpu, ok := value.(string)
	require.True(t, ok, "cpu request is %T", value)
	return cpu
}

// nestedOf walks maps and lists alike: a numeric path element indexes a list.
func nestedOf(t *testing.T, obj interface{}, path ...string) interface{} {
	t.Helper()
	current := obj
	for _, step := range path {
		switch node := current.(type) {
		case map[string]interface{}:
			current = node[step]
		case []interface{}:
			i := listIndex(t, step)
			require.Less(t, i, len(node), "index %s is past the end of the list", step)
			current = node[i]
		default:
			t.Fatalf("cannot walk %q into a %T", step, current)
		}
	}
	return current
}

func setNested(t *testing.T, obj map[string]interface{}, value interface{}, path ...string) {
	t.Helper()
	parent := nestedOf(t, obj, path[:len(path)-1]...)
	last := path[len(path)-1]
	switch node := parent.(type) {
	case map[string]interface{}:
		node[last] = value
	case []interface{}:
		node[listIndex(t, last)] = value
	default:
		t.Fatalf("cannot set %q on a %T", last, parent)
	}
}

func listIndex(t *testing.T, step string) int {
	t.Helper()
	require.Len(t, step, 1, "list index %q must be a single digit in these fixtures", step)
	require.True(t, step[0] >= '0' && step[0] <= '9', "list index %q is not a digit", step)
	return int(step[0] - '0')
}
