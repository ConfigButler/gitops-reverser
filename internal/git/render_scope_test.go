// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gogit "github.com/go-git/go-git/v5"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// Render-root scoping (docs/design/support-boundary/render-root-scoping.md §4): a GitTarget
// whose spec.path is a kustomize overlay reads a base OUTSIDE its subtree through `../../base`
// as read-only context, while writes stay inside spec.path. The scan re-roots at the common
// ancestor (renderBase) so the whole store runs in one coordinate system; the write jail
// (writeSubdir) keeps a planned write inside the overlay.

const overlayBaseDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/example/podinfo:6.3.0 # base tag, shadowed by the overlay
`

const overlayBaseServiceYAML = `apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  selector:
    app: web
  ports:
    - port: 80
`

const overlayBaseKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - service.yaml
`

const overlayKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: production
resources:
  - ../../base
images:
  - name: ghcr.io/example/podinfo
    newTag: "6.4.0"
`

// overlayGitPath is the GitTarget subtree: a pure overlay whose only content is a
// kustomization that reads ../../base. Everything it deploys comes from the base.
const overlayGitPath = "apps/frontend/overlays/production"

// seedOverlayWorktree writes the base/ + overlays/production/ layout and returns the base
// deployment and overlay kustomization absolute paths (deployment first).
func seedOverlayWorktree(t *testing.T, root string) (string, string) {
	t.Helper()
	write := func(rel, content string) string {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
		return full
	}
	baseDeployPath := write("apps/frontend/base/deployment.yaml", overlayBaseDeploymentYAML)
	write("apps/frontend/base/service.yaml", overlayBaseServiceYAML)
	write("apps/frontend/base/kustomization.yaml", overlayBaseKustomizationYAML)
	overlayKustPath := write(overlayGitPath+"/kustomization.yaml", overlayKustomizationYAML)
	return baseDeployPath, overlayKustPath
}

// overlayDeploymentEvent is the rendered Deployment as the cluster runs it: namespace from
// the overlay's transformer, image from the overlay's images: entry.
func overlayDeploymentEvent(image string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "web", "namespace": "production"},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "podinfo", "image": image},
						},
					},
				},
			},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "production", Name: "web",
		},
		Operation: "UPDATE",
	}
}

func flushAtBase(
	t *testing.T,
	writer *contentWriter,
	worktree *gogit.Worktree,
	mapper typeset.Lookup,
	base string,
	events ...Event,
) (bool, error) {
	t.Helper()
	w := &BranchWorker{contentWriter: writer, mapper: mapper}
	return w.flushEventsToWorktree(context.Background(), worktree, base, events, nil)
}

// The read scope of a pure overlay re-roots at the base's parent, keeps every scanned path
// under the common ancestor, and reports the overlay itself as the write jail.
func TestScanRenderScope_ResolvesOverlayBase(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedOverlayWorktree(t, root)

	scoped, err := scanRenderScope(root, overlayGitPath)
	require.NoError(t, err)

	assert.Equal(t, "apps/frontend", scoped.renderBase, "renderBase is the common ancestor of the overlay and its base")
	assert.Equal(t, "overlays/production", scoped.writeSubdir, "the overlay is the write jail relative to renderBase")

	got := map[string]bool{}
	for _, f := range scoped.scan.YAMLFiles {
		got[f.Path] = true
	}
	for _, want := range []string{
		"overlays/production/kustomization.yaml",
		"base/deployment.yaml",
		"base/service.yaml",
		"base/kustomization.yaml",
	} {
		assert.True(t, got[want], "expected %q in the re-rooted scan", want)
	}
}

// A self-contained subtree (no out-of-scope base) resolves to the identity: renderBase equals
// spec.path and writeSubdir is empty, so nothing downstream changes.
func TestScanRenderScope_SelfContainedIsIdentity(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedOverridesWorktree(t, root) // kustomization + apps/deployment.yaml, all in-subtree

	scoped, err := scanRenderScope(root, "")
	require.NoError(t, err)
	assert.Empty(t, scoped.renderBase)
	assert.Empty(t, scoped.writeSubdir)
}

// The launch capability: an image tag bump on a base object the overlay renders lands on the
// OVERLAY's images: entry — inside spec.path — and the base file, which is read-only context
// outside spec.path, keeps its bytes.
func TestPlanFlush_Overlay_RoutesImageTagToOverlayEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	baseDeployPath, overlayKustPath := seedOverlayWorktree(t, worktree.Filesystem.Root())

	changed, err := flushAtBase(t, writer, worktree, deploymentMapper(), overlayGitPath,
		overlayDeploymentEvent("ghcr.io/example/podinfo:6.5.0"))
	require.NoError(t, err)
	require.True(t, changed, "the tag bump must land on the overlay entry")

	assertFileBytes(t, baseDeployPath, overlayBaseDeploymentYAML,
		"the base is read-only context; its bytes must not change")

	kust, err := os.ReadFile(overlayKustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), `newTag: "6.5.0"`, "the overlay entry absorbs the live tag")
	assert.NotContains(t, string(kust), `newTag: "6.4.0"`)
}

// Live state equal to the overlay's render is a full no-op across both files.
func TestPlanFlush_Overlay_InSyncIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	baseDeployPath, overlayKustPath := seedOverlayWorktree(t, worktree.Filesystem.Root())

	changed, err := flushAtBase(t, writer, worktree, deploymentMapper(), overlayGitPath,
		overlayDeploymentEvent("ghcr.io/example/podinfo:6.4.0"))
	require.NoError(t, err)
	assert.False(t, changed, "live == render must write nothing")

	assertFileBytes(t, baseDeployPath, overlayBaseDeploymentYAML, "base untouched")
	assertFileBytes(t, overlayKustPath, overlayKustomizationYAML, "overlay untouched")
}

// An edit to a base-owned field the overlay cannot express (here a new pod-template label)
// would have to be written into the base — outside the write jail. The whole flush is refused
// rather than writing through into shared context. This is the deferred patch-authoring case
// surfacing as an honest refusal, not a silent base write.
func TestPlanFlush_Overlay_BaseFieldEditRefused(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	baseDeployPath, overlayKustPath := seedOverlayWorktree(t, worktree.Filesystem.Root())

	event := overlayDeploymentEvent("ghcr.io/example/podinfo:6.4.0")
	// Add a base-owned field the images/replicas entries do not govern.
	spec := event.Object.Object["spec"].(map[string]interface{})
	tmpl := spec["template"].(map[string]interface{})
	meta := tmpl["metadata"].(map[string]interface{})
	meta["labels"].(map[string]interface{})["tier"] = "backend"

	_, err := flushAtBase(t, writer, worktree, deploymentMapper(), overlayGitPath, event)
	require.Error(t, err, "an edit that would write the read-only base must be refused")
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)

	assertFileBytes(t, baseDeployPath, overlayBaseDeploymentYAML, "the base must be left untouched on refusal")
	assertFileBytes(t, overlayKustPath, overlayKustomizationYAML, "nothing is committed on refusal")
}

// A base reference that climbs above the repository root is refused: the operator never reads
// outside the repository it manages.
func TestScanRenderScope_RefusesBaseEscapingRepoRoot(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	// spec.path is a single-level directory; ../../base climbs to ../base, above the root.
	kust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"
	full := filepath.Join(root, "app", "kustomization.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(kust), 0o600))

	_, err := scanRenderScope(root, "app")
	require.Error(t, err, "a base escaping the repository root must be refused")
	assert.Contains(t, err.Error(), "escapes the repository root")
}

// A base that itself reads another out-of-scope base is followed transitively, and renderBase
// climbs to the common ancestor of the overlay and every base in the chain. Foreign content in
// the overlay is carried through re-keyed, and a non-manifest file in a base is skipped (a base
// contributes only its manifests to the render).
func TestScanRenderScope_TransitiveBaseAndForeignRekey(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	// overlay -> ../../base -> ../shared: two hops, both out of spec.path.
	write("apps/frontend/overlays/production/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: production\nresources:\n  - ../../base\n")
	write("apps/frontend/overlays/production/notes.txt", "operator note, not a manifest\n")
	// A .yml (not .yaml) base kustomization, so readKustomization's fallback name is exercised.
	write("apps/frontend/base/kustomization.yml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"resources:\n  - deployment.yaml\n  - ../shared\n")
	write("apps/frontend/base/deployment.yaml", overlayBaseDeploymentYAML)
	write("apps/frontend/base/README.txt", "a base may carry non-manifest files; they are skipped\n")
	write("apps/frontend/shared/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - configmap.yaml\n")
	write("apps/frontend/shared/configmap.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\ndata:\n  k: v\n")

	scoped, err := scanRenderScope(root, "apps/frontend/overlays/production")
	require.NoError(t, err)
	assert.Equal(t, "apps/frontend", scoped.renderBase, "renderBase climbs to the ancestor of both bases")
	assert.Equal(t, "overlays/production", scoped.writeSubdir)

	got := map[string]bool{}
	for _, f := range scoped.scan.YAMLFiles {
		got[f.Path] = true
	}
	for _, want := range []string{
		"overlays/production/kustomization.yaml",
		"base/kustomization.yml", "base/deployment.yaml",
		"shared/kustomization.yaml", "shared/configmap.yaml",
	} {
		assert.True(t, got[want], "expected %q in the re-rooted scan", want)
	}
	assert.False(t, got["base/README.txt"], "a base's non-manifest file is not scanned as a manifest")

	foreign := map[string]bool{}
	for _, fe := range scoped.scan.Foreign {
		foreign[fe.Path] = true
	}
	assert.True(t, foreign["overlays/production/notes.txt"],
		"foreign content in the overlay is carried through re-keyed to render coordinates")
}

// A remote base is skipped when resolving read scope — it is refused before any build, never a
// directory to scan — while a local base beside it is still followed.
func TestScanRenderScope_SkipsRemoteBase(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	write("apps/frontend/overlays/production/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: production\nresources:\n  - ../../base\n"+
			"  - github.com/example-org/gitops//apps/remote?ref=v1.0.0\n")
	write("apps/frontend/base/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n")
	write("apps/frontend/base/deployment.yaml", overlayBaseDeploymentYAML)

	scoped, err := scanRenderScope(root, "apps/frontend/overlays/production")
	require.NoError(t, err, "a remote base is skipped, not an error")
	assert.Equal(t, "apps/frontend", scoped.renderBase)
	assert.Equal(t, "overlays/production", scoped.writeSubdir)
}

// The path helpers underpinning the re-root: common-ancestor, relative-under, and the
// minimal-directory collapse, at their boundary cases.
func TestRenderScopePathHelpers(t *testing.T) {
	ancestor := []struct {
		in   []string
		want string
	}{
		{[]string{"a/b/c", "a/b/d"}, "a/b"},
		{[]string{"a/b/c"}, "a/b/c"},
		{[]string{"a/b", "c/d"}, ""},      // no common prefix -> repo root
		{[]string{"a/b/c", "a/b"}, "a/b"}, // one contains the other
		{[]string{}, ""},
	}
	for _, c := range ancestor {
		assert.Equalf(t, c.want, commonAncestor(c.in), "commonAncestor(%v)", c.in)
	}

	assert.Equal(t, "overlays/prod", relUnder("apps", "apps/overlays/prod"))
	assert.Empty(t, relUnder("apps/x", "apps/x"), "a path relative to itself is empty")
	assert.Equal(t, "apps/x", relUnder("", "apps/x"), "an empty ancestor (repo root) returns the child")

	assert.ElementsMatch(t, []string{"a"}, minimalDirs([]string{"a", "a/b", "a/b/c"}),
		"nested directories collapse to the top-level root")
	assert.ElementsMatch(t, []string{"a", "b"}, minimalDirs([]string{"a", "b"}),
		"unrelated directories are all kept")
}

// The generalised write-fan-in guard: a base reached by more than one render root — with NO
// override entries at stake anywhere — is refused for in-place editing. The former check only
// fired on override ambiguity, so this shared-base field write-through is exactly the hole
// render-root scoping closes. spec.path is the common parent, so the shared base is in scope
// and only ReachedByMultipleRenderRoots (not OverridesAmbiguousAt) can catch it.
func TestFanInPrecondition_RefusesSharedBaseWriteThroughWithoutOverrides(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	plainOverlay := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
		"namespace: shared\nresources:\n  - ../base\n"
	files := map[string]string{
		"a/kustomization.yaml":    plainOverlay,
		"b/kustomization.yaml":    plainOverlay,
		"base/kustomization.yaml": "resources:\n  - deployment.yaml\n",
		"base/deployment.yaml":    overlayBaseDeploymentYAML,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	// A live image change on the shared Deployment has no images: entry to route to, so it
	// would fall back to a source-form write into base/deployment.yaml — the file both a and b
	// render.
	event := overlayDeploymentEvent("ghcr.io/example/podinfo:9.9.9")
	event.Identifier.Namespace = "shared"
	event.Object.SetNamespace("shared")

	_, err := flushAtBase(t, writer, worktree, deploymentMapper(), "", event)
	require.Error(t, err)
	assert.Contains(t, refusalIssueKinds(t, err), manifestanalyzer.IssueWriteFanIn,
		"a write into a base reached by two render roots must be refused even with no overrides")
	assertFileBytes(t, filepath.Join(root, "base", "deployment.yaml"), overlayBaseDeploymentYAML,
		"the shared base must keep its bytes")
}
