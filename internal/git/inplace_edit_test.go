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

// configMapEvent builds an UPDATE event for a ConfigMap with one data key.
func inplaceCMEvent(name, namespace, color string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
			"data":       map[string]interface{}{"color": color},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: name,
		},
		Operation: "UPDATE",
	}
}

// newWorktreeForTest gives a real git worktree rooted at a fresh temp dir.
func newWorktreeForTest(t *testing.T) *gogit.Worktree {
	t.Helper()
	repo, err := gogit.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	return worktree
}

// When the file on disk is hand-authored (carries a comment), an update edits it
// in place: the comment survives and only the changed field is rewritten. This is
// the file-agnostic-placement "magic" landing in the live writer.
func TestHandleCreateOrUpdate_PreservesHandAuthoredFormatting(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("app", "default", "green")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	seeded := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: app\n  namespace: default\n" +
		"data:\n  # keep this operator note across edits\n  color: blue\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(seeded), 0o600))

	changed, err := handleCreateOrUpdateOperation(context.Background(), writer, event, relPath, full, worktree)
	require.NoError(t, err)
	require.True(t, changed, "a real value change must be written")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "# keep this operator note across edits",
		"the hand-authored comment must survive the in-place edit")
	assert.Contains(t, string(got), "color: green", "the changed value is applied")
	assert.NotContains(t, string(got), "color: blue")
}

// A file already in the operator's canonical format is rewritten wholesale, byte
// identical to what buildContentForWrite produces — operator-authored content is
// never reformatted by the in-place path.
func TestHandleCreateOrUpdate_CanonicalFileStaysWholesale(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("app", "default", "green")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	// Seed with the canonical rendering of a *different* value, so the update is a
	// real change against an operator-canonical file.
	seedCanonical, err := writer.buildContentForWrite(context.Background(), inplaceCMEvent("app", "default", "blue"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, seedCanonical, 0o600))

	changed, err := handleCreateOrUpdateOperation(context.Background(), writer, event, relPath, full, worktree)
	require.NoError(t, err)
	require.True(t, changed)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	wantCanonical, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, string(wantCanonical), string(got),
		"a canonical file is rewritten wholesale, not reformatted by the in-place path")
}
