// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// These tests drive the projection with GROUND TRUTH.
//
// They used to construct (gitRaw, desired, overrides) by hand, which meant they asserted what
// we BELIEVED a folder renders to. That is exactly the belief that was wrong — twice, in
// shipped code — so a test net woven out of it could not have caught either bug, and did not.
//
// Now every case builds a small real repository, renders it with kustomize, reads the
// attribution off a dyed counterfactual render, and drives the projection with the result. A
// row that disagrees with kustomize now fails here instead of silently corrupting a source
// file, and if kustomize ever changes its semantics these fail rather than the operator.

// splitFixture is the whole harness: build the tree, ask kustomize what it renders and which
// entry supplied what, then invert the live object against that answer.
func splitFixture(
	t *testing.T,
	files []manifestedit.FileContent,
	sourcePath string,
	live map[string]interface{},
) (*unstructured.Unstructured, []OverrideEdit) {
	t.Helper()
	desired := &unstructured.Unstructured{Object: live}

	assignments, failed := renderChains(files, parseKustomizations(files))
	require.Empty(t, failed, "the fixture must be a folder kustomize can build")

	assignment := assignments[chainKey{
		originPath: sourcePath,
		kind:       desired.GetKind(),
		name:       desired.GetName(),
	}]
	require.NotNil(t, assignment, "kustomize must render %s/%s from %s",
		desired.GetKind(), desired.GetName(), sourcePath)

	var gitRaw map[string]interface{}
	require.NoError(t, yaml.Unmarshal(contentOf(t, files, sourcePath), &gitRaw))

	return SplitDesiredForOverrides(gitRaw, desired, assignment.rendered)
}

func contentOf(t *testing.T, files []manifestedit.FileContent, path string) []byte {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f.Content
		}
	}
	t.Fatalf("fixture has no file %q", path)
	return nil
}

func file(path, content string) manifestedit.FileContent {
	return manifestedit.FileContent{Path: path, Content: []byte(content)}
}

// deploymentSource is the Deployment as GIT holds it.
func deploymentSource(image string, replicas string) string {
	if replicas != "" {
		replicas = "  replicas: " + replicas + "\n"
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
%s  template:
    spec:
      containers:
        - name: app
          image: %s
`, replicas, image)
}

// liveDeployment is the Deployment as the CLUSTER holds it — the rendered form, which is what
// the source file plus the entries have to reproduce.
func liveDeployment(image string, replicas *int64) map[string]interface{} {
	spec := map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": image},
				},
			},
		},
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "web"},
		"spec":       spec,
	}
}

func kustomizationWith(body string) string {
	return "resources:\n  - deployment.yaml\n" + body
}

func imageOf(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	slots := collectImageSlots(u.Object)
	require.Len(t, slots, 1, "want exactly one image slot")
	return slots[0].image
}

func imagesEntry(body string) []manifestedit.FileContent {
	return []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("ghcr.io/example/app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(body)),
	}
}

// The core of the edit-through: a live tag change produced by a newTag entry lands on the
// entry, and the source file keeps its bytes.
func TestSplitDesired_TagRoutedToEntry(t *testing.T) {
	files := imagesEntry("images:\n  - name: ghcr.io/example/app\n    newTag: \"1.5.0\"\n")

	out, edits := splitFixture(t, files, "deployment.yaml",
		liveDeployment("ghcr.io/example/app:2.0.0", nil))

	require.Equal(t, "ghcr.io/example/app:1.0.0", imageOf(t, out), "the source form must be restored")
	require.Len(t, edits, 1)
	require.Equal(t, OverrideEdit{
		KustomizationPath: "kustomization.yaml",
		Edit: manifestedit.KustomizationEdit{
			Section: manifestedit.KustomizationSectionImages, EntryIndex: 0,
			EntryName: "ghcr.io/example/app", Field: "newTag", Value: "2.0.0",
		},
	}, edits[0])
}

// THE IDEMPOTENT PIN — the case that killed leave-one-out probing, and the reason the dye
// exists.
//
// The source is already at v1 and the overlay pins v1: the state every repository is in the
// moment a release lands in both places. Removing the entry moves NOTHING, so a probe that
// asks "what changed?" concludes the source file supplies the tag — and writes the user's next
// tag into the base, where the overlay overrides it straight back, on every reconcile, forever.
//
// The dye is not fooled: a nonce nothing else can produce comes out of that field, so the
// entry is the supplier even though its value is identical to the source's.
func TestSplitDesired_IdempotentPinIsStillAttributedToTheEntry(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:v1", "")),
		file("kustomization.yaml", kustomizationWith("images:\n  - name: app\n    newTag: v1\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:v2", nil))

	require.Equal(t, "app:v1", imageOf(t, out), "the base must NOT absorb the tag; the entry owns it")
	require.Len(t, edits, 1, "the tag must land on the entry, or it never converges")
	require.Equal(t, "newTag", edits[0].Edit.Field)
	require.Equal(t, "v2", edits[0].Edit.Value)
}

// A TIE: two entries pinning the same tag. Removal cannot attribute either of them — drop
// one and the other still produces the same bytes. The dye reads the LAST writer straight off
// the output, because the two nonces are distinguishable even though the two values were not.
func TestSplitDesired_TieIsAttributedToTheLastWriter(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:v1", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: app\n    newTag: v9\n  - name: app\n    newTag: v9\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:v10", nil))

	require.Equal(t, "app:v1", imageOf(t, out))
	require.Len(t, edits, 1)
	require.Equal(t, 1, edits[0].Edit.EntryIndex, "the LAST matching entry is the one that renders")
	require.Equal(t, "v10", edits[0].Edit.Value)
}

// Live equal to the rendered value is a full no-op: the source file's "stale" tag is dead text
// the entry shadows, and must not be overwritten.
func TestSplitDesired_LiveMatchesRenderIsNoOp(t *testing.T) {
	files := imagesEntry("images:\n  - name: ghcr.io/example/app\n    newTag: \"1.5.0\"\n")

	out, edits := splitFixture(t, files, "deployment.yaml",
		liveDeployment("ghcr.io/example/app:1.5.0", nil))

	require.Equal(t, "ghcr.io/example/app:1.0.0", imageOf(t, out), "the source keeps its bytes")
	require.Empty(t, edits)
}

// The matching entry declares only newName, so the TAG is file-supplied: it flows into the
// source image while the name stays in its source form.
func TestSplitDesired_UngovernedComponentPatchesFile(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("old/app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: old/app\n    newName: new/app\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("new/app:2.0.0", nil))

	require.Equal(t, "old/app:2.0.0", imageOf(t, out), "the source name stays; the live tag lands in the file")
	require.Empty(t, edits, "no entry supplies the tag")
}

// A live name change whose supplier is a newName entry updates the entry.
func TestSplitDesired_NameChangeRoutedToNewName(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("old/app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: old/app\n    newName: new/app\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("mirror/app:1.0.0", nil))

	require.Equal(t, "old/app:1.0.0", imageOf(t, out), "the source is untouched")
	require.Len(t, edits, 1)
	require.Equal(t, "newName", edits[0].Edit.Field)
	require.Equal(t, "mirror/app", edits[0].Edit.Value)
}

// A digest entry replaces the TAG as well as the digest — kustomize: "overriding tag or digest
// will replace both original tag and digest values" — so `app@sha256:...` is the only thing
// this folder can render, and the live object carries no tag.
func TestSplitDesired_DigestRoutedToEntry(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: app\n    digest: sha256:bbb\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app@sha256:ccc", nil))

	require.Equal(t, "app:1.0.0", imageOf(t, out), "the source keeps its tag")
	require.Len(t, edits, 1)
	require.Equal(t, manifestedit.KustomizationEdit{
		Section: manifestedit.KustomizationSectionImages, EntryIndex: 0,
		EntryName: "app", Field: "digest", Value: "sha256:ccc",
	}, edits[0].Edit)
}

// The #231 corruption, pinned so it cannot come back.
//
// Source `app:1.0.0`, an entry supplying only a digest. kustomize renders `app@sha256:bbb` —
// the digest REPLACES the tag. The hand-written transformer believed the render was
// `app:1.0.0@sha256:bbb`, so on seeing the real live object (no tag) it concluded the user had
// removed the tag, and rewrote `app:1.0.0` to `app` in the source file. On every reconcile,
// silently. Nothing about the source document may change here.
func TestSplitDesired_DigestEntryDoesNotStripTheSourceTag(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: app\n    digest: sha256:bbb\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app@sha256:bbb", nil))

	require.Equal(t, "app:1.0.0", imageOf(t, out), "the tag must NOT be stripped out of the source")
	require.Empty(t, edits, "live already matches the render")
}

// Live drops the digest an entry supplies. There is no way to say "no digest" on an entry that
// sets one, so nothing routes and the object writes through — where the oracle then adjudicates
// it, because a write-through of a governed field does not converge.
func TestSplitDesired_RemovalIsUnroutable(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith(
			"images:\n  - name: app\n    digest: sha256:abc\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:1.0.0", nil))

	require.Equal(t, "app:1.0.0", imageOf(t, out), "the live value is written through")
	require.Empty(t, edits)
}

// Two containers governed by one entry cannot pin two different tags on it.
func TestSplitDesired_ConflictingContainersAbandonRouting(t *testing.T) {
	source := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: a
          image: app:1.0.0
        - name: b
          image: app:1.0.0
`
	files := []manifestedit.FileContent{
		file("deployment.yaml", source),
		file("kustomization.yaml", kustomizationWith("images:\n  - name: app\n    newTag: \"1.5.0\"\n")),
	}
	live := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "web"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "a", "image": "app:2.0.0"},
						map[string]interface{}{"name": "b", "image": "app:3.0.0"},
					},
				},
			},
		},
	}

	out, edits := splitFixture(t, files, "deployment.yaml", live)

	require.Empty(t, edits, "one entry cannot carry two different tags")
	slots := collectImageSlots(out.Object)
	require.Len(t, slots, 2)
	require.Equal(t, "app:2.0.0", slots[0].image, "write-through keeps the live values")
	require.Equal(t, "app:3.0.0", slots[1].image)
}

// A base renames the image and the overlay pins the tag OF THE RENAMED IMAGE. A live tag change
// lands on the overlay's entry.
//
// This is also the rename-chain guard doing its job: the overlay's name: matches the base's
// newName:, so dyeing newName would stop the overlay's entry matching and change the render's
// shape. Names therefore go undyed here — and the TAG is still attributed exactly, because a
// tag dye is a pure sink even inside a rename chain.
func TestSplitDesired_ChainedEntriesCompose(t *testing.T) {
	files := []manifestedit.FileContent{
		file("base/deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("base/kustomization.yaml",
			"resources:\n  - deployment.yaml\nimages:\n  - name: app\n    newName: mirror/app\n"),
		file("kustomization.yaml",
			"resources:\n  - base\nimages:\n  - name: mirror/app\n    newTag: \"2.0.0\"\n"),
	}

	out, edits := splitFixture(t, files, "base/deployment.yaml",
		liveDeployment("mirror/app:9.9.9", nil))

	require.Equal(t, "app:1.0.0", imageOf(t, out), "the source is untouched")
	require.Len(t, edits, 1)
	require.Equal(t, "kustomization.yaml", edits[0].KustomizationPath, "the OVERLAY's entry owns the tag")
	require.Equal(t, "newTag", edits[0].Edit.Field)
	require.Equal(t, "9.9.9", edits[0].Edit.Value)
}

// B1: an images: entry's name: is a REGULAR EXPRESSION, and kustomize matches on it as one.
// Our matcher was string equality, so we believed `- name: "ap."` matched nothing while
// kustomize rewrote the image — and the projection then read the difference as a user edit and
// wrote the rendered value into the source manifest, killing the entry. The dye cannot make
// that mistake: it does not match anything, it reads where kustomize's own nonce came out.
func TestSplitDesired_RegexEntryNameIsAttributed(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("kustomization.yaml", kustomizationWith("images:\n  - name: \"ap.\"\n    newTag: \"1.5.0\"\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:2.0.0", nil))

	require.Equal(t, "app:1.0.0", imageOf(t, out), "the source keeps its bytes; the regex entry owns the tag")
	require.Len(t, edits, 1, "kustomize matched this entry, so we must attribute to it")
	require.Equal(t, "newTag", edits[0].Edit.Field)
	require.Equal(t, "2.0.0", edits[0].Edit.Value)
}

// A pinned count absorbs the live scale; the file's replicas field is restored to its source
// form — here, ABSENT, because the transformer creates the field.
func TestSplitDesired_ReplicasRoutedToEntry(t *testing.T) {
	five := int64(5)
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")), // no spec.replicas
		file("kustomization.yaml", kustomizationWith("replicas:\n  - name: web\n    count: 3\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:1.0.0", &five))

	_, has, _ := unstructured.NestedInt64(out.Object, "spec", "replicas")
	require.False(t, has, "the source has no replicas field, so the desired-for-file must not either")
	require.Len(t, edits, 1)
	require.Equal(t, manifestedit.KustomizationEdit{
		Section: manifestedit.KustomizationSectionReplicas, EntryIndex: 0,
		EntryName: "web", Field: "count", Value: "5",
	}, edits[0].Edit)
}

// Live equals the pinned count: the source's own (stale) value is restored and nothing routes.
func TestSplitDesired_ReplicasMatchRestoresSource(t *testing.T) {
	three := int64(3)
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "1")),
		file("kustomization.yaml", kustomizationWith("replicas:\n  - name: web\n    count: 3\n")),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:1.0.0", &three))

	count, _, _ := unstructured.NestedInt64(out.Object, "spec", "replicas")
	require.Equal(t, int64(1), count, "the source's own value is restored")
	require.Empty(t, edits)
}

// B2: kustomize's replica fieldspec is Deployment, ReplicaSet, StatefulSet AND
// ReplicationController. isReplicaKind listed three of the four, so a scale on an RC governed
// by a replicas: entry was written into the source document, where the transformer overrode it
// straight back — non-converging drift, silently, forever.
//
// There is no list of kinds any more. kustomize's fieldspec is the authority, and the dye
// reports what it did.
func TestSplitDesired_ReplicationControllerIsGoverned(t *testing.T) {
	source := `apiVersion: v1
kind: ReplicationController
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: app:1.0.0
`
	files := []manifestedit.FileContent{
		file("rc.yaml", source),
		file("kustomization.yaml", "resources:\n  - rc.yaml\nreplicas:\n  - name: web\n    count: 3\n"),
	}
	live := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ReplicationController",
		"metadata":   map[string]interface{}{"name": "web"},
		"spec": map[string]interface{}{
			"replicas": int64(7),
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": "app:1.0.0"},
					},
				},
			},
		},
	}

	out, edits := splitFixture(t, files, "rc.yaml", live)

	_, has, _ := unstructured.NestedInt64(out.Object, "spec", "replicas")
	require.False(t, has, "the source has no replicas field; the entry supplies it")
	require.Len(t, edits, 1, "kustomize DOES scale a ReplicationController, so the count must route")
	require.Equal(t, "count", edits[0].Edit.Field)
	require.Equal(t, "7", edits[0].Edit.Value)
}

// A document no replicas: entry names gets no dyed count, so nothing governs its spec.replicas
// and a live scale simply writes through into the file.
//
// The second workload is not padding: kustomize REFUSES to build a folder whose replicas: entry
// matches nothing ("resource with name other does not match a config with the following GVK
// [Deployment StatefulSet ReplicaSet ReplicationController]"). Which is also kustomize stating
// its own replica fieldspec out loud — all four kinds, the fourth being the one isReplicaKind
// forgot.
func TestSplitDesired_UnmatchedNameLeavesReplicasToTheFile(t *testing.T) {
	other := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: other
spec:
  template:
    spec:
      containers:
        - name: app
          image: app:1.0.0
`
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "1")),
		file("other.yaml", other),
		file("kustomization.yaml",
			"resources:\n  - deployment.yaml\n  - other.yaml\nreplicas:\n  - name: other\n    count: 3\n"),
	}
	five := int64(5)

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:1.0.0", &five))

	count, _, _ := unstructured.NestedInt64(out.Object, "spec", "replicas")
	require.Equal(t, int64(5), count, "no entry names web, so the source file owns its count")
	require.Empty(t, edits)
}

// B3, both halves, measured against kustomize:
//
//   - volumes[].image.reference IS rewritten by the image transformer. The old collector never
//     looked at it, so the rendered value was mirrored back into the source document as though
//     the user had typed it. It must now route to the entry like any other image.
//   - ephemeralContainers are NOT rewritten. No dye lands there, so no entry is credited, and
//     the change belongs in the source file — which is exactly where it goes.
func TestSplitDesired_VolumeImageRoutesAndEphemeralContainerDoesNot(t *testing.T) {
	source := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: app:1.0.0
      ephemeralContainers:
        - name: debug
          image: app:1.0.0
      volumes:
        - name: vol
          image:
            reference: app:1.0.0
`
	files := []manifestedit.FileContent{
		file("deployment.yaml", source),
		file("kustomization.yaml", kustomizationWith("images:\n  - name: app\n    newTag: \"1.5.0\"\n")),
	}
	// Live: the container and the VOLUME render at 1.5.0 (kustomize rewrote both); the
	// ephemeral container still renders at the source value. The user bumps the entry's tag to
	// 2.0.0 and independently edits the ephemeral container.
	live := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "web"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": "app:2.0.0"},
					},
					"ephemeralContainers": []interface{}{
						map[string]interface{}{"name": "debug", "image": "app:9.9.9"},
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name":  "vol",
							"image": map[string]interface{}{"reference": "app:2.0.0"},
						},
					},
				},
			},
		},
	}

	out, edits := splitFixture(t, files, "deployment.yaml", live)

	require.Len(t, edits, 1, "the container and the volume agree on 2.0.0, so one entry edit carries both")
	require.Equal(t, "newTag", edits[0].Edit.Field)
	require.Equal(t, "2.0.0", edits[0].Edit.Value)

	slots := map[string]string{}
	for _, s := range collectImageSlots(out.Object) {
		slots[s.key] = s.image
	}
	require.Contains(t, slots, "/spec/template/spec/volumes\x00vol")
	require.Equal(t, "app:1.0.0", slots["/spec/template/spec/volumes\x00vol"],
		"the volume image is entry-governed, so the source keeps its bytes")
	require.Equal(t, "app:1.0.0", slots["/spec/template/spec/containers\x00app"],
		"the container image is entry-governed, so the source keeps its bytes")
	require.Equal(t, "app:9.9.9", slots["/spec/template/spec/ephemeralContainers\x00debug"],
		"kustomize does not rewrite ephemeralContainers, so the SOURCE FILE owns this one")
}

// A document no entry governs at all attributes nothing, and the whole object writes through.
func TestSplitDesired_NoOverridesWritesThrough(t *testing.T) {
	files := []manifestedit.FileContent{
		file("deployment.yaml", deploymentSource("app:1.0.0", "")),
		file("kustomization.yaml", "resources:\n  - deployment.yaml\n"),
	}

	out, edits := splitFixture(t, files, "deployment.yaml", liveDeployment("app:2.0.0", nil))

	require.Empty(t, edits)
	require.Equal(t, "app:2.0.0", imageOf(t, out), "nothing governs it, so the live value lands in the file")
}
