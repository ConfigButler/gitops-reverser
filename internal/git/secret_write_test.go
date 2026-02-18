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

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestWriteEvents_SecretEncryptionFailureDoesNotWritePlaintext(t *testing.T) {
	repoPath := t.TempDir()
	_, err := gogit.PlainInit(repoPath, false)
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

	_, err = WriteEvents(context.Background(), repoPath, []Event{event}, "master", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret encryption is required")

	secretPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.yaml")
	_, statErr := os.Stat(secretPath)
	require.Error(t, statErr, "Secret file should not be written when encryption fails")

	sopsPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.sops.yaml")
	_, statErr = os.Stat(sopsPath)
	assert.Error(t, statErr, "Secret file should not be written when encryption fails")
}

func TestWriteEvents_SecretWritesSOPSPath(t *testing.T) {
	repoPath := t.TempDir()
	_, err := gogit.PlainInit(repoPath, false)
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

	writer := newContentWriter()
	writer.setEncryptor(&stubEncryptor{result: []byte("encrypted: true\nsops:\n  version: 3.9.0\n")})

	_, err = WriteEventsWithContentWriter(context.Background(), writer, repoPath, []Event{event}, "master", nil)
	require.NoError(t, err)

	sopsPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.sops.yaml")
	assert.FileExists(t, sopsPath)
}

func TestWriteEvents_DeleteSecretRemovesSOPSPath(t *testing.T) {
	repoPath := t.TempDir()
	repo, err := gogit.PlainInit(repoPath, false)
	require.NoError(t, err)

	sopsPath := filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.sops.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(sopsPath), 0750))
	require.NoError(t, os.WriteFile(sopsPath, []byte("encrypted"), 0600))

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add("v1/secrets/default/test-secret.sops.yaml")
	require.NoError(t, err)
	_, err = worktree.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "seed@example.com", When: time.Now()},
	})
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

	_, err = WriteEvents(context.Background(), repoPath, []Event{event}, "master", nil)
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(repoPath, "v1", "secrets", "default", "test-secret.yaml"))
	require.Error(t, statErr)
	_, statErr = os.Stat(sopsPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWriteEvents_DoesNotBootstrapRootSOPSConfig(t *testing.T) {
	repoPath := t.TempDir()
	_, err := gogit.PlainInit(repoPath, false)
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

	_, err = WriteEvents(context.Background(), repoPath, []Event{event}, "master", nil)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(repoPath, sopsConfigFileName))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWriteEvents_DoesNotCreateBootstrapOnlyCommit(t *testing.T) {
	repoPath := t.TempDir()
	repo, err := gogit.PlainInit(repoPath, false)
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

	result, err := WriteEvents(context.Background(), repoPath, []Event{event}, "master", nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 0, result.CommitsCreated)

	_, err = repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}
