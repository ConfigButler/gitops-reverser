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

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const hardKustomizeYAML = "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
	"kind: Kustomization\n" +
	"resources:\n  - cm.yaml\n" +
	"patches:\n  - path: patch.yaml\n"

const plainKustomizeYAML = "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
	"kind: Kustomization\n" +
	"resources:\n  - cm.yaml\n"

// A GitTarget subtree holding a kustomization.yaml that uses an unsupported feature is
// refused: the live flush returns an *AcceptanceRefusedError naming the file and writes
// nothing, so the operator never edits a folder it cannot safely manage.
func TestPlanFlush_RefusesUnsupportedKustomizeFolder(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	seedPlacedManifest(t, worktree, "kustomization.yaml", hardKustomizeYAML)

	w := &BranchWorker{contentWriter: writer}
	event := cmEvent("CREATE", "fresh", "green")
	_, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{event}, nil)

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "flush must refuse with *AcceptanceRefusedError")
	assert.Contains(t, refused.Error(), "kustomization.yaml", "the refusal must name the offending file")

	// Nothing was written: the canonical ConfigMap path must not exist.
	canonical := filepath.Join(root, writer.filePathForIdentifier(event.Identifier))
	_, statErr := os.Stat(canonical)
	assert.True(t, os.IsNotExist(statErr), "a refused folder must not be written into")
}

// A plain kustomization (namespace/resources only) is auxiliary input, never a refusal:
// it is retained and the writer keeps editing the managed resources beside it.
func TestPlanFlush_AcceptsPlainKustomizeFolder(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	seedPlacedManifest(t, worktree, "kustomization.yaml", plainKustomizeYAML)

	w := &BranchWorker{contentWriter: writer}
	create := []Event{cmEvent("CREATE", "fresh", "green")}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", create, nil)
	require.NoError(t, err, "a plain kustomization must not be refused")
	assert.True(t, changed, "the ConfigMap must be written beside the retained kustomization")
}

// The operator's own bootstrap artifact (.sops.yaml creation rules) is non-KRM YAML, but
// it is the writer's own file and must never trip the acceptance gate.
func TestPlanFlush_DoesNotRefuseOwnSopsConfig(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	seedPlacedManifest(t, worktree, ".sops.yaml", "creation_rules:\n  - path_regex: .*\n")

	w := &BranchWorker{contentWriter: writer}
	create := []Event{cmEvent("CREATE", "fresh", "green")}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", create, nil)
	require.NoError(t, err, ".sops.yaml is the operator's own config and must not be refused")
	assert.True(t, changed, "the ConfigMap must still be written beside .sops.yaml")
}

func TestResyncRefusal_DoesNotStageSOPSBootstrap(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	repo, err := gogit.PlainOpen(root)
	require.NoError(t, err)

	seedPlacedManifest(t, worktree, "apps/kustomization.yaml", hardKustomizeYAML)

	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	pending := PendingWrite{
		Kind:               PendingWriteResync,
		GitTargetName:      "target-a",
		GitTargetNamespace: "default",
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: "target-a", Namespace: "default"}: {
				Name:      "target-a",
				Namespace: "default",
				Path:      "apps",
				BootstrapOptions: buildBootstrapOptions(&ResolvedEncryptionConfig{
					AgeRecipients: []string{"age1testrecipient"},
				}),
			},
		},
	}

	_, err = w.executeResyncPendingWrite(context.Background(), repo, worktree, pending)
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)

	_, statErr := os.Stat(filepath.Join(root, "apps", ".sops.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a refused resync must not stage bootstrap .sops.yaml")
}
