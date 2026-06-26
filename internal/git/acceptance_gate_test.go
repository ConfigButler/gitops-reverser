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
	"os"
	"path/filepath"
	"testing"

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
	_, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{event})

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.Error(t, err, "an unsupported folder must be refused")
	require.True(t, errors.As(err, &refused), "flush must refuse with *AcceptanceRefusedError, got %v", err)
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
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{cmEvent("CREATE", "fresh", "green")})
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
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{cmEvent("CREATE", "fresh", "green")})
	require.NoError(t, err, ".sops.yaml is the operator's own config and must not be refused")
	assert.True(t, changed, "the ConfigMap must still be written beside .sops.yaml")
}
