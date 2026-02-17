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

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestWriteEvents_SecretEncryptionFailureDoesNotWritePlaintext(t *testing.T) {
	originalWriter := defaultContentWriter
	defaultContentWriter = newContentWriter()
	t.Cleanup(func() { defaultContentWriter = originalWriter })

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
	assert.Error(t, statErr, "Secret file should not be written when encryption fails")
}
