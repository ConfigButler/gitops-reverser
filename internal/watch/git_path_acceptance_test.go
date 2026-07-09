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
