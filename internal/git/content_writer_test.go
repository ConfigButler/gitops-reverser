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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

type stubEncryptor struct {
	callCount int
	err       error
	result    []byte
}

func (s *stubEncryptor) Encrypt(_ context.Context, _ []byte, _ ResourceMeta) ([]byte, error) {
	s.callCount++
	if s.err != nil {
		return nil, s.err
	}
	return append([]byte(nil), s.result...), nil
}

func TestBuildContentForWrite_MarshalOrderedYAML(t *testing.T) {
	writer := newContentWriter()

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "my-config",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"config.yaml": "enabled: true",
				},
			},
		},
	}

	got, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)

	output := string(got)
	assert.Contains(t, output, "apiVersion: v1")
	assert.Contains(t, output, "kind: ConfigMap")
	assert.Contains(t, output, "metadata:")
	assert.Contains(t, output, "name: my-config")
	assert.Contains(t, output, "namespace: default")
	assert.Contains(t, output, "data:")
	assert.Contains(t, output, "config.yaml: 'enabled: true'")
}

func TestBuildContentForWrite_ReturnsMarshalError(t *testing.T) {
	writer := newContentWriter()

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "bad-config",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"invalid": make(chan int),
				},
			},
		},
	}

	_, err := writer.buildContentForWrite(context.Background(), event)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal object to YAML")
}

func TestBuildContentForWrite_SecretRequiresEncryptor(t *testing.T) {
	writer := newContentWriter()

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "my-secret",
		},
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "my-secret",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"password": "cGxhaW4=",
				},
			},
		},
	}

	_, err := writer.buildContentForWrite(context.Background(), event)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret encryption is required but no encryptor is configured")
}

func TestBuildContentForWrite_SecretEncryptionCacheMarkerReuse(t *testing.T) {
	writer := newContentWriter()

	enc := &stubEncryptor{result: []byte("encrypted: true\nsops:\n  version: 3.9.0\n")}
	writer.setEncryptor(enc)

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":            "my-secret",
				"namespace":       "default",
				"uid":             "uid-1",
				"resourceVersion": "10",
				"generation":      int64(1),
			},
			"data": map[string]interface{}{
				"password": "cGxhaW4=",
			},
		},
	}

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "my-secret",
		},
		Object: obj,
	}

	first, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	second, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, 1, enc.callCount)
	assert.Equal(t, first, second)
}

func TestBuildContentForWrite_SecretUIDChangeForcesReencrypt(t *testing.T) {
	writer := newContentWriter()

	enc := &stubEncryptor{result: []byte("encrypted: true\nsops:\n  version: 3.9.0\n")}
	writer.setEncryptor(enc)

	makeSecret := func(uid string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":            "my-secret",
					"namespace":       "default",
					"uid":             uid,
					"resourceVersion": "10",
					"generation":      int64(1),
				},
				"data": map[string]interface{}{
					"password": "cGxhaW4=",
				},
			},
		}
	}

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "my-secret",
		},
		Object: makeSecret("uid-1"),
	}
	_, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)

	event.Object = makeSecret("uid-2")
	_, err = writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, 2, enc.callCount)
}

func TestBuildContentForWrite_SecretEncryptionFailure(t *testing.T) {
	writer := newContentWriter()

	writer.setEncryptor(&stubEncryptor{err: errors.New("boom")})

	event := Event{
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "my-secret",
		},
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "my-secret",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"password": "cGxhaW4=",
				},
			},
		},
	}

	_, err := writer.buildContentForWrite(context.Background(), event)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret encryption failed")
}
