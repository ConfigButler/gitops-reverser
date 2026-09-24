// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestReportRemoteObservation_SkipsAnUnattributableTarget. The projection is keyed by
// "namespace/name", so a reference with either half empty — the CLI, and tests — would file the
// report under a key no GitTarget ever reads.
func TestReportRemoteObservation_SkipsAnUnattributableTarget(t *testing.T) {
	var reported []itypes.ResourceReference
	w := &BranchWorker{Log: logr.Discard()}
	w.remoteReporter = func(target itypes.ResourceReference, _ RemoteObservation) {
		reported = append(reported, target)
	}

	w.reportRemoteObservation([]itypes.ResourceReference{
		itypes.NewResourceReference("", "shop"),
		itypes.NewResourceReference("checkout", ""),
		itypes.NewResourceReference("checkout", "shop"),
	}, RemoteObservation{Revision: "aaaa", At: time.Now(), By: ObservedByPush})

	require.Len(t, reported, 1, "only a fully named target can be filed against")
	assert.Equal(t, "shop/checkout", reported[0].String())
}

// TestReportRemoteObservation_IsANoOpWithoutAReporter covers the CLI, which runs the same write
// path with no status surface behind it.
func TestReportRemoteObservation_IsANoOpWithoutAReporter(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	assert.NotPanics(t, func() {
		w.reportRemoteObservation(
			[]itypes.ResourceReference{itypes.NewResourceReference("checkout", "shop")},
			RemoteObservation{})
	})
}

// TestSetRemoteReporter_ReachesEveryWorkerTheManagerCreates. The hook is installed once at
// startup, before any worker exists, and a worker created later has to carry it: a worker built
// without one silently drops status.remote for every target on its branch.
func TestSetRemoteReporter_ReachesEveryWorkerTheManagerCreates(t *testing.T) {
	m := NewWorkerManager(nil, logr.Discard(), BranchWorkerLimits{}, itypes.SensitiveResourcePolicy{})
	called := false
	m.SetRemoteReporter(func(itypes.ResourceReference, RemoteObservation) { called = true })

	require.NotNil(t, m.remoteReporter)
	m.remoteReporter(itypes.NewResourceReference("checkout", "shop"), RemoteObservation{})
	assert.True(t, called)
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
