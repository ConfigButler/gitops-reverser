// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func newExecutorTestRepo(t *testing.T) (*BranchWorker, *git.Repository, *git.Worktree, string) {
	t.Helper()

	repoPath := t.TempDir()
	repo, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)
	require.NoError(t, PinExplicitSigningPolicy(repo))
	require.NoError(t, setHeadToMain(repo))

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	return &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{})}, repo, worktree, repoPath
}

func newExecutorSecretEvent(path string) Event {
	return Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "unit-secret",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"password": "ZG8tbm90LWNvbW1pdA==",
				},
			},
		},
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "unit-secret",
		},
		Operation: "CREATE",
		UserInfo:  UserInfo{Username: "alice"},
		Path:      path,
	}
}

func seedEventContent(t *testing.T, repoPath string, worktree *git.Worktree, writer *contentWriter, event Event) {
	t.Helper()

	content, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)

	gitPath := generateFilePath(event.Identifier, types.SensitiveResourcePolicy{})
	if event.Path != "" {
		gitPath = filepath.ToSlash(filepath.Join(event.Path, gitPath))
	}

	fullPath := filepath.Join(repoPath, gitPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, content, 0o600))
	_, err = worktree.Add(gitPath)
	require.NoError(t, err)
	_, err = worktree.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "seed@example.com", When: time.Now()},
	})
	require.NoError(t, err)
}

func TestGenerateFilePath_AdditionalSensitiveResourceUsesSOPSPath(t *testing.T) {
	policy, err := types.ParseSensitiveResourcePolicy("core.cozystack.io/tenantsecrets")
	require.NoError(t, err)

	path := generateFilePath(types.ResourceIdentifier{
		Group:     "core.cozystack.io",
		Version:   "v1beta1",
		Resource:  "tenantsecrets",
		Namespace: "tenant-a",
		Name:      "registry",
	}, policy)

	assert.Equal(t, "tenant-a/core.cozystack.io/tenantsecrets/registry.sops.yaml", path)
}

func TestExecutor_GroupedSingleEvent_UsesLiveTemplate(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.LiveTemplate = "group: {{.Author}} changed {{.Count}}"

	pendingWrite := PendingWrite{
		Kind:         PendingWriteCommit,
		Events:       []Event{makeEvent("alice", "api")},
		CommitConfig: config,
	}

	message, options, err := pendingWrite.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "group: alice changed 1", message)
	assert.Equal(t, "alice", options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_GroupedMultiEvent_UsesLiveTemplate(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.LiveTemplate = "group: {{.Author}} {{.Count}} {{.GitTarget}}"

	pendingWrite := PendingWrite{
		Kind: PendingWriteCommit,
		Events: []Event{
			makeEvent("alice", "api"),
			makeEvent("alice", "worker"),
		},
		CommitConfig: config,
	}

	message, options, err := pendingWrite.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "group: alice 2 team-a", message)
	assert.Equal(t, "alice", options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_AtomicUnit_UsesReconcileMessage(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.ReconcileTemplate = "reconcile: {{.Count}} {{.GitTarget}}"

	pendingWrite := PendingWrite{
		Kind:          PendingWriteAtomic,
		CommitMessage: "",
		Events: []Event{
			makeEvent("alice", "api"),
			makeEvent("bob", "worker"),
		},
		CommitConfig:       config,
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
	}

	message, options, err := pendingWrite.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "reconcile: 2 team-a", message)
	assert.Equal(t, DefaultCommitterName, options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_NoOpUnit_SkipsCommit(t *testing.T) {
	worker, repo, worktree, repoPath := newExecutorTestRepo(t)
	event := configMapEvent("existing", "alice", "team-a")
	seedEventContent(t, repoPath, worktree, worker.contentWriter, event)

	headBefore, err := repo.Head()
	require.NoError(t, err)

	created, hash, err := worker.executePendingWrite(context.Background(), repo, worktree, PendingWrite{
		Kind:         PendingWriteCommit,
		Events:       []Event{event},
		CommitConfig: ResolveCommitConfig(nil),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, created)
	assert.True(t, hash.IsZero(), "a no-change write reports a zero commit hash")

	headAfter, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore.Hash(), headAfter.Hash())
}

func TestExecutor_AppliesEncryptionFromPendingWrite_NotFromWorker(t *testing.T) {
	installFakeSOPSBinary(t)

	worker, repo, worktree, repoPath := newExecutorTestRepo(t)
	cfg := &ResolvedEncryptionConfig{
		Provider:      EncryptionProviderSOPS,
		AgeRecipients: []string{"age1qexecutorunitrecipient"},
	}
	event := func() Event {
		event := newExecutorSecretEvent("team-secrets")
		event.BootstrapOptions = buildBootstrapOptions(cfg)
		event.GitTargetName = "deleted-target"
		event.GitTargetNamespace = "default"
		return event
	}()
	pendingWrite := PendingWrite{
		Kind:         PendingWriteCommit,
		Events:       []Event{event},
		CommitConfig: ResolveCommitConfig(nil),
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: "deleted-target", Namespace: "default"}: {
				Name:             "deleted-target",
				Namespace:        "default",
				Path:             "team-secrets",
				EncryptionConfig: cfg,
			},
		},
	}

	created, hash, err := worker.executePendingWrite(context.Background(), repo, worktree, pendingWrite)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	assert.False(t, hash.IsZero(), "a committed write reports its commit hash")

	encryptedPath := filepath.Join(repoPath, "team-secrets", "default", "secrets", "unit-secret.sops.yaml")
	assert.FileExists(t, encryptedPath)
	assert.NoFileExists(t, filepath.Join(repoPath, "team-secrets", "default", "secrets", "unit-secret.yaml"))

	content, err := os.ReadFile(encryptedPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "sops:")

	expectedScope := secretEncryptionCacheScope(filepath.Join(repoPath, "team-secrets"), cfg)
	assert.Equal(t, expectedScope, worker.contentWriter.encryptionScope)
}

// A resync renders the target's reconcile template and hands the result over as the pending
// write's message. That generated text is not a CommitRequest literal override, so the request
// contract's length and control-character limits must not reject an operator's own template.
func TestCommitMetadata_ResyncRenderedMessageIsNotHeldToTheLiteralRequestContract(t *testing.T) {
	longMessage := "chore: reconcile " + strings.Repeat("x", 1200)

	for name, rendered := range map[string]string{
		"longer than a request message may be": longMessage,
		"carrying a tab":                       "chore: reconcile\n\n\tindented detail",
	} {
		t.Run(name, func(t *testing.T) {
			pendingWrite := PendingWrite{
				Kind:               PendingWriteResync,
				GitTargetName:      "team-a",
				GitTargetNamespace: "default",
				CommitConfig:       ResolveCommitConfig(nil),
				CommitMessage:      rendered,
			}

			message, options, err := pendingWrite.commitMetadata()
			require.NoError(t, err)
			assert.Equal(t, rendered, message)
			assert.NotNil(t, options)
		})
	}
}
