// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
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
func sharedImageEvent(name, image string) Event {
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
