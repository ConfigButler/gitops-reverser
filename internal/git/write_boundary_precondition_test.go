// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The write-boundary preconditions (Track 1) make two invariants explicit and tested rather
// than emergent, before any byte is written. See
// docs/design/support-boundary/gittarget-granularity-and-cross-environment-edits.md §1:
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

// diamondFiles is a minimal single-root diamond: root → a → base and root → b → base, where a
// and b carry differing images entries so base/deployment.yaml is reached by two distinct
// override chains (write-fan-in > 1). Ordered so seeding it into Git produces stable commits.
func diamondFiles() []struct{ rel, content string } {
	return []struct{ rel, content string }{
		// No root kustomization: a/ and b/ are two SEPARATE render roots that both
		// read ../base. A true diamond (one root reaching base through both) is not
		// buildable by kustomize at all — "may not add resource with an already
		// registered id" — so it is refused at the acceptance gate and never reaches
		// the write-boundary preconditions this test is about.
		{"a/kustomization.yaml", diamondOverlayKust("1.0.0")},
		{"b/kustomization.yaml", diamondOverlayKust("2.0.0")},
		{"base/kustomization.yaml", "resources:\n  - deployment.yaml\n"},
		{"base/deployment.yaml", diamondDeploymentYAML},
	}
}

// seedDiamond writes the diamond into a worktree on disk.
func seedDiamond(t *testing.T, root string) {
	t.Helper()
	for _, f := range diamondFiles() {
		full := filepath.Join(root, filepath.FromSlash(f.rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(f.content), 0o600))
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

// seedDiamondInRemote pushes the diamond to a remote branch as one commit. It clones once
// rather than reusing simulateClientCommitOnDisk per file, because the diamond spans
// subdirectories and that helper does not create parent directories.
func seedDiamondInRemote(t *testing.T, remoteURL, branch string) {
	t.Helper()
	clientPath := filepath.Join(t.TempDir(), "seed")
	repo, worktree := initLocalRepo(t, clientPath, remoteURL, branch)
	for _, f := range diamondFiles() {
		full := filepath.Join(clientPath, filepath.FromSlash(f.rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(f.content), 0o600))
		_, err := worktree.Add(f.rel)
		require.NoError(t, err)
	}
	_, err := worktree.Commit("seed diamond", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Client", Email: "client@example.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)},
	}))
}

// diamondGitTarget is the GitTarget the live-path test's events name, rooted at the repo root
// so the whole diamond is inside its write scope.
func diamondGitTarget(providerName, branch string) *configv1alpha3.GitTarget {
	return &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo-test", Namespace: "default"},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef: configv1alpha3.GitProviderReference{Name: providerName},
			Branch:      branch,
			Path:        "",
		},
	}
}

// TestEventLoop_LiveFanInRefusal_FailsGitTargetAndCommitsNothing closes the live-path gap: a
// live event window whose flush trips a write-boundary precondition is finalized off a timer,
// with no result channel to carry the refusal back to the router. It must still reach the user
// as a GitTarget refusal — reported through the worker's PathRefusalReporter, which the
// watch Manager maps to GitPathAccepted=False / Stalled=True — and it must leave the branch
// exactly where it was: an ambiguous write is prevented, never half-applied.
func TestEventLoop_LiveFanInRefusal_FailsGitTargetAndCommitsNothing(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	createBareRepo(t, remotePath)
	remoteURL := "file://" + remotePath
	seedDiamondInRemote(t, remoteURL, "main")
	seededHash := branchHash(t, remotePath, "main")

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main", diamondGitTarget("test-repo", "main"))
	require.NoError(t, err)
	worker.mapper = deploymentMapper()

	var refusals []*manifestanalyzer.AcceptanceRefusedError
	var refusedTargets []types.ResourceReference
	worker.pathRefusal = func(target types.ResourceReference, refused *manifestanalyzer.AcceptanceRefusedError) {
		refusedTargets = append(refusedTargets, target)
		refusals = append(refusals, refused)
	}

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now() // keep the (irrelevant) push out of this test
	defer loop.stopTimers()

	event := overridesDeploymentEvent("ghcr.io/example/podinfo:9.9.9", 3)
	event.UserInfo = UserInfo{Username: "alice"}
	event.GitTargetName = "podinfo-test"
	event.GitTargetNamespace = "default"
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	require.NotNil(t, loop.openWindow, "the live event must open a commit window")

	assert.False(t, loop.finalizeOpenWindow(), "a refused write plan must not produce a pending write")
	assert.Empty(t, loop.pendingWrites, "a refused flush must retain nothing to push")
	assert.Nil(t, loop.openWindow, "the refused window must be dropped, not retried forever")

	require.Len(t, refusals, 1, "a live write-boundary refusal must be reported exactly once")
	assert.Equal(t, types.NewResourceReference("podinfo-test", "default"), refusedTargets[0],
		"the refusal must name the GitTarget whose window was refused")
	assert.Contains(t, refusalIssueKinds(t, refusals[0]), manifestanalyzer.IssueWriteFanIn)

	// The GitTarget fails, and Git is untouched: no commit was created on the worker's local
	// branch, so nothing could ever be pushed.
	localHead, err := gogit.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	head, err := localHead.Head()
	require.NoError(t, err)
	assert.Equal(t, seededHash, head.Hash(), "a refused live write must create no commit")
}

// branchHash reads a branch tip straight out of a repository on disk.
func branchHash(t *testing.T, repoPath, branch string) plumbing.Hash {
	t.Helper()
	repo, err := gogit.PlainOpen(repoPath)
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(t, err)
	return ref.Hash()
}
