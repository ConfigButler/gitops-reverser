// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The write-boundary preconditions (Track 1) make two invariants explicit and tested rather
// than emergent, before any byte is written. See
// docs/design/gitops-api/gittarget-granularity-and-cross-environment-edits.md §1:
//   - L1: every planned write stays inside the GitTarget write scope (spec.path).
//   - L2: never write a live change through into a source file more than one render root
//     reaches with override entries at stake (write-fan-in must be 1).

// TestWritePathEscapesScope pins the L1 containment predicate: only an absolute, empty, or
// "..".-escaping base-relative path leaves the write scope; nested paths and a "../" that
// cleans back inside stay in.
func TestWritePathEscapesScope(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"default/pods/web.yaml", false},
		{"a/b/c.yaml", false},
		{"a/../b.yaml", false}, // cleans to b.yaml, still inside
		{"", true},
		{"/etc/passwd", true},
		{"..", true},
		{"../escape.yaml", true},
		{"../../base/deployment.yaml", true},
		{"a/../../escape.yaml", true},
	}
	for _, c := range cases {
		if got := writePathEscapesScope(c.rel); got != c.want {
			t.Errorf("writePathEscapesScope(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

// TestPathScopePrecondition_RefusesEscapingWrite proves the L1 write-plan precondition: a
// planned write inside the scope is fine, and one whose path escapes the subtree refuses the
// whole flush with IssueWriteEscapesScope, naming the path — before any byte is written.
func TestPathScopePrecondition_RefusesEscapingWrite(t *testing.T) {
	wb := &writeBatch{buffers: map[string]*fileBuffer{}}
	wb.buffers["default/cm.yaml"] = &fileBuffer{rel: "default/cm.yaml", current: []byte("a")}
	require.NoError(t, wb.pathScopePrecondition(), "a write inside the scope must be allowed")

	wb.buffers["../escape.yaml"] = &fileBuffer{rel: "../escape.yaml", current: []byte("b")}
	kinds := refusalIssueKinds(t, wb.pathScopePrecondition())
	assert.Contains(t, kinds, manifestanalyzer.IssueWriteEscapesScope,
		"a write escaping spec.path must refuse the flush")
}

// diamondDeploymentYAML is the shared base document a single render root reaches two ways
// (through overlays a and b) with differing images entries — the ambiguity the writer must
// never resolve by writing through into the shared file.
const diamondDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/example/podinfo:6.3.0
`

// diamondOverlayKust builds an overlay kustomization that references the shared base and
// pins the podinfo image to newTag — a and b differ, so the two chains reaching base conflict.
func diamondOverlayKust(newTag string) string {
	return "resources:\n  - ../base\nimages:\n  - name: ghcr.io/example/podinfo\n    newTag: \"" + newTag + "\"\n"
}

// seedDiamond writes a minimal single-root diamond: root → a → base and root → b → base,
// where a and b carry differing images entries so base/deployment.yaml is reached by two
// distinct override chains (write-fan-in > 1).
func seedDiamond(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"kustomization.yaml":      "resources:\n  - a\n  - b\n",
		"a/kustomization.yaml":    diamondOverlayKust("1.0.0"),
		"b/kustomization.yaml":    diamondOverlayKust("2.0.0"),
		"base/kustomization.yaml": "resources:\n  - deployment.yaml\n",
		"base/deployment.yaml":    diamondDeploymentYAML,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// TestFanInPrecondition_RefusesAmbiguousOverrideWriteThrough proves the L2 write-boundary
// invariant end to end: a live image bump on the diamond's shared Deployment would fall back
// to a write-through into base/deployment.yaml — a file two render paths reach with override
// entries at stake. The flush must refuse (IssueWriteFanIn) before writing, and the shared
// source file must keep its bytes, rather than the former warn-and-write-through behaviour.
func TestFanInPrecondition_RefusesAmbiguousOverrideWriteThrough(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedDiamond(t, root)

	w := &BranchWorker{contentWriter: writer, mapper: deploymentMapper()}
	_, err := w.flushEventsToWorktree(context.Background(), worktree, "",
		[]Event{overridesDeploymentEvent("ghcr.io/example/podinfo:9.9.9", 3)}, nil)
	assert.Contains(t, refusalIssueKinds(t, err), manifestanalyzer.IssueWriteFanIn,
		"an ambiguous-override write-through must be refused, not written through")

	assertFileBytes(t, filepath.Join(root, "base", "deployment.yaml"), diamondDeploymentYAML,
		"a refused fan-in write must leave the shared source file untouched")
}
