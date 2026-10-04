// SPDX-License-Identifier: Apache-2.0

package git

// The worker's report of its publication: what every GitTarget on the branch and every save it
// holds says during an outage, and what the metrics read. See
// docs/design/gittarget-branch-worker-pending-writes.md, step 5c.

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// An outage is reported once, from its start: a later failed attempt keeps that start, a pause is
// added to it, and a landed publication clears it.
func TestPublication_ReportsAnOutageFromItsStart(t *testing.T) {
	f, loop, restoreSyncs := outageLoop(t, "publication-outage")
	report := f.worker.Publication()
	require.True(t, report.Failing)
	assert.Contains(t, report.Cause, "connection refused")
	assert.False(t, report.IntakePaused)
	assert.False(t, report.ParentUnavailable)
	require.False(t, report.Since.IsZero())

	fireRetry(loop)
	loop.publishLoopState(0)
	require.True(t, loop.retry.pending())
	assert.Equal(t, report.Since, f.worker.Publication().Since, "a later failure keeps the outage's start")

	f.worker.branchBufferMaxBytes = 1
	loop.publishLoopState(0)
	assert.True(t, f.worker.Publication().IntakePaused)
	assert.Contains(t, f.worker.Publication().Message(), "intake of new changes is paused")

	restoreSyncs()
	fireRetry(loop)
	loop.publishLoopState(0)
	assert.Equal(t, PublicationStatus{}, f.worker.Publication(), "a landed publication clears the report")
}

// The GitTargets on the branch are told when the report changes, and only then: a retry that fails
// the same way again changes nothing a status would say.
func TestPublication_NotifiesOnlyWhenTheReportChanges(t *testing.T) {
	f, loop, restoreSyncs := outageLoop(t, "publication-notify")
	notified := 0
	f.worker.publicationReporter = func() { notified++ }

	fireRetry(loop)
	loop.publishLoopState(0)
	assert.Zero(t, notified, "the same failure again is no news")

	f.worker.branchBufferMaxBytes = 1
	loop.publishLoopState(0)
	assert.Equal(t, 1, notified, "the pause is")

	restoreSyncs()
	fireRetry(loop)
	loop.publishLoopState(0)
	assert.Equal(t, 2, notified, "and so is the recovery")
}

// The backlog an outage leaves is measurable without a status write per tick: what is kept,
// whether intake is paused, how old the oldest kept write is, and when the next retry is due. A
// failure to commit decided writes is counted, which no push metric could see.
func TestPublication_MetricsDescribeTheBacklog(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f, loop, restoreSyncs := outageLoop(t, "publication-metrics")
	manager := &WorkerManager{Log: logr.Discard(), workers: map[BranchKey]*BranchWorker{{Branch: "main"}: f.worker}}
	manager.setPublicationGaugeSources()
	t.Cleanup(func() {
		for _, name := range publicationGauges {
			telemetry.SetGaugeSource(name, nil)
		}
	})
	branch := map[string]string{"branch": "main"}
	gauge := func(name string) (int64, bool) {
		return telemetry.CollectInt64Sum(reader, "gitopsreverser_"+name, branch)
	}

	failures, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_git_materialization_failures_total",
		map[string]string{"branch": "main", "reason": "unreachable"})
	require.True(t, ok, "a decided write the remote could not take is counted")
	assert.Positive(t, failures)

	kept, ok := gauge(telemetry.GaugeGitRetainedWrites)
	require.True(t, ok)
	assert.Equal(t, int64(1), kept)
	bytes, _ := gauge(telemetry.GaugeGitRetainedBytes)
	assert.Positive(t, bytes)
	paused, _ := gauge(telemetry.GaugeGitIntakePaused)
	assert.Zero(t, paused)
	oldest, ok := gauge(telemetry.GaugeGitOldestRetainedWrite)
	require.True(t, ok)
	assert.Positive(t, oldest)
	nextRetry, ok := gauge(telemetry.GaugeGitNextRetry)
	require.True(t, ok)
	assert.Equal(t, loop.retry.due.Unix(), nextRetry)

	f.worker.branchBufferMaxBytes = 1
	loop.publishLoopState(0)
	paused, _ = gauge(telemetry.GaugeGitIntakePaused)
	assert.Equal(t, int64(1), paused)

	restoreSyncs()
	fireRetry(loop)
	loop.publishLoopState(0)
	kept, _ = gauge(telemetry.GaugeGitRetainedWrites)
	assert.Zero(t, kept)
	_, ok = gauge(telemetry.GaugeGitOldestRetainedWrite)
	assert.False(t, ok, "nothing kept, nothing to date")
	_, ok = gauge(telemetry.GaugeGitNextRetry)
	assert.False(t, ok, "nothing owed, no retry due")
}

// A change of the report reaches every GitTarget on the branch, and none on another branch: they
// share the worker, so they share its outage, and a second branch's worker is its own.
func TestPublication_ChangesReachEveryGitTargetOnTheBranch(t *testing.T) {
	target := func(name, branch string) *configv1alpha3.GitTarget {
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
			Spec: configv1alpha3.GitTargetSpec{
				GitProviderRef: meta.LocalObjectReference{Name: "repo"},
				Branch:         branch,
			},
		}
	}
	k8s := fake.NewClientBuilder().WithScheme(replayScheme(t)).WithObjects(
		target("apps", "main"), target("infra", "main"), target("other", "release")).Build()
	manager := NewWorkerManager(k8s, logr.Discard(), BranchWorkerLimits{}, itypes.SensitiveResourcePolicy{})

	manager.notifyPublication(context.Background(), BranchKey{RepoNamespace: "shop", RepoName: "repo", Branch: "main"})

	var told []string
	for len(manager.PublicationEvents()) > 0 {
		told = append(told, (<-manager.PublicationEvents()).Object.GetName())
	}
	assert.ElementsMatch(t, []string{"apps", "infra"}, told)
}
