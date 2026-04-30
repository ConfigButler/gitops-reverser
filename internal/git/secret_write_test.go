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
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func installFakeSOPSBinary(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
cat <<'EOF'
apiVersion: v1
kind: Secret
sops:
  version: 3.9.0
encrypted_regex: "^(data|stringData)$"
EOF
cat >/dev/null
`), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func secretTargetObjects(t *testing.T, providerName, branch, path string) []client.Object {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	return []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sops-age-key",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"identity.agekey": []byte(identity.String()),
			},
		},
		&configv1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "secret-target",
				Namespace: "default",
			},
			Spec: configv1alpha1.GitTargetSpec{
				ProviderRef: configv1alpha1.GitProviderReference{
					Kind: "GitProvider",
					Name: providerName,
				},
				Branch: branch,
				Path:   path,
				Encryption: &configv1alpha1.EncryptionSpec{
					Provider: "sops",
					Age: &configv1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: configv1alpha1.AgeRecipientsSpec{
							ExtractFromSecret: true,
						},
					},
					SecretRef: configv1alpha1.LocalSecretReference{
						Name: "sops-age-key",
					},
				},
			},
		},
	}
}

func TestBranchWorker_SecretEncryptionFailureDoesNotWritePlaintext(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "master")
	require.NoError(t, err)

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "test-secret",
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
			Name:      "test-secret",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "tester@example.com",
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	err = worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret encryption is required")

	repoPath := worker.repoPathForRemote(remoteURL)
	secretPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.yaml")
	_, statErr := os.Stat(secretPath)
	require.Error(t, statErr, "Secret file should not be written when encryption fails")

	sopsPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.sops.yaml")
	_, statErr = os.Stat(sopsPath)
	assert.Error(t, statErr, "Secret file should not be written when encryption fails")
}

func TestBranchWorker_SecretWritesSOPSPath(t *testing.T) {
	installFakeSOPSBinary(t)

	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	objects := secretTargetObjects(t, "test-repo", "master", "")
	worker, err := newTestBranchWorker(remoteURL, "test-repo", "master", objects...)
	require.NoError(t, err)

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "test-secret",
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
			Name:      "test-secret",
		},
		Operation:          "CREATE",
		UserInfo:           UserInfo{Username: "tester@example.com"},
		GitTargetName:      "secret-target",
		GitTargetNamespace: "default",
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	sopsPath := filepath.Join(worker.repoPathForRemote(remoteURL), "v1", "secrets", "default", "test-secret.sops.yaml")
	assert.FileExists(t, sopsPath)
}

func TestBranchWorker_DeleteSecretRemovesSOPSPath(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	repo, worktree := initLocalRepo(t, seedPath, remoteURL, "master")
	sopsPath := filepath.Join(seedPath, "v1", "secrets", "default", "test-secret.sops.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(sopsPath), 0o750))
	require.NoError(t, os.WriteFile(sopsPath, []byte("encrypted"), 0o600))
	_, err := worktree.Add("v1/secrets/default/test-secret.sops.yaml")
	require.NoError(t, err)
	_, err = worktree.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "seed@example.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/master:refs/heads/master")},
	}))

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "master")
	require.NoError(t, err)

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "test-secret",
		},
		Operation: "DELETE",
		UserInfo: UserInfo{
			Username: "tester@example.com",
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	localRepoPath := worker.repoPathForRemote(remoteURL)
	_, statErr := os.Stat(filepath.Join(localRepoPath, "v1", "secrets", "default", "test-secret.yaml"))
	require.Error(t, statErr)
	_, statErr = os.Stat(filepath.Join(localRepoPath, "v1", "secrets", "default", "test-secret.sops.yaml"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestBranchWorker_DoesNotBootstrapRootSOPSConfig(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "master")
	require.NoError(t, err)

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "sample-config",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key": "value",
				},
			},
		},
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "default",
			Name:      "sample-config",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "tester@example.com",
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	_, statErr := os.Stat(filepath.Join(worker.repoPathForRemote(remoteURL), sopsConfigFileName))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestBranchWorker_DoesNotCreateBootstrapOnlyCommit(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "master")
	require.NoError(t, err)

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "default",
			Name:      "does-not-exist",
		},
		Operation: "DELETE",
		UserInfo: UserInfo{
			Username: "tester@example.com",
		},
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	repo, err := gogit.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	_, err = repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}
