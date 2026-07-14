// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TOLERATE, DON'T AUTHOR — at the commit, which is where it has to be true.
//
// A folder with a patch is now ADOPTED, and the two edit-through channels work in it exactly as
// they do anywhere else: that is the whole point, because a patch on a replica count has nothing
// to do with an image tag, and refusing the folder refused both.
//
// What is still refused is AUTHORING: an edit to a field the patch owns has nowhere to land, and
// the re-render says so rather than committing a write the build overrides straight back.

// The patch pins spec.replicas. The images: entry supplies the tag. They are unrelated, and until
// now the patch refused the whole GitTarget and took the tag with it.
const patchedKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - apps/deployment.yaml
patches:
  - path: apps/replicas-patch.yaml
images:
  - name: ghcr.io/example/podinfo
    newTag: "6.4.0"
`

const replicasPatchYAML = `# The production overlay pins the replica count. It is a PARTIAL object.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 5
`

func seedPatchedWorktree(t *testing.T, root string) (string, string, string) {
	t.Helper()
	deployPath := filepath.Join(root, "apps", "deployment.yaml")
	patchPath := filepath.Join(root, "apps", "replicas-patch.yaml")
	kustPath := filepath.Join(root, "kustomization.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(deployPath), 0o750))
	require.NoError(t, os.WriteFile(deployPath, []byte(overridesDeploymentYAML), 0o600))
	require.NoError(t, os.WriteFile(patchPath, []byte(replicasPatchYAML), 0o600))
	require.NoError(t, os.WriteFile(kustPath, []byte(patchedKustomizationYAML), 0o600))
	return deployPath, patchPath, kustPath
}

// THE MILESTONE, in one test: a folder carrying a patch is adopted, and an image bump still routes
// to the images: entry. The patch is read-only context — it is not touched, and it is not read.
func TestPlanFlush_PatchedFolderStillRoutesAnImageBumpToTheEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, patchPath, kustPath := seedPatchedWorktree(t, worktree.Filesystem.Root())

	// The live object is what the folder renders: the patch's 5 replicas, the entry's 6.4.0 tag —
	// with the tag bumped to 6.5.0, which is the user's edit.
	changed, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.5.0", 5))
	require.NoError(t, err)
	require.True(t, changed)

	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), `newTag: "6.5.0"`, "the tag belongs on the entry, patch or no patch")

	assertFileBytes(t, deployPath, overridesDeploymentYAML,
		"the source manifest keeps its bytes: neither the tag nor the patched replica count lands here")
	assertFileBytes(t, patchPath, replicasPatchYAML,
		"and the patch is READ-ONLY context — tolerating it is not authoring it")
}

// The folder is in sync. The patch says 5 replicas and the cluster runs 5. Nothing to do — and in
// particular the base manifest must not absorb the overlay's 5, which is the corruption that made
// tolerating patches unsafe until the projection stopped writing what the build supplies.
func TestPlanFlush_PatchedFolderInSyncIsANoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, patchPath, kustPath := seedPatchedWorktree(t, worktree.Filesystem.Root())

	changed, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.4.0", 5))

	require.NoError(t, err)
	assert.False(t, changed, "the live object is exactly what the folder renders")
	assertFileBytes(t, deployPath, overridesDeploymentYAML,
		"the patch's replica count is the OVERLAY's, and it must not be baked into the base")
	assertFileBytes(t, patchPath, replicasPatchYAML, "nor may the patch itself be rewritten")
	assertFileBytes(t, kustPath, patchedKustomizationYAML, "nor the kustomization")
}

// AND AUTHORING IS STILL REFUSED. Scaling the Deployment is an edit to a field the PATCH owns:
// there is no replicas: entry, the source file cannot hold it (the patch would stamp 5 back on the
// next render), and writing a patch from scratch is not supported.
//
// So the flush is refused, loudly, naming the object — instead of committing a write that would
// leave the resource permanently un-mirrored while looking like it worked. That is the same
// refusal a patch-owned field got before this change; what has changed is that it is now the EDIT
// that is refused, not the whole GitTarget.
func TestPlanFlush_RefusesAnEditToAFieldThePatchOwns(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, patchPath, kustPath := seedPatchedWorktree(t, worktree.Filesystem.Root())

	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.4.0", 9))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "a patch-owned edit must be refused, and refused legibly")
	assert.Contains(t, refused.Error(), "Deployment/web", "the refusal names the object")
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderRefused),
		"the renderer is what refused it, so it surfaces as WriteBoundaryRefused")

	assertFileBytes(t, deployPath, overridesDeploymentYAML, "a refused flush writes nothing")
	assertFileBytes(t, patchPath, replicasPatchYAML, "and above all does not try to author the patch")
	assertFileBytes(t, kustPath, patchedKustomizationYAML, "and touches no kustomization")
}
