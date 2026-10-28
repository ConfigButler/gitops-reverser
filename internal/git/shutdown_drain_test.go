// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// A clean shutdown closes the open window and pushes it. The manager stops a worker by canceling the
// context it started it with, and a push made on that context was refused before it reached the
// remote: the save attached to the window failed with "worker stopped before the commit reached the
// remote", and the edit was lost with the checkout. Only a real server shows it; go-git's file://
// transport never reads the context.
func TestShutdown_TheLastWindowIsPushedOnceTheManagerCancels(t *testing.T) {
	f := newLedgerFixture(t, "shutdown-push", true)
	worker := startDrainWorker(t, f)
	parent, cancel := context.WithCancel(context.Background())
	require.NoError(t, worker.Start(parent))

	const message = "save: the window open at shutdown"
	collectAttached(t, worker, "saved-at-shutdown", message)

	cancel()
	worker.Stop()

	res, ok := outcome(t, worker)
	require.True(t, ok, "the save is settled before the worker exits")
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Contains(t, configMapFileNames(t, f.repoDir), "saved-at-shutdown.yaml")
	tip := remoteTip(t, f.repoDir)
	assert.Equal(t, tip.Hash.String(), res.Commit, "the save reports the commit on the remote")
	assert.Equal(t, message, tip.Message)
}

// The shutdown's work is bounded: a remote that stalls the last push is cut off at
// workerShutdownBudget, and the save it carried fails rather than holding the process past its grace.
func TestShutdown_AStalledLastPushEndsAtTheBudget(t *testing.T) {
	f := newLedgerFixture(t, "shutdown-stall", true)
	worker := startDrainWorker(t, f)
	budget := workerShutdownBudget
	workerShutdownBudget = stallBound
	t.Cleanup(func() { workerShutdownBudget = budget })
	parent, cancel := context.WithCancel(context.Background())
	require.NoError(t, worker.Start(parent))

	collectAttached(t, worker, "stalled-at-shutdown", "save: stalled")
	stallPhase(f.sim, isReceivePack)
	t.Cleanup(func() { f.sim.around.Store(nil) })

	cancel()
	require.NoError(t, requireReturns(t, "Stop", func() error {
		worker.Stop()
		return nil
	}))

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), "worker stopped")
}

// startDrainWorker readies the fixture's worker to run its own loop on team-a.
func startDrainWorker(t *testing.T, f *ledgerFixture) *BranchWorker {
	t.Helper()
	createPlainGitTarget(t, f.worker, "team-a", "team-a")
	t.Cleanup(f.worker.Stop)
	return f.worker
}

// collectAttached opens a window with one edit and attaches a request to it, then waits until the
// running loop reports it collecting. The request's timers are long, so only a stop closes it.
func collectAttached(t *testing.T, worker *BranchWorker, name, message string) {
	t.Helper()
	require.True(t, worker.Enqueue(configMapTargetEvent(name, "alice", "team-a")))
	req := attachReq("alice", time.Hour)
	req.Message = message
	worker.EnqueueAttach(req)
	require.Eventually(t, func() bool {
		return worker.LookupCommitRequestPhase("default", crName, "uid-"+crName) == PhaseCollectingWindow
	}, 10*time.Second, 10*time.Millisecond, "the request attaches to the open window")
}

// remoteTip is the commit at main in a bare repository.
func remoteTip(t *testing.T, bareRepoDir string) *object.Commit {
	t.Helper()
	repo, err := gogit.PlainOpen(bareRepoDir)
	require.NoError(t, err)
	ref, err := repo.Reference("refs/heads/main", true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	return commit
}

// The manager runs what SetAfterWorkersStopped injected once every worker has stopped, so the outcomes
// the workers settled are there to read, and on a context of its own, since the manager's is canceled.
func TestShutdown_TheManagerRunsTheSettleOnceTheWorkersStopped(t *testing.T) {
	f := newLedgerFixture(t, "shutdown-manager", true)
	worker := startDrainWorker(t, f)
	m := NewWorkerManager(f.worker.Client, logr.Discard(), BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	m.workers[BranchKey{Repository: f.sim.RepoURL, Branch: "main"}] = worker
	parent, cancel := context.WithCancel(context.Background())
	require.NoError(t, worker.Start(parent))
	collectAttached(t, worker, "settled-after-stop", "save: read after the stop")

	var (
		settled  FinalizeResult
		resolved bool
		ctxErr   error
		deadline time.Time
	)
	m.SetAfterWorkersStopped(func(ctx context.Context) {
		settled, resolved = outcome(t, worker)
		ctxErr = ctx.Err()
		deadline, _ = ctx.Deadline()
	})
	done := make(chan error, 1)
	go func() { done <- m.Start(parent) }()
	cancel()
	require.NoError(t, <-done)

	require.True(t, resolved, "the worker had settled the save before the step ran")
	assert.Equal(t, FinalizeCommitted, settled.Outcome)
	require.NoError(t, ctxErr, "the step runs on a live context")
	assert.WithinDuration(t, time.Now().Add(afterWorkersStoppedBudget), deadline, afterWorkersStoppedBudget)
}

// shutdownGraceMargin is what a stop needs beyond the workers' drain and the settle step: the caches
// and the rest of the runnables stopping, and the process exiting.
const shutdownGraceMargin = 5 * time.Second

// Both deployments give the pod time for the whole drain: the workers' last push
// (workerShutdownBudget), then the CommitRequest outcomes written after it (afterWorkersStoppedBudget).
// A shorter grace period has the kubelet kill the pod before the outcomes are written: the e2e
// manifest once set 10s.
func TestShutdown_EveryDeploymentWaitsForTheDrain(t *testing.T) {
	need := workerShutdownBudget + afterWorkersStoppedBudget + shutdownGraceMargin
	grace := regexp.MustCompile(`(?m)^\s*terminationGracePeriodSeconds:\s*(\d+)\s*$`)
	for _, manifest := range []string{
		"../../config/deployment.yaml",
		"../../charts/gitops-reverser/templates/deployment.yaml",
	} {
		t.Run(filepath.Base(filepath.Dir(manifest))+"/"+filepath.Base(manifest), func(t *testing.T) {
			content, err := os.ReadFile(manifest)
			require.NoError(t, err)
			match := grace.FindSubmatch(content)
			require.NotNil(t, match, "the manifest sets terminationGracePeriodSeconds")
			seconds, err := strconv.Atoi(string(match[1]))
			require.NoError(t, err)
			assert.GreaterOrEqual(t, time.Duration(seconds)*time.Second, need,
				"terminationGracePeriodSeconds covers the workers' drain, the settle step and a margin")
		})
	}
}
