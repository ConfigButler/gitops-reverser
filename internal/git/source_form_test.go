// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// THE BUILD'S OUTPUT MUST NOT BECOME THE BUILD'S INPUT.
//
// The writer mirrors a live object into the file that produced it. Under kustomize that file is
// not what the cluster runs, and mirroring the live object straight back wrote the OVERLAY's
// values into the BASE — measured, on a folder the operator accepts today, with nothing changed
// in the cluster and nothing changed in the render.
//
// These pin it at the commit, which is where it bit.

// labelledKustomizationYAML injects metadata into every object it renders. It is a supported
// folder: `labels` and `commonAnnotations` are on the modelled list, and there is no patch and no
// generator anywhere near it.
const labelledKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - apps/deployment.yaml
labels:
  - pairs:
      env: prod
    includeSelectors: false
commonAnnotations:
  owner: platform
`

const labelledDeploymentYAML = `apiVersion: apps/v1
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
          image: ghcr.io/example/podinfo:6.3.0 # hand-authored, and it stays that way
`

// labelledLiveDeployment is the object as the CLUSTER holds it — which is the RENDER: the overlay's
// label and annotation are on it, because kustomize put them there before Flux applied it.
func labelledLiveDeployment(labelValue string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":        "web",
				"namespace":   "default",
				"labels":      map[string]interface{}{"env": labelValue},
				"annotations": map[string]interface{}{"owner": "platform"},
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels":      map[string]interface{}{"app": "web"},
						"annotations": map[string]interface{}{"owner": "platform"},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "podinfo", "image": "ghcr.io/example/podinfo:6.3.0"},
						},
					},
				},
			},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "web",
		},
		Operation: "UPDATE",
	}
}

func seedLabelledWorktree(t *testing.T, root string) (string, string) {
	t.Helper()
	deployPath := filepath.Join(root, "apps", "deployment.yaml")
	kustPath := filepath.Join(root, "kustomization.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(deployPath), 0o750))
	require.NoError(t, os.WriteFile(deployPath, []byte(labelledDeploymentYAML), 0o600))
	require.NoError(t, os.WriteFile(kustPath, []byte(labelledKustomizationYAML), 0o600))
	return deployPath, kustPath
}

// The folder is in sync: the live object IS the render. So the flush must do NOTHING.
//
// It used to write `env: prod` and `owner: platform` into the base manifest — the overlay's
// metadata, absorbed into the file the overlay renders, as if the author had typed it. Every
// reconcile of an unchanged folder produced a commit, and the file was left wrong: remove the
// kustomization later and the drift is permanent.
func TestPlanFlush_InjectedMetadataIsNeverWrittenIntoTheSourceManifest(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedLabelledWorktree(t, worktree.Filesystem.Root())

	changed, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		labelledLiveDeployment("prod"))

	require.NoError(t, err)
	assert.False(t, changed, "the live object is exactly what the folder renders: there is nothing to write")
	assertFileBytes(t, deployPath, labelledDeploymentYAML,
		"the overlay's labels and annotations belong to the BUILD, not to the file it renders")
	assertFileBytes(t, kustPath, labelledKustomizationYAML, "and the kustomization is untouched")
}

// A field the build does NOT supply is still the user's, and still lands in the file. The fix
// must not turn a kustomize folder into a read-only one.
func TestPlanFlush_UngovernedFieldStillLandsInTheSourceManifest(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, _ := seedLabelledWorktree(t, worktree.Filesystem.Root())

	event := labelledLiveDeployment("prod")
	containers, _, err := unstructured.NestedSlice(event.Object.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	containers[0].(map[string]interface{})["image"] = "ghcr.io/example/podinfo:6.5.0"
	require.NoError(t, unstructured.SetNestedSlice(
		event.Object.Object, containers, "spec", "template", "spec", "containers"))

	changed, err := flushEventsForTest(t, writer, worktree, deploymentMapper(), event)
	require.NoError(t, err)
	require.True(t, changed)

	deploy, err := os.ReadFile(deployPath)
	require.NoError(t, err)
	assert.Contains(t, string(deploy), "ghcr.io/example/podinfo:6.5.0",
		"no images: entry governs the tag, so the change belongs in the file")
	assert.Contains(t, string(deploy), "# hand-authored, and it stays that way",
		"and it is an in-place edit, so the comment survives")
	assert.NotContains(t, string(deploy), "env: prod",
		"the injected label still has no business in the source file")
}

// And a change to a field the BUILD supplies is refused, not absorbed. The user relabels the live
// Deployment; no entry can express it and the source file cannot hold it — the overlay would
// stamp `env: prod` straight back on the next render. The re-render sees exactly that and refuses
// the flush, which is the reported outcome the design demands over a write that never converges.
//
// The oracle now runs for ANY document a render root produces. It used to run only when an
// images:/replicas: entry existed somewhere in the chain — and this folder has none, so this write
// went through unchecked and silently failed to converge, forever.
func TestPlanFlush_RefusesAChangeToABuildSuppliedField(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedLabelledWorktree(t, worktree.Filesystem.Root())

	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		labelledLiveDeployment("staging"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the flush must be refused, and refused legibly")
	assert.Contains(t, refused.Error(), "Deployment/web", "the refusal names the object")
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderRefused),
		"the renderer is what refused it, so it surfaces as WriteBoundaryRefused")

	assertFileBytes(t, deployPath, labelledDeploymentYAML, "a refused flush writes nothing")
	assertFileBytes(t, kustPath, labelledKustomizationYAML, "and touches no kustomization")
}
