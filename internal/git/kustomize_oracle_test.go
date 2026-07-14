// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// flushEventsForTest is applyEventsViaPlanFlushWithMapper's sibling for the cases that are
// ABOUT the error: a refused flush returns one, and swallowing it with require.NoError
// would assert the opposite of the thing under test.
func flushEventsForTest(
	t *testing.T,
	writer *contentWriter,
	worktree *gogit.Worktree,
	mapper typeset.Lookup,
	events ...Event,
) (bool, error) {
	t.Helper()
	w := &BranchWorker{contentWriter: writer, mapper: mapper}
	return w.flushEventsToWorktree(context.Background(), worktree, "", events, nil)
}

// Before a kustomize-governed write is committed, the repository is re-rendered WITH it
// and the result is required to be exactly the live object, with every other rendered
// object untouched. These are the tests for the refusal — the happy path is already
// covered by inplace_overrides_test.go, and a check that only ever says yes is not a
// check.
//
// See docs/design/support-boundary/render-attribution.md §5.

// A base pinned at one tag, an overlay that shadows it, and a SECOND deployment the same
// images: entry also matches. Converging web's tag by editing the entry would drag api's
// image along with it — the entry is shared context. The oracle re-renders, sees api move,
// and refuses the whole write rather than committing a change to a resource nobody asked
// to change.
const sharedEntryKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - apps/web.yaml
  - apps/api.yaml
images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`

func sharedImageDeploymentYAML(name string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
spec:
  selector:
    matchLabels:
      app: ` + name + `
  template:
    metadata:
      labels:
        app: ` + name + `
    spec:
      containers:
        - name: main
          image: ghcr.io/example/shared:0.9.0
`
}

func seedSharedEntryWorktree(t *testing.T, root string) (string, string, string) {
	t.Helper()
	webPath := filepath.Join(root, "apps", "web.yaml")
	apiPath := filepath.Join(root, "apps", "api.yaml")
	kustPath := filepath.Join(root, "kustomization.yaml")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "apps"), 0o750))
	require.NoError(t, os.WriteFile(webPath, []byte(sharedImageDeploymentYAML("web")), 0o600))
	require.NoError(t, os.WriteFile(apiPath, []byte(sharedImageDeploymentYAML("api")), 0o600))
	require.NoError(t, os.WriteFile(kustPath, []byte(sharedEntryKustomizationYAML), 0o600))
	return webPath, apiPath, kustPath
}

// sharedImageEvent is the live Deployment as the cluster holds it: the rendered form,
// which is what the source file plus the overlay's entry must reproduce.
func sharedImageEvent(name, image string) Event { //nolint:unparam // image varies by intent, not by call today
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": name}},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": name}},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "main", "image": image},
						},
					},
				},
			},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: name,
		},
		Operation: "UPDATE",
	}
}

// Routing web's new tag onto the entry would re-render api at that tag too, and api's live
// state says otherwise. The proposal is not isolated, so the flush is refused — loudly, as
// an AcceptanceRefusedError that reaches the GitTarget — and not one byte is written.
//
// A refusal, not a silent skip: a write that does not survive the re-render is one that
// would never converge, and absorbing it would leave the resource un-mirrored forever with
// nothing to show for it (render-attribution.md §7).
func TestPlanFlush_RefusesAWriteThatDragsASiblingAlong(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	webPath, apiPath, kustPath := seedSharedEntryWorktree(t, worktree.Filesystem.Root())

	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		sharedImageEvent("web", "ghcr.io/example/shared:2.0.0"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the flush must be refused, and refused legibly")
	assert.Contains(t, refused.Error(), "Deployment/api",
		"the refusal must name the object the write would have dragged along")
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderRefused),
		"it is a write-boundary refusal, so it surfaces as WriteBoundaryRefused")

	assertFileBytes(t, kustPath, sharedEntryKustomizationYAML,
		"the entry is shared context; editing it would move api too")
	assertFileBytes(t, webPath, sharedImageDeploymentYAML("web"), "a refused flush writes nothing")
	assertFileBytes(t, apiPath, sharedImageDeploymentYAML("api"), "and certainly leaves the sibling alone")
}

// A NEW resource landing in a kustomize directory that supplies its namespace must not be
// read as collateral damage by the oracle.
//
// The bytes written for it deliberately OMIT metadata.namespace — the kustomization's
// namespace: transformer puts it back, and every sibling in that directory follows the same
// convention. But the render therefore HAS a namespace, so an intent built from the
// namespace-stripped object demands a render that does not, and the whole flush is refused:
// the new resource is lost, and so is the perfectly good governed write batched with it.
// Recovery never comes, because resync replays the same batch.
//
// The write and the intent are different objects. This is the test that says so.
func TestPlanFlush_NewResourceInANamespaceInheritingDirIsNotCollateralDamage(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	// FLAT on purpose: the kustomization sits beside the manifests it lists, so a new
	// document lands in the same directory AND gets a resources: entry — which is what puts
	// it inside the render, where the oracle can see it. (With the manifests in a subfolder
	// the new file is never added to resources:, so it is not rendered at all and this case
	// cannot arise.)
	kustPath := filepath.Join(root, "kustomization.yaml")
	webPath := filepath.Join(root, "web.yaml")
	require.NoError(t, os.WriteFile(webPath, []byte(sharedImageDeploymentYAML("web")), 0o600))
	require.NoError(t, os.WriteFile(kustPath, []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - web.yaml
images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`), 0o600))

	changed, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		// A governed write, which is what turns the oracle on...
		sharedImageEvent("web", "ghcr.io/example/shared:2.0.0"),
		// ...and a brand-new resource in the same namespace-inheriting directory.
		newCacheDeploymentEvent("redis:7"),
	)

	require.NoError(t, err, "the flush must not be refused: both writes are exactly what we intended")
	require.True(t, changed)

	kust, readErr := os.ReadFile(kustPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(kust), `newTag: "2.0.0"`, "the governed write must still land")
	assert.Contains(t, string(kust), "cache.yaml", "and the new document must be added to the render")

	created := findFileContaining(t, root, "name: cache")
	require.NotEmpty(t, created, "the new resource must have been written")
	assert.NotContains(t, created, "namespace: default",
		"the directory supplies the namespace; the file must not repeat it")
}

// findFileContaining returns the content of the first YAML file under root holding needle.
// The new document's placement is the placement rules' business, not this test's.
func findFileContaining(t *testing.T, root, needle string) string {
	t.Helper()
	var found string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" || !strings.HasSuffix(path, ".yaml") {
			return nil //nolint:nilerr // a walk error simply means nothing was found here
		}
		b, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(b), needle) {
			found = string(b)
		}
		return nil
	}))
	return found
}

// A NEW resource whose image an existing images: entry overrides cannot be mirrored by
// writing the live value into a new file: the entry rewrites it on the very next render, so
// the committed file asserts a state the folder does not produce, and it never converges.
//
// We do not route a new document's values onto an entry — it has no override chain yet, it
// did not exist when the store was built — so the honest answer is that this live state cannot
// be expressed in this repository as it stands. The oracle says so, names the file and the
// object, and writes nothing. Before, this was committed silently and wrongly.
func TestPlanFlush_RefusesANewResourceAnEntryWouldOverride(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "web.yaml"),
		[]byte(sharedImageDeploymentYAML("web")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(
		`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - web.yaml
images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`), 0o600))

	// The new resource runs the image the entry matches, at a tag the entry does not produce.
	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		newCacheDeploymentEvent("ghcr.io/example/shared:2.0.0"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the entry would override the new file; that must not be committed silently")
	assert.Contains(t, refused.Error(), "Deployment/cache", "the refusal must name the object it cannot express")

	_, statErr := os.Stat(filepath.Join(root, "cache.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a refused flush writes nothing")
}

// newCacheDeploymentEvent is a live Deployment with no document in Git yet.
func newCacheDeploymentEvent(image string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "cache", "namespace": "default"},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "cache"}},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "cache"}},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "cache", "image": image},
						},
					},
				},
			},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "cache",
		},
		Operation: "CREATE",
	}
}

// The same tag bump, but both deployments move together: now the entry edit reproduces
// BOTH live objects, nothing else drifts, and the write lands. This is the control — it is
// what proves the refusal above is discriminating rather than a blanket "no".
func TestPlanFlush_AllowsTheSharedEntryWriteWhenEverySiblingAgrees(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	webPath, apiPath, kustPath := seedSharedEntryWorktree(t, worktree.Filesystem.Root())

	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(),
		sharedImageEvent("web", "ghcr.io/example/shared:2.0.0"),
		sharedImageEvent("api", "ghcr.io/example/shared:2.0.0"))

	require.True(t, changed, "both siblings agree on the new tag; the entry edit is now isolated")
	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), `newTag: "2.0.0"`, "the entry absorbs the tag both deployments now run")
	assertFileBytes(t, webPath, sharedImageDeploymentYAML("web"), "the source manifests keep their bytes")
	assertFileBytes(t, apiPath, sharedImageDeploymentYAML("api"), "the source manifests keep their bytes")
}
