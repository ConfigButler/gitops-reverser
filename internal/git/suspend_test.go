// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// spec.suspend is a write gate and nothing more: the scan in front of it still runs, and the
// layout that scan resolves is still published. Both halves are load-bearing, and the second
// one is the half that would rot silently — a suspend that also stopped scanning would leave
// status.placement frozen at whatever the folder looked like when suspension began, so the dry
// run would show a stale answer with no way to tell.

// suspendedTarget is a resolved target metadata record with suspend set, writing at base.
func suspendedTarget(suspend bool) ResolvedTargetMetadata {
	return ResolvedTargetMetadata{
		Name:      "checkout",
		Namespace: "shop",
		Suspend:   suspend,
	}
}

// captureLayout installs a reporter that records every published report.
func captureLayout(w *BranchWorker) *[]LayoutReport {
	var reports []LayoutReport
	w.layoutReporter = func(_ types.ResourceReference, report LayoutReport) {
		reports = append(reports, report)
	}
	return &reports
}

// A suspended target folds no event into the worktree: the file the event would have created
// is not there, and the flush reports no change, so nothing is committed and nothing is pushed.
func TestSuspend_LiveWriteCreatesNothing(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}

	event := newConfigMapEvent("cache", "app")
	targets := map[pendingTargetKey]ResolvedTargetMetadata{
		{}: suspendedTarget(true),
	}

	changed, err := worker.applyPendingWriteEvents(t.Context(), repoFor(t, worktree), worktree, []Event{event}, targets)

	require.NoError(t, err)
	assert.False(t, changed, "a suspended target must report no change, so no commit is created")
	_, statErr := os.Stat(filepath.Join(root, "app", "configmaps", "cache.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a suspended target must write no file")
}

// The same event against the same target, not suspended, does create the file. Without this
// the test above would pass just as well if placement were broken.
func TestSuspend_UnsuspendedTargetStillWrites(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}

	targets := map[pendingTargetKey]ResolvedTargetMetadata{
		{}: suspendedTarget(false),
	}
	changed, err := worker.applyPendingWriteEvents(
		t.Context(), repoFor(t, worktree), worktree, []Event{newConfigMapEvent("cache", "app")}, targets)

	require.NoError(t, err)
	assert.True(t, changed)
	_, statErr := os.Stat(filepath.Join(root, "app", "configmaps", "cache.yaml"))
	assert.NoError(t, statErr, "an active target writes the file the suspended one withheld")
}

// Suspend's cutover, pinned. The gate reads the value CAPTURED when the write was planned, not
// the live GitTarget, so a suspension arriving after planning does not retract the write — and a
// write already committed locally is still pushed by the retained-writes path, which never
// consults suspend at all.
//
// That is the contract rather than a gap in it: a local commit that is never pushed would sit in
// the worker's checkout indefinitely and surface later, out of order, on resume. Suspend is a
// valve on new work. This test exists so that reading is a decision the suite defends rather than
// an accident of where the flag happens to be read.
func TestSuspend_IsTheValueCapturedWhenTheWriteWasPlanned(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}

	// The metadata this write was planned under: not suspended. The GitTarget in the cluster may
	// have been suspended since, and this write does not care.
	planned := suspendedTarget(false)
	targets := map[pendingTargetKey]ResolvedTargetMetadata{{}: planned}

	changed, err := worker.applyPendingWriteEvents(
		t.Context(), repoFor(t, worktree), worktree, []Event{newConfigMapEvent("cache", "app")}, targets)

	require.NoError(t, err)
	assert.True(t, changed, "a write planned before suspension still lands")
	_, statErr := os.Stat(filepath.Join(root, "app", "configmaps", "cache.yaml"))
	assert.NoError(t, statErr)
}

// The half that keeps a stopped target's status fresh rather than frozen: the suspended target still
// scans, so it still publishes what its folder resolved to: a valve that stopped looking as well
// as writing would freeze status.placement at whatever the folder looked like when someone
// panicked, which is exactly when a stale answer costs the most.
func TestSuspend_StillPublishesTheResolvedLayout(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedFile(t, root, "kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: app\nresources:\n- web.yaml\n")
	seedFile(t, root, "web.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\ndata:\n  k: v\n")

	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	reports := captureLayout(worker)
	targets := map[pendingTargetKey]ResolvedTargetMetadata{
		{}: suspendedTarget(true),
	}

	_, err := worker.applyPendingWriteEvents(
		t.Context(), repoFor(t, worktree), worktree, []Event{newConfigMapEvent("cache", "app")}, targets)
	require.NoError(t, err)

	require.Len(t, *reports, 1, "a suspended target must still publish what it resolved")
	report := (*reports)[0]
	assert.Equal(t, manifestanalyzer.LayoutSingleKustomization, report.Reason)
	assert.Equal(t, ".", report.RenderRoot)
	assert.Equal(t, manifestanalyzer.LayoutModeKustomizeRoot, report.Mode)
}

// The report is a fact about the folder, so it does not wait for a placement to happen: it is
// published by the scan that precedes the write, which is the property status.placement rests
// on and the one a later refactor is most likely to break.
func TestLayoutReport_PublishedBeforeAnythingIsWritten(t *testing.T) {
	worktree := newWorktreeForTest(t)
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	reports := captureLayout(worker)

	// An empty folder: no kustomization, no documents, and no write has ever happened here.
	err := worker.refuseUnsafeWorktree(t.Context(), worktree, "", suspendedTarget(true))
	require.NoError(t, err)

	require.Len(t, *reports, 1)
	assert.Equal(t, manifestanalyzer.LayoutNone, (*reports)[0].Reason,
		"an empty folder resolves to the canonical ladder, and says so before it is written to")
}

// An unattributable batch publishes nothing rather than filing a report under a key no
// GitTarget reads. The CLI and most unit tests are exactly this case.
func TestLayoutReport_UnattributableBatchPublishesNothing(t *testing.T) {
	worktree := newWorktreeForTest(t)
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	reports := captureLayout(worker)

	err := worker.refuseUnsafeWorktree(t.Context(), worktree, "", ResolvedTargetMetadata{Path: ""})
	require.NoError(t, err)

	assert.Empty(t, *reports, "a batch that names no GitTarget must publish no report")
}

// The report is a fact about the FOLDER and about nothing else. The target's declared placement
// policy does not reach it: a reader who wants to know what was declared reads the spec in the
// same GET, and a status field that copied it would be a second place to look that can disagree
// with the first.
func TestLayoutReport_DoesNotRestateTheDeclaredPolicy(t *testing.T) {
	worktree := newWorktreeForTest(t)
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	reports := captureLayout(worker)

	target := suspendedTarget(true)
	target.Placement = &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/secrets": "secrets/{name}.yaml"},
	}
	require.NoError(t, worker.refuseUnsafeWorktree(t.Context(), worktree, "", target))

	require.Len(t, *reports, 1)
	report := (*reports)[0]
	assert.Equal(t, manifestanalyzer.LayoutNone, report.Reason)
	assert.Equal(t, manifestanalyzer.LayoutModePlain, report.Mode,
		"an empty folder is written as plain files whatever the target declares")
	assert.Empty(t, report.RenderRoot)
	assert.Empty(t, report.ReadOnlyBases)
}

func repoFor(t *testing.T, worktree *gogit.Worktree) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainOpen(worktree.Filesystem().Root())
	require.NoError(t, err)
	return repo
}

// bootstrapEnabledTarget is a target at base that stages its path's bootstrap files —
// .gittargetignore always, .sops.yaml because the recipient makes the SOPS half renderable.
func bootstrapEnabledTarget(name, base string, suspend bool) ResolvedTargetMetadata {
	return ResolvedTargetMetadata{
		Name:      name,
		Namespace: "shop",
		Path:      base,
		Suspend:   suspend,
		BootstrapOptions: pathBootstrapOptions{
			Enabled:           true,
			IncludeSOPSConfig: true,
			TemplateData:      bootstrapTemplateData{AgeRecipients: []string{"age1exampleexampleexample"}},
		},
	}
}

func bootstrapEventFor(md ResolvedTargetMetadata, name string) Event {
	event := newConfigMapEvent(name, "app")
	event.Path = md.Path
	event.BootstrapOptions = md.BootstrapOptions
	return event
}

// A suspended target leaves NOTHING behind, including files it did not author as content.
//
// Bootstrap staging writes .gittargetignore and .sops.yaml into the target's path and adds
// them to the index — and the index belongs to the branch, not to one target. Staging it for a
// suspended path therefore used to smuggle those files into the next ACTIVE target's commit,
// so a target that was supposed to be writing nothing appeared in history with two files in
// its folder. This asserts both halves: nothing on disk, and nothing in the commit.
func TestSuspend_StagesNoBootstrapFilesIntoTheNextCommit(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	repo := repoFor(t, worktree)
	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}

	suspended := bootstrapEnabledTarget("suspended", "suspended", true)
	active := bootstrapEnabledTarget("active", "active", false)
	targets := map[pendingTargetKey]ResolvedTargetMetadata{
		{Name: suspended.Name, Namespace: suspended.Namespace}: suspended,
		{Name: active.Name, Namespace: active.Namespace}:       active,
	}

	changed, err := worker.applyPendingWriteEvents(t.Context(), repo, worktree, []Event{
		bootstrapEventFor(suspended, "cache"),
		bootstrapEventFor(active, "cache"),
	}, targets)
	require.NoError(t, err)
	require.True(t, changed, "the active target wrote, so this window commits")

	// The active target commits. Anything the suspended path left in the index rides along.
	hash, err := worktree.Commit("active target write", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	for _, name := range []string{gitTargetIgnoreFileName, sopsConfigFileName} {
		_, statErr := os.Stat(filepath.Join(root, "suspended", name))
		assert.True(t, os.IsNotExist(statErr),
			"a suspended target must not have %s written into its folder", name)
		assert.False(t, commitHoldsPath(t, repo, hash, "suspended/"+name),
			"a suspended target's %s must not reach a commit", name)
	}

	// The control: the active target got both, so the assertions above are about suspend
	// rather than about bootstrap staging having quietly stopped working.
	for _, name := range []string{gitTargetIgnoreFileName, sopsConfigFileName} {
		assert.True(t, commitHoldsPath(t, repo, hash, "active/"+name),
			"the active target's %s must still be committed", name)
	}
}

// commitHoldsPath reports whether a commit's tree holds a path.
func commitHoldsPath(t *testing.T, repo *gogit.Repository, hash plumbing.Hash, path string) bool {
	t.Helper()
	commit, err := repo.CommitObject(hash)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	if _, err := tree.File(path); err != nil {
		require.ErrorIs(t, err, object.ErrFileNotFound)
		return false
	}
	return true
}
