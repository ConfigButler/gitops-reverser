// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// KustomizationBuildRefs reports the local files a kustomization loads: its resources+bases
// graph (deprecated bases: folded into resources:) and its patch path: files. It is what the
// live writer follows to find the out-of-scope files an overlay reads.
func TestKustomizationBuildRefs(t *testing.T) {
	resources, patches, ok := KustomizationBuildRefs([]byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n  - ../../base\n  - service.yaml\n" +
			"patches:\n  - path: ../shared/patch.yaml\n  - patch: \"- op: remove\\n  path: /x\"\n"))
	require.True(t, ok)
	assert.Equal(t, []string{"../../base", "service.yaml"}, resources)
	assert.Equal(t, []string{"../shared/patch.yaml"}, patches, "only path: patches name a file; inline is omitted")

	bases, _, ok := KustomizationBuildRefs([]byte("bases:\n  - ../base\n"))
	require.True(t, ok)
	assert.Equal(t, []string{"../base"}, bases, "deprecated bases: folds into resources:")

	_, _, ok = KustomizationBuildRefs([]byte("resources: not-a-list\n"))
	assert.False(t, ok, "an unparseable kustomization is not followed")
}

func TestIsRemoteBaseEntry(t *testing.T) {
	assert.True(t, IsRemoteBaseEntry("github.com/example-org/gitops//apps/base?ref=v1.4.0"))
	assert.True(t, IsRemoteBaseEntry("https://example.com/base"))
	assert.True(t, IsRemoteBaseEntry("git@github.com:example/repo.git"))
	assert.False(t, IsRemoteBaseEntry("../../base"))
	assert.False(t, IsRemoteBaseEntry("deployment.yaml"))
}

// ReachedByMultipleRenderRoots is the generalised write-fan-in signal: a base file two
// overlays render is flagged, a self-contained root's own files are not.
func TestReachedByMultipleRenderRoots(t *testing.T) {
	overlay := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../base\n"
	files := []manifestedit.FileContent{
		{Path: "a/kustomization.yaml", Content: []byte(overlay)},
		{Path: "b/kustomization.yaml", Content: []byte(overlay)},
		{Path: "base/kustomization.yaml", Content: []byte("resources:\n  - deployment.yaml\n")},
		{Path: "base/deployment.yaml", Content: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n")},
	}
	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())

	assert.True(t, store.ReachedByMultipleRenderRoots("base/deployment.yaml"),
		"a base two render roots read is fan-in > 1")
	assert.False(t, store.ReachedByMultipleRenderRoots("a/kustomization.yaml"),
		"a render root's own kustomization is not a shared resource file")
}

// A file listed as BOTH a resource and a patch is a resource first — materialised and
// mirrored, never silently retained as a build input. Before, the global patch-file set
// retained any matching file, so a valid resource that was also patched vanished from the
// store and was never mirrored.
func TestPatchFiles_DualRoleResourceIsMirrored(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
				"resources:\n  - cm.yaml\npatches:\n  - path: cm.yaml\n")},
		{Path: "cm.yaml", Content: []byte(
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  k: v\n")},
	}
	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())
	if _, managed := store.FilesByPath["cm.yaml"]; !managed {
		t.Fatalf("a file that is both a resource and a patch must be managed, not retained")
	}
}

// A single render root reaching its own nested base is fan-in = 1: nothing is flagged, so the
// self-contained kustomize-single layout is still edited in place.
func TestReachedByMultipleRenderRoots_SingleRootNotFlagged(t *testing.T) {
	files := []manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - base\n")},
		{Path: "base/kustomization.yaml", Content: []byte("resources:\n  - deployment.yaml\n")},
		{Path: "base/deployment.yaml", Content: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n")},
	}
	store := BuildStoreFromFiles(context.Background(), files, nil, WriterAllowlist())
	assert.False(t, store.ReachedByMultipleRenderRoots("base/deployment.yaml"),
		"a base reached by a single root is fan-in = 1 and edited through normally")
}
