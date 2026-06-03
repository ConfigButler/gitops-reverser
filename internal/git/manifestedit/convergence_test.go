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

package manifestedit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Convergence is the property the vision leans on: repeated reconciles must
// settle to "no change" without keeping a separate reconciliation-state layer.
// The first patch may normalize known drift (folded scalars reflow, flush-left
// sequences re-indent) and clean server fields (status), but every patch after
// that must be a byte-stable no-op.
func TestPatch_ConvergesAfterFirstWrite(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  items:
  - one
  - two
  note: >-
    line one
    line two
status:
  replicas: 3
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
  labels:
    app: demo
spec:
  items:
  - one
  - two
  note: >-
    line one
    line two
`)

	first, _ := PatchDocument([]byte(git), 0, desired)
	require.Equal(t, EditPatched, first.Mode)
	assert.Contains(t, string(first.Content), "app: demo", "the change is applied")
	assert.NotContains(t, string(first.Content), "status:", "server-side status is cleaned from Git")

	second, _ := PatchDocument(first.Content, 0, desired)
	assert.Equal(t, EditNoChange, second.Mode, "the next reconcile must be a no-op")
	assert.Equal(t, string(first.Content), string(second.Content), "and byte-stable")

	third, _ := PatchDocument(second.Content, 0, desired)
	assert.Equal(t, EditNoChange, third.Mode, "and it stays converged")
	assert.Equal(t, string(second.Content), string(third.Content))
}

// A pure no-op never re-encodes, so even a folded scalar that would reflow on a
// real edit is preserved byte-for-byte when nothing actually changes.
func TestPatch_NoOpDoesNotReflowFoldedScalar(t *testing.T) {
	git := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  note: >-
    line one
    line two
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  note: >-
    line one
    line two
`)
	res, _ := PatchDocument([]byte(git), 0, desired)
	assert.Equal(t, EditNoChange, res.Mode)
	assert.Equal(t, git, string(res.Content), "a no-op preserves the folded layout exactly")
}
