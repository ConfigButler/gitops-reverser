// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestRecordRemoteObservation_DeliversOneFactToTheWholeBranch. A worker serves every GitTarget on
// its (provider, branch), and "where is branch B" is one fact for all of them. It used to be
// delivered against whichever targets the caller happened to hold — the ones a push's writes
// named, or the one a refresh was serving — so some targets on a branch had it and some did not,
// and the layer above kept a copy per target of a value that was identical for all of them.
func TestRecordRemoteObservation_DeliversOneFactToTheWholeBranch(t *testing.T) {
	m := NewWorkerManager(nil, logr.Discard(), BranchWorkerLimits{}, itypes.SensitiveResourcePolicy{})
	key := BranchKey{RepoNamespace: "shop", RepoName: "repo1", Branch: "main"}
	repo := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	w := &BranchWorker{Log: logr.Discard(), repo: repo}
	w.remoteReporter = func(observed RemoteObservation) { m.recordRemoteObservation(key, observed) }

	_, known := m.RemoteForBranch(key)
	require.False(t, known, "nothing has looked yet, which is not the same as a branch with no commits")

	w.recordRemoteObservation("aaaa", ObservedByPush)

	observed, known := m.RemoteForBranch(key)
	require.True(t, known)
	assert.Equal(t, "aaaa", observed.Revision)
	assert.Equal(t, ObservedByPush, observed.By)
	assert.Equal(t, repo, observed.Repo, "a revision means nothing without the repository it is in")
	assert.WithinDuration(t, time.Now(), observed.At, time.Minute)
}

// TestRecordRemoteObservation_IsANoOpWithoutAReporter covers the CLI, which runs the same write
// path with nothing keeping observations behind it.
func TestRecordRemoteObservation_IsANoOpWithoutAReporter(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	assert.NotPanics(t, func() { w.recordRemoteObservation("aaaa", ObservedByPush) })
}

// TestRemoteForBranch_SurvivesTheWorkerItWasProvedBy is why the observation is kept by the manager
// and not by the worker. A worker replaced because its GitProvider names another repository takes
// its own memory with it; the layer that publishes needs the OLD observation, with the repository
// stamped on it, to see that what it has published describes a repository this GitTarget has left.
func TestRemoteForBranch_SurvivesTheWorkerItWasProvedBy(t *testing.T) {
	m, ctx := startedManager(t)
	key := BranchKey{RepoNamespace: "gitops-system", RepoName: "repo1", Branch: "main"}
	before := RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	after := RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}

	require.NoError(t, m.EnsureWorker(ctx, "repo1", "gitops-system", "main", before))
	worker, ok := m.GetWorkerForTarget("repo1", "gitops-system", "main")
	require.True(t, ok)
	worker.recordRemoteObservation("aaaa", ObservedByPush)

	require.NoError(t, m.EnsureWorker(ctx, "repo1", "gitops-system", "main", after))

	observed, known := m.RemoteForBranch(key)
	require.True(t, known,
		"the replacement has proved nothing yet, and the old answer is still what is published")
	assert.Equal(t, before, observed.Repo,
		"stamped with the repository it was proved against, which is how the publisher takes it back")
}

// TestUpdateBranchMetadataFromPullReport_RecordsAnAbsentBranchAsNoRevision. A fetch that found no
// such branch on the remote still observed something, and "no revision" is that answer: a branch
// does not exist without a commit. Recording nothing would leave the last known revision of some
// earlier branch standing as this one's.
func TestUpdateBranchMetadataFromPullReport_RecordsAnAbsentBranchAsNoRevision(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	w.recordRemoteObservation("aaaa", ObservedByPush)

	w.updateBranchMetadataFromPullReport(&PullReport{ExistsOnRemote: false})

	observed, known := w.LastRemoteObservation()
	require.True(t, known)
	assert.Empty(t, observed.Revision, "the branch is not there, and that is the observation")
	assert.Equal(t, ObservedByFetch, observed.By)
	assert.True(t, w.baseTrusted(), "the reset still happened: the worktree matches what the remote has")
}

// TestUpdateBranchMetadataFromPullReport_RecordsTheFetchedHead is the ordinary half.
func TestUpdateBranchMetadataFromPullReport_RecordsTheFetchedHead(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}

	w.updateBranchMetadataFromPullReport(&PullReport{
		ExistsOnRemote: true, HEAD: BranchInfo{Sha: "bbbb"},
	})

	observed, known := w.LastRemoteObservation()
	require.True(t, known)
	assert.Equal(t, "bbbb", observed.Revision)
	assert.Equal(t, ObservedByFetch, observed.By)
}

// TestSetScanAcceptanceReporter_ReachesEveryWorkerTheManagerCreates. The hook is installed once at
// startup, before any worker exists, so a worker created later has to carry it: without one the
// refresher reads the folder, finds content nobody can write, and tells nobody.
func TestSetScanAcceptanceReporter_ReachesEveryWorkerTheManagerCreates(t *testing.T) {
	m := NewWorkerManager(nil, logr.Discard(), BranchWorkerLimits{}, itypes.SensitiveResourcePolicy{})
	called := false
	m.SetScanAcceptanceReporter(
		func(itypes.ResourceReference, *manifestanalyzer.AcceptanceRefusedError) { called = true },
	)

	require.NotNil(t, m.scanAcceptance)
	m.scanAcceptance(itypes.NewResourceReference("checkout", "shop"), nil)
	assert.True(t, called)
}

// TestReportScanAcceptance_SkipsAnUnattributableTarget. The projection is keyed by
// "namespace/name", so a reference with either half empty — the CLI, and tests — would file the
// verdict under a key no GitTarget reads.
func TestReportScanAcceptance_SkipsAnUnattributableTarget(t *testing.T) {
	reported := 0
	w := &BranchWorker{Log: logr.Discard()}
	w.scanAcceptance = func(itypes.ResourceReference, *manifestanalyzer.AcceptanceRefusedError) {
		reported++
	}

	w.reportScanAcceptance(itypes.NewResourceReference("", "shop"), nil)
	w.reportScanAcceptance(itypes.NewResourceReference("checkout", ""), nil)
	assert.Zero(t, reported)

	w.reportScanAcceptance(itypes.NewResourceReference("checkout", "shop"), nil)
	assert.Equal(t, 1, reported)
}
