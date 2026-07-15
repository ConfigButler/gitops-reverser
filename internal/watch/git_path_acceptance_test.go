// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The live-event write path has no result channel: a commit window is finalized on a timer, so
// a refused write plan used to be logged and dropped, leaving the GitTarget looking healthy
// while its edit was silently prevented. ReportGitPathRefusal is the hook the branch worker
// calls instead, and it must produce the same GitPathAccepted=False transition the resync path
// produces (TestDrainScopedResync_RefusalMarksGitPathRefused pins that side).

// TestReportGitPathRefusal_SurfacesWriteBoundaryRefusal proves a live write-boundary refusal
// reaches the GitTarget status surface with the specific WriteBoundaryRefused reason, naming
// the file the operator refused to write through.
func TestReportGitPathRefusal_SurfacesWriteBoundaryRefusal(t *testing.T) {
	mgr := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("podinfo-test", "team-a")

	mgr.ReportGitPathRefusal(gitDest, &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind:    manifestanalyzer.IssueWriteFanIn,
			Path:    "base/deployment.yaml",
			Message: "more than one kustomize render path reaches it",
		}},
	})

	gitPath := mgr.GitPathAcceptanceForGitTarget(gitDest)
	assert.False(t, gitPath.Accepted, "a refused live write must mark the target Git path unaccepted")
	assert.Equal(t, "WriteBoundaryRefused", gitPath.Reason,
		"a pure write-boundary refusal must not hide behind the umbrella UnsupportedContent reason")
	assert.Contains(t, gitPath.Message, "base/deployment.yaml", "the refusal must name the offending file")
	assert.Empty(t, mgr.targetStreamStates, "a Git path refusal must not mutate stream readiness")
}

// TestReportGitPathRefusal_ContentRefusalKeepsUmbrellaReason pins the fallback: a live refusal
// that is not purely a write-boundary violation still surfaces, under UnsupportedContent.
func TestReportGitPathRefusal_ContentRefusalKeepsUmbrellaReason(t *testing.T) {
	mgr := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("podinfo-test", "team-a")

	mgr.ReportGitPathRefusal(gitDest, &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind:    manifestanalyzer.IssueForeignFile,
			Path:    "notes.txt",
			Message: "foreign file",
		}},
	})

	gitPath := mgr.GitPathAcceptanceForGitTarget(gitDest)
	assert.False(t, gitPath.Accepted)
	assert.Equal(t, "UnsupportedContent", gitPath.Reason)
}

// TestReportGitPathRefusal_SatisfiesWorkerManagerReporter is a compile-time proof that the
// Manager method can be installed as the branch workers' refusal hook, so the live-path wiring
// in cmd/main.go cannot drift out of shape unnoticed.
func TestReportGitPathRefusal_SatisfiesWorkerManagerReporter(t *testing.T) {
	var reporter git.PathRefusalReporter = (&Manager{Log: logr.Discard()}).ReportGitPathRefusal
	assert.NotNil(t, reporter)
}

func TestRenderFidelityStatus_ReducesCurrentEpochScopes(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	manager := &Manager{Log: logr.Discard()}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	deployment := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	other := targetWatchKey{GVR: configmapsGVR, Namespace: "ops"}

	manager.targetWatchesMu.Lock()
	manager.beginTargetRenderFidelityEpochLocked(target, []targetWatchKey{deployment, other})
	epoch := manager.targetRenderFidelity[target.Key()].Epoch
	manager.targetWatchesMu.Unlock()

	manager.MarkTargetRenderFidelityScopeClean(target, epoch, deployment)
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State)
	manager.MarkTargetRenderFidelityScopeDiverged(target, epoch, other,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	assert.Equal(t, git.RenderFidelityFalse, manager.RenderFidelityForGitTarget(target).State)

	manager.MarkTargetRenderFidelityScopeClean(target, epoch, other)
	assert.Equal(t, git.RenderFidelityFalse, manager.RenderFidelityForGitTarget(target).State,
		"a later clean result cannot overwrite the failed scope in the same epoch")

	manager.targetWatchesMu.Lock()
	manager.beginTargetRenderFidelityEpochLocked(target, []targetWatchKey{deployment, other})
	freshEpoch := manager.targetRenderFidelity[target.Key()].Epoch
	manager.targetWatchesMu.Unlock()
	manager.MarkTargetRenderFidelityScopeClean(target, epoch, deployment)
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State,
		"a stale result from the previous epoch must be ignored")
	manager.MarkTargetRenderFidelityScopeClean(target, freshEpoch, deployment)
	manager.MarkTargetRenderFidelityScopeClean(target, freshEpoch, other)
	assert.Equal(t, git.RenderFidelityTrue, manager.RenderFidelityForGitTarget(target).State)
}

func TestReportGitPathRefusal_RenderFidelityKeepsGitPathAccepted(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	manager := &Manager{Log: logr.Discard()}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")

	manager.ReportGitPathRefusal(target, &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind: manifestanalyzer.IssueRenderDoesNotMatchLive, Field: "data.region", Token: "${REGION}",
		}},
	})

	assert.True(t, manager.GitPathAcceptanceForGitTarget(target).Accepted)
	fidelity := manager.RenderFidelityForGitTarget(target)
	assert.Equal(t, git.RenderFidelityFalse, fidelity.State)
	assert.Equal(t, "RenderDoesNotMatchLive", fidelity.Reason)
}
