/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	require.NoError(t, setHeadToMain(repo))

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	return &BranchWorker{contentWriter: newContentWriter()}, repo, worktree, repoPath
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

	gitPath := generateFilePath(event.Identifier)
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

func TestExecutor_GroupedSingleEvent_UsesPerEventMessageFallback(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.Template = "event: {{.Name}} by {{.Username}}"
	config.Message.GroupTemplate = "group: {{.Author}} changed {{.Count}}"

	unit := CommitUnit{
		MessageKind:  CommitMessagePerEvent,
		Events:       []Event{makeEvent("alice", "api")},
		CommitConfig: config,
		GroupAuthor:  "alice",
		Target: ResolvedTargetMetadata{
			Name: "team-a",
		},
	}

	message, options, err := unit.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "event: api by alice", message)
	assert.Equal(t, "alice", options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_GroupedMultiEvent_UsesGroupTemplate(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.Template = "event: {{.Name}}"
	config.Message.GroupTemplate = "group: {{.Author}} {{.Count}} {{.GitTarget}}"

	unit := CommitUnit{
		MessageKind: CommitMessageGrouped,
		Events: []Event{
			makeEvent("alice", "api"),
			makeEvent("alice", "worker"),
		},
		CommitConfig: config,
		GroupAuthor:  "alice",
		Target: ResolvedTargetMetadata{
			Name: "team-a",
		},
	}

	message, options, err := unit.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "group: alice 2 team-a", message)
	assert.Equal(t, "alice", options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_AtomicUnit_UsesBatchMessage(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.BatchTemplate = "batch: {{.Count}} {{.GitTarget}}"

	unit := CommitUnit{
		MessageKind:   CommitMessageBatch,
		CommitMessage: "",
		Events: []Event{
			makeEvent("alice", "api"),
			makeEvent("bob", "worker"),
		},
		CommitConfig: config,
		Target: ResolvedTargetMetadata{
			Name: "team-a",
		},
	}

	message, options, err := unit.commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, "batch: 2 team-a", message)
	assert.Equal(t, DefaultCommitterName, options.Author.Name)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
}

func TestExecutor_NoOpUnit_SkipsCommit(t *testing.T) {
	worker, repo, worktree, repoPath := newExecutorTestRepo(t)
	event := configMapEvent("existing", "alice", "team-a")
	seedEventContent(t, repoPath, worktree, worker.contentWriter, event)

	headBefore, err := repo.Head()
	require.NoError(t, err)

	created, err := worker.executeCommitUnit(context.Background(), repo, worktree, CommitUnit{
		MessageKind:  CommitMessagePerEvent,
		Events:       []Event{event},
		CommitConfig: ResolveCommitConfig(nil),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, created)

	headAfter, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore.Hash(), headAfter.Hash())
}

func TestExecutor_AppliesEncryptionFromCommitUnit_NotFromWorker(t *testing.T) {
	installFakeSOPSBinary(t)

	worker, repo, worktree, repoPath := newExecutorTestRepo(t)
	cfg := &ResolvedEncryptionConfig{
		Provider:      EncryptionProviderSOPS,
		AgeRecipients: []string{"age1qexecutorunitrecipient"},
	}
	unit := CommitUnit{
		MessageKind: CommitMessagePerEvent,
		Events: []Event{func() Event {
			event := newExecutorSecretEvent("team-secrets")
			event.BootstrapOptions = buildBootstrapOptions(cfg)
			return event
		}()},
		CommitConfig: ResolveCommitConfig(nil),
		Target: ResolvedTargetMetadata{
			Name:             "deleted-target",
			Path:             "team-secrets",
			EncryptionConfig: cfg,
		},
	}

	created, err := worker.executeCommitUnit(context.Background(), repo, worktree, unit)
	require.NoError(t, err)
	assert.Equal(t, 1, created)

	encryptedPath := filepath.Join(repoPath, "team-secrets", "v1", "secrets", "default", "unit-secret.sops.yaml")
	assert.FileExists(t, encryptedPath)
	assert.NoFileExists(t, filepath.Join(repoPath, "team-secrets", "v1", "secrets", "default", "unit-secret.yaml"))

	content, err := os.ReadFile(encryptedPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "sops:")

	expectedScope := secretEncryptionCacheScope(filepath.Join(repoPath, "team-secrets"), cfg)
	assert.Equal(t, expectedScope, worker.contentWriter.encryptionScope)
}
