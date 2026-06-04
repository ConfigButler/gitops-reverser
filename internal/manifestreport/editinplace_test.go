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

package manifestreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A nil desired object is not editable: EditInPlace returns not-ok rather than
// dereferencing it.
func TestEditInPlace_NilObjectReturnsNotOk(t *testing.T) {
	existing := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: default\n")
	got, ok := EditInPlace("v1/configmaps/default/app.yaml", existing, nil)
	assert.False(t, ok)
	assert.Nil(t, got)
}

// EditInPlace edits an existing hand-authored document so it matches the desired
// object, preserving the comment and layout of everything it does not change.
func TestEditInPlace_PreservesCommentsOnChange(t *testing.T) {
	existing := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  # operator note: keep this across edits
  color: blue
`)
	desired := configMap("app", "green")

	got, ok := EditInPlace("v1/configmaps/default/app.yaml", existing, desired)
	require.True(t, ok)
	assert.Contains(t, string(got), "# operator note: keep this across edits",
		"the hand-authored comment must survive the edit")
	assert.Contains(t, string(got), "color: green", "the changed field is updated")
	assert.NotContains(t, string(got), "color: blue")
}

// A no-op edit returns the file byte-for-byte, comment intact.
func TestEditInPlace_NoOpPreservesBytes(t *testing.T) {
	existing := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  # keep me
  color: blue
`)
	got, ok := EditInPlace("v1/configmaps/default/app.yaml", existing, configMap("app", "blue"))
	require.True(t, ok)
	assert.Equal(t, string(existing), string(got), "a no-op edit preserves the file exactly")
}

// When the file has no document for the desired identity, EditInPlace declines so
// the caller falls back to writing canonical content.
func TestEditInPlace_WrongIdentityDeclines(t *testing.T) {
	existing := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n  namespace: default\n" +
		"data:\n  color: blue\n")
	_, ok := EditInPlace("v1/configmaps/default/app.yaml", existing, configMap("app", "green"))
	assert.False(t, ok, "no document for the identity: decline and fall back")
}

// An encrypted (SOPS) document is never edited in place: EditInPlace declines so
// the secret goes through the re-encrypt writer instead.
func TestEditInPlace_EncryptedDeclines(t *testing.T) {
	existing := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: default\n" +
		"data:\n  password: ENC[AES256_GCM,data:abc]\nsops:\n  age: []\n")
	desired := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "db", "namespace": "default"},
		"data":     map[string]interface{}{"password": "hunter2"},
	}}
	got, ok := EditInPlace("v1/secrets/default/db.sops.yaml", existing, desired)
	assert.False(t, ok, "encrypted documents must not be edited in place")
	assert.Nil(t, got)
}
