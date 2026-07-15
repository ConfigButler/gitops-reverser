// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// refusalIssueKinds extracts the issue kinds from an error if it is (or wraps) an
// AcceptanceRefusedError, so a test can assert exactly which refusal fired.
func refusalIssueKinds(t *testing.T, err error) []manifestanalyzer.IssueKind {
	t.Helper()
	require.Error(t, err)
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "error should be an AcceptanceRefusedError: %v", err)
	kinds := make([]manifestanalyzer.IssueKind, 0, len(refused.Issues))
	for _, iss := range refused.Issues {
		kinds = append(kinds, iss.Kind)
	}
	return kinds
}

// TestWriter_IgnoreShadowsManagedPath proves the write-plan precondition (§4.3): a
// .gittargetignore pattern that matches a path the operator is about to write aborts the
// whole flush and fails the GitTarget, before any byte is written.
func TestWriter_IgnoreShadowsManagedPath(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := tempDir + "/remote.git"
	createBareRepo(t, remotePath)
	remoteURL := "file://" + remotePath

	// "default/pods/" shadows the canonical write path default/pods/<name>.yaml. It is not
	// a catastrophic whole-space pattern, so the acceptance gate accepts it — the danger is
	// caught only at write time, where the path is finally known.
	simulateClientCommitOnDisk(t, remoteURL, "main", ".gittargetignore", "default/pods/\n")

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main")
	require.NoError(t, err)
	event := createTestEvent(t, "shadowed-pod")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)

	err = worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false)
	assert.Contains(t, refusalIssueKinds(t, err), manifestanalyzer.IssueIgnoreShadowsManaged)
}

// TestWriter_ForeignFileRefused proves the live writer refuses a folder that holds foreign
// non-YAML content under the GitTarget root.
func TestWriter_ForeignFileRefused(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := tempDir + "/remote.git"
	createBareRepo(t, remotePath)
	remoteURL := "file://" + remotePath

	simulateClientCommitOnDisk(t, remoteURL, "main", "db-password.txt", "hunter2")

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main")
	require.NoError(t, err)
	event := createTestEvent(t, "pod-a")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)

	err = worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false)
	assert.Contains(t, refusalIssueKinds(t, err), manifestanalyzer.IssueForeignFile)
}

// TestWriter_IgnoredForeignFileAllowsWrite proves the in-repo escape hatch end to end: a
// foreign file named in the root .gittargetignore is never read, so the operator writes
// its manifest cleanly.
func TestWriter_IgnoredForeignFileAllowsWrite(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := tempDir + "/remote.git"
	createBareRepo(t, remotePath)
	remoteURL := "file://" + remotePath

	simulateClientCommitOnDisk(t, remoteURL, "main", "notes.txt", "keep me")
	simulateClientCommitOnDisk(t, remoteURL, "main", ".gittargetignore", "*.txt\n")

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main")
	require.NoError(t, err)
	event := createTestEvent(t, "pod-b")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)

	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
}

// TestWriter_BenignPassengerAllowsWrite proves the live writer accepts a folder whose only
// non-managed content is an inert hygiene passenger (a LICENSE) — without any
// .gittargetignore — so adopting an existing repo does not stall on repo-hygiene files.
func TestCommitPendingWrites_BenignPassengerAllowsWrite(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := tempDir + "/remote.git"
	createBareRepo(t, remotePath)
	remoteURL := "file://" + remotePath

	simulateClientCommitOnDisk(t, remoteURL, "main", "LICENSE", "Apache-2.0")

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main")
	require.NoError(t, err)
	event := createTestEvent(t, "pod-c")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)

	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
}
