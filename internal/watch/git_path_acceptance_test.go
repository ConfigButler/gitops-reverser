// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime/schema"

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
	assert.Empty(t, mgr.watchPlane().streams, "a Git path refusal must not mutate stream readiness")
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

func TestRenderFidelityStatus_ReducesTheCurrentPlansScopes(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	manager := &Manager{Log: logr.Discard()}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	deployment := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	other := targetWatchKey{GVR: configmapsGVR, Namespace: "ops"}

	revisions := manager.restartAllFidelityScopes(target, deployment, other)

	manager.MarkTargetRenderFidelityScopeClean(target, revisions[deployment.Cell()], deployment.Cell())
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State)
	manager.MarkTargetRenderFidelityScopeDiverged(target, revisions[other.Cell()], other.Cell(),
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	assert.Equal(t, git.RenderFidelityFalse, manager.RenderFidelityForGitTarget(target).State)

	manager.MarkTargetRenderFidelityScopeClean(target, revisions[other.Cell()], other.Cell())
	assert.Equal(t, git.RenderFidelityFalse, manager.RenderFidelityForGitTarget(target).State,
		"a later clean result cannot overwrite the failed scope under the same revision")

	fresh := manager.restartAllFidelityScopes(target, deployment, other)
	manager.MarkTargetRenderFidelityScopeClean(target, revisions[deployment.Cell()], deployment.Cell())
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State,
		"a stale result from the previous revision must be ignored")
	manager.MarkTargetRenderFidelityScopeClean(target, fresh[deployment.Cell()], deployment.Cell())
	manager.MarkTargetRenderFidelityScopeClean(target, fresh[other.Cell()], other.Cell())
	assert.Equal(t, git.RenderFidelityTrue, manager.RenderFidelityForGitTarget(target).State)
}

// restartAllFidelityScopes reconciles the gate with every scope restarted, the shape a forced
// recheck produces.
func (m *Manager) restartAllFidelityScopes(
	target types.ResourceReference,
	keys ...targetWatchKey,
) map[types.CellKey]uint64 {
	cells := cellsForWatchKeys(keys)
	revisions, _ := m.reconcileTargetRenderFidelity(target, cells, cells)
	return revisions
}

// A stream carries the revision it was STARTED with, so a cancelled stream still in flight with
// a replay result cannot report a scope clean under a revision it never replayed for. Reading
// the cell's current revision when the result was ready is what made that possible, and a scope is
// now a cell — so a stream retired by a served-version change lands squarely on the live cell's
// scope instead of missing it.
func TestTargetWatchStream_CarriesTheRevisionItWasStartedWith(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	manager := &Manager{Log: logr.Discard()}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	v1 := targetWatchKey{
		GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Namespace: "apps"}
	v2 := targetWatchKey{
		GVR: schema.GroupVersionResource{Group: "apps", Version: "v2", Resource: "deployments"}, Namespace: "apps"}

	retired := manager.restartAllFidelityScopes(target, v1)[v1.Cell()]
	require.NotZero(t, retired, "the revision a started stream captures")

	live := manager.restartAllFidelityScopes(target, v2)[v2.Cell()]
	require.NotEqual(t, retired, live)

	// The retired v1 stream reports its replay clean. Same cell as the live v2 stream, older
	// revision: it must not reopen writes for a snapshot the new plan never gathered.
	manager.MarkTargetRenderFidelityScopeClean(target, retired, v1.Cell())
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State)

	manager.MarkTargetRenderFidelityScopeClean(target, live, v2.Cell())
	assert.Equal(t, git.RenderFidelityTrue, manager.RenderFidelityForGitTarget(target).State,
		"the live stream's own result is what reopens writes")
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

// TestMarkRenderFidelityScopeClean_NamesAResultTheGateWouldNotTake covers the branch that made
// Failure A undiagnosable: the gate answers applied=false for a stale revision, an unknown scope
// or an unknown target, and the caller used to discard that answer without a word — so a scope
// could owe a report for ever with nothing anywhere saying why.
func TestMarkRenderFidelityScopeClean_NamesAResultTheGateWouldNotTake(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	log, lines := recordingLogger()
	manager := &Manager{Log: log}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	cell := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}

	first := manager.restartAllFidelityScopes(target, cell)
	// Restart it so the earlier revision is genuinely superseded rather than merely absent.
	manager.restartAllFidelityScopes(target, cell)

	manager.MarkTargetRenderFidelityScopeClean(target, first[cell.Cell()], cell.Cell())

	joined := strings.Join(*lines, "\n")
	assert.Contains(t, joined, "a render scope result was not applied")
	assert.Contains(t, joined, cell.Cell().String())
	assert.Equal(t, git.RenderFidelityUnknown, manager.RenderFidelityForGitTarget(target).State,
		"a refused report must not converge the target")
}

// TestMarkRenderFidelityScopeClean_NamesAReportWithNoRevision covers the earlier return. A wired
// gate always issues a non-zero revision, so a stream reporting under zero was started without
// one; its result is unusable and its scope keeps owing a report.
func TestMarkRenderFidelityScopeClean_NamesAReportWithNoRevision(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	log, lines := recordingLogger()
	manager := &Manager{Log: log}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	cell := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	manager.restartAllFidelityScopes(target, cell)

	manager.MarkTargetRenderFidelityScopeClean(target, 0, cell.Cell())

	assert.Contains(t, strings.Join(*lines, "\n"), "stream carries no revision")
}

// TestRenderFidelityStatus_PublishesTheCurrentStatusNotTheObservedOne pins the ordering fix.
//
// Sibling drains record concurrently and their publishes can reorder, so a drain that observed
// "one scope still pending" could write that over the fresh "True" a later drain had already
// published. Publishing what the GATE currently says instead of what this drain saw removes the
// race.
func TestRenderFidelityStatus_PublishesTheCurrentStatusNotTheObservedOne(t *testing.T) {
	workerManager := git.NewWorkerManager(nil, logr.Discard(), 0, types.SensitiveResourcePolicy{})
	manager := &Manager{Log: logr.Discard()}
	manager.EventRouter = NewEventRouter(workerManager, manager, nil, logr.Discard())
	target := types.NewResourceReference("podinfo-test", "team-a")
	first := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	second := targetWatchKey{GVR: configmapsGVR, Namespace: "ops"}

	revisions := manager.restartAllFidelityScopes(target, first, second)

	// Complete the set, so the gate is True...
	manager.MarkTargetRenderFidelityScopeClean(target, revisions[first.Cell()], first.Cell())
	manager.MarkTargetRenderFidelityScopeClean(target, revisions[second.Cell()], second.Cell())
	require.Equal(t, git.RenderFidelityTrue, manager.RenderFidelityForGitTarget(target).State)

	// ...then let a straggler re-report the scope it already reported. Under the old behaviour it
	// published the status IT computed; the published projection must still describe a converged
	// target, because that is what the gate says.
	manager.MarkTargetRenderFidelityScopeClean(target, revisions[first.Cell()], first.Cell())

	published := manager.watchPlane().fidelity[target.Key()]
	assert.Equal(t, git.RenderFidelityTrue, published.State,
		"the published projection must follow the gate, not one drain's snapshot of it")
}
