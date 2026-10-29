// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// The tests here pin docs/design/concurrent-author-windows.md: a window per author while their
// footprints are proven not to overlap, and serial everywhere else.

// newAdmissionFixture is an echo fixture whose folder the worker has bootstrapped with a commit of its
// own, so writes there can be proven, and whose windows stay open for an hour unless something closes
// them. Pushes are held.
func newAdmissionFixture(t *testing.T, slug string, files map[string]string) *echoFixture {
	t.Helper()
	f := newEchoFixtureWith(t, slug, configv1alpha3.PruneOnEvent,
		map[string]string{"x": "x1", "z": "z1", "q": "q1"}, files)
	holdPushes(f.loop)
	f.writeWindow(echoEvent("q", "q2")) // the zero window commits at once, with the bootstrap files
	require.Empty(t, f.loop.windows.all())
	f.loop.defaultWindow = commitWindowDefaults{idleTimeout: time.Hour, maxDuration: time.Hour}
	return f
}

func (f *echoFixture) writeBy(author, name, value string) {
	event := echoEvent(name, value)
	event.UserInfo.Username = author
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}}})
}

func authorsOf(windows []*openWindow) []string {
	var authors []string
	for _, w := range windows {
		authors = append(authors, w.Author)
	}
	return authors
}

func TestWindowAdmission_AuthorsOfDisjointFilesKeepAWindowEach(t *testing.T) {
	f := newAdmissionFixture(t, "admission-disjoint", nil)

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("alice", "x", "x3")

	open := f.loop.windows.all()
	require.Equal(t, []string{"alice", "bob"}, authorsOf(open), "oldest first")
	require.NotNil(t, open[0].footprint)
	assert.Equal(t, []string{echoFile("x")}, open[0].footprint.paths)
	assert.Equal(t, []string{echoFile("z")}, open[1].footprint.paths)
	assert.Less(t, open[0].seq, open[1].seq)

	// Bob's window closes first; commits land in close order.
	open[1].timers.maxAt = time.Now().Add(-time.Millisecond)
	f.loop.closeOrArmWindow()
	f.loop.endWake(0)
	require.Equal(t, []string{"alice"}, authorsOf(f.loop.windows.all()))
	f.loop.finalizeOpenWindowWithReason(windowFinalizeReasonShutdown)
	f.loop.endWake(0)
	f.loop.pushPending()

	assert.Equal(
		t,
		[]string{"bob", "alice"},
		splitLines(gitIn(t, f.repoDir, "log", "--reverse", "--format=%an", "-2", "main")),
	)
	assert.Equal(t, echoDocument("x", "x3"), f.onMain(echoFile("x")))
	assert.Equal(t, echoDocument("z", "z2"), f.onMain(echoFile("z")))
}

func TestWindowAdmission_AWriteToAFileAnotherWindowHoldsClosesThatWindowFirst(t *testing.T) {
	f := newAdmissionFixture(t, "admission-conflict", nil)
	reader := captureInfo(f.worker)

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("carol", "x", "x3")

	assert.Equal(t, []string{"bob", "carol"}, authorsOf(f.loop.windows.all()))
	require.Len(t, f.loop.pendingWrites, 2, "the bootstrap write, and alice's window")
	assert.Equal(t, "alice", f.loop.pendingWrites[1].Author(), "decided before carol's write opened a window")
	assert.Len(
		t,
		reader.matching(`"msg"="Finalizing open commit window" "reason"="path-conflict" "windowAuthor"="alice"`),
		1,
	)
}

func TestWindowAdmission_AWriteThatCannotBeProvenStaysSerial(t *testing.T) {
	shared := echoDocument("s1", "a") + "---\n" + echoDocument("s2", "b")
	f := newAdmissionFixture(t, "admission-unproven",
		map[string]string{echoTarget + "/default/configmaps/shared.yaml": shared})

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "s1", "c")

	open := f.loop.windows.all()
	require.Equal(t, []string{"bob"}, authorsOf(open), "a document in a shared file closes every other window")
	require.NotNil(t, open[0].footprint)
	assert.False(t, open[0].footprint.known())
	assert.Equal(t, "alice", f.loop.pendingWrites[len(f.loop.pendingWrites)-1].Author())

	// And an unknown window closes before a write is admitted beside it.
	f.writeBy("carol", "z", "z2")
	assert.Equal(t, []string{"carol"}, authorsOf(f.loop.windows.all()))
}

func TestWindowAdmission_AWorkerThatFellBackHoldsOneWindow(t *testing.T) {
	f := newAdmissionFixture(t, "admission-single", nil)
	f.loop.singleWindow = true

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")

	assert.Equal(t, []string{"bob"}, authorsOf(f.loop.windows.all()))
}

func TestWindowAdmission_ClosingOneWindowArmsTheTimerForTheOthers(t *testing.T) {
	f := newAdmissionFixture(t, "admission-rearm", nil)
	clock := useTestClock(f.worker)

	f.writeBy("alice", "x", "x2")
	clock.advance(time.Minute)
	f.writeBy("bob", "z", "z2")
	open := f.loop.windows.all()
	require.Len(t, open, 2)
	aliceAt, _ := open[0].closeAt()
	bobAt, _ := open[1].closeAt()
	require.Equal(t, aliceAt, f.loop.commitTimerAt, "armed for the earliest deadline")

	f.loop.finalizeWindow(open[0], windowFinalizeReasonAttachNext)

	require.NotNil(t, f.loop.commitTimer, "bob's window still has a deadline to keep")
	assert.Equal(t, bobAt, f.loop.commitTimerAt)
}

func TestWindowAdmission_ShutdownClosesEveryWindowOldestFirst(t *testing.T) {
	f := newAdmissionFixture(t, "admission-shutdown", nil)
	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	require.Len(t, f.loop.windows.all(), 2)
	before := len(splitLines(gitIn(t, f.repoDir, "log", "--format=%H", "main")))

	f.loop.handleShutdown()

	assert.Empty(t, f.loop.windows.all())
	assert.Equal(t, []string{"alice", "bob"},
		splitLines(gitIn(t, f.repoDir, "log", "--reverse", "--format=%an", "-2", "main")),
		"one commit per window, in the order the windows opened")
	assert.Len(t, splitLines(gitIn(t, f.repoDir, "log", "--format=%H", "main")), before+3,
		"the bootstrap write held behind the cooldown, then the two windows")
	assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")))
	assert.Equal(t, echoDocument("z", "z2"), f.onMain(echoFile("z")))
}

// TestWindowAdmission_ShutdownDecidesTheWritesAlreadyQueued pins the writes the worker accepted and
// had not handled when the shutdown began: Enqueue told the watch they were taken, so the last push
// carries them, each in its author's window, and the items the loop does not decide at shutdown are
// still drained.
func TestWindowAdmission_ShutdownDecidesTheWritesAlreadyQueued(t *testing.T) {
	f := newAdmissionFixture(t, "admission-shutdown-queued", nil)
	f.loop.pushPending()
	f.writeBy("alice", "x", "x2")
	queued := func(author, name, value string) Event {
		event := echoEvent(name, value)
		event.UserInfo.Username = author
		return event
	}
	require.True(t, f.worker.Enqueue(queued("alice", "x", "x3")))
	f.worker.EnqueueAttach(&AttachCommitRequest{Namespace: "default", Name: "save", Author: "carol"})
	require.True(t, f.worker.Enqueue(queued("bob", "z", "z2")))

	f.loop.handleShutdown()

	assert.Empty(t, f.worker.eventQueue)
	assert.Equal(t, int64(0), f.worker.inflightItems.Load(), "every queued item is released")
	assert.Equal(t, echoDocument("x", "x3"), f.onMain(echoFile("x")), "alice's queued write is published")
	assert.Equal(t, echoDocument("z", "z2"), f.onMain(echoFile("z")), "bob's queued write is published")
	assert.Equal(t, []string{"alice", "bob"},
		splitLines(gitIn(t, f.repoDir, "log", "--reverse", "--format=%an", "-2", "main")),
		"each in its author's window")
}

func TestWindowAdmission_AWriteOfAnotherGitTargetClosesTheOpenWindows(t *testing.T) {
	f := newAdmissionFixture(t, "admission-other-target", nil)
	reader := captureInfo(f.worker)
	createGitTargetWithPruneMode(t, f.worker, "team-b", "team-b", configv1alpha3.PruneOnEvent)
	other := func(author, value string) Event {
		event := configMapTargetEvent("b", author, "team-b")
		event.Operation = string(itypes.OperationUpdate)
		event.Object.Object["data"] = map[string]interface{}{"key": value}
		return event
	}
	window := f.loop.defaultWindow
	f.loop.defaultWindow = commitWindowDefaults{}
	f.writeWindow(other("carol", "b1")) // team-b's folder is bootstrapped by its first commit
	require.Empty(t, f.loop.windows.all())
	f.loop.defaultWindow = window

	f.writeBy("alice", "x", "x2")
	f.writeWindow(other("bob", "b2"))

	assert.Equal(t, []string{"bob"}, authorsOf(f.loop.windows.all()))
	assert.Len(t, reader.matching(`"msg"="Finalizing open commit window" "reason"="author-or-target-change" `+
		`"windowAuthor"="alice"`), 1, "per-target encryption and bootstrap stay serial")
}

// TestWindowAdmission_AFootprintBugIsRefusedOnTheRealPath drives a footprint bug through the loop: two
// windows open side by side, the index is corrupted so x is proven at z's path, and alice's window
// closes on its timer. Her commit is checked before it is made, refused, and undone; her save fails,
// the worker falls back, and a resync is asked for.
func TestWindowAdmission_AFootprintBugIsRefusedOnTheRealPath(t *testing.T) {
	f := newAdmissionFixture(t, "admission-violation-path", nil)
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	f.writeBy("alice", "x", "x2")
	serviceAttach(f.loop, attachReq("alice", time.Hour))
	f.writeBy("bob", "q", "q3")
	open := f.loop.windows.all()
	require.Len(t, open, 2)
	require.NotNil(t, open[0].pendingCR, "alice's save rides her window")
	head := gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD")

	ix := f.loop.footprints[itypes.NewResourceReference(echoTarget, "default")]
	require.NotNil(t, ix)
	for dm, ref := range ix.docLoc {
		if strings.HasSuffix(ref.FilePath, "configmaps/x.yaml") {
			ref.FilePath = strings.TrimSuffix(ref.FilePath, "x.yaml") + "z.yaml"
			ix.docLoc[dm] = ref
		}
	}

	open[0].timers.maxAt = time.Now().Add(-time.Millisecond)
	f.loop.closeOrArmWindow()
	f.loop.endWake(0)

	assert.Equal(t, head, gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD"), "nothing was committed")
	assert.Empty(t, gitIn(t, f.worker.repoPath(), "status", "--porcelain"), "and the worktree was undone")
	assert.True(t, f.loop.singleWindow)
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(echoTarget, "default")}, asked)
	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), echoFile("x"))
}

// TestWindowAdmission_ACommitThatAddsOrDeletesAFileOutsideItsFootprintIsRefused: the check reads
// every staged change, a file added or deleted as much as one modified.
func TestWindowAdmission_ACommitThatAddsOrDeletesAFileOutsideItsFootprintIsRefused(t *testing.T) {
	deleted := echoEvent("x", "x1")
	deleted.Operation = string(itypes.OperationDelete)
	created := echoEvent("w", "w1")
	created.Operation = string(itypes.OperationCreate)
	for name, tc := range map[string]struct {
		event   Event
		outside string
	}{
		"a file added":   {event: created, outside: echoFile("w")},
		"a file deleted": {event: deleted, outside: echoFile("x")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, "admission-violation-"+strings.ReplaceAll(name, " ", "-"), nil)
			pendingWrite, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{tc.event})
			require.NoError(t, err)
			rev, ok := f.loop.currentRevision(echoTarget, "default")
			require.True(t, ok)
			pendingWrite.footprint = &footprint{paths: []string{echoFile("z")}, revision: rev}

			err = f.worker.commitPendingWrites([]PendingWrite{*pendingWrite})
			var violation *footprintViolationError
			require.ErrorAs(t, err, &violation)
			assert.Equal(t, []string{tc.outside}, violation.outside)
		})
	}
}

// TestFootprint_UnknownReasons pins every reason a write's footprint is unknown, and so serial, as
// proveEvents says it.
func TestFootprint_UnknownReasons(t *testing.T) {
	kustomization := func(body string) string {
		return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" + body
	}
	deleted := echoEvent("gone", "g")
	deleted.Operation = string(itypes.OperationDelete)
	created := echoEvent("fresh", "f1")
	created.Operation = string(itypes.OperationCreate)
	patch := deploymentScalePatch("api", 3)
	patch.GitTargetName, patch.GitTargetNamespace = echoTarget, "default"
	secret := echoEvent("s", "s1")
	secret.Identifier.Resource = "secrets"
	secret.Object.SetKind("Secret")

	for name, tc := range map[string]struct {
		files     map[string]string
		placement *configv1alpha3.GitTargetPlacementSpec
		event     Event
		why       string
	}{
		"a field patch":     {event: patch, why: "a field patch"},
		"an encrypted type": {event: secret, why: "an encrypted type"},
		"a generated object": {
			files: map[string]string{echoTarget + "/kustomization.yaml": kustomization(
				"configMapGenerator:\n- name: fresh\n  literals:\n  - a=b\n  options:\n    disableNameSuffixHash: true\n")},
			event: created, why: "an object a configMapGenerator renders",
		},
		"useKustomize": {
			placement: &configv1alpha3.GitTargetPlacementSpec{UseKustomize: true},
			event:     created, why: "placement.useKustomize is on",
		},
		"placement.default": {
			placement: &configv1alpha3.GitTargetPlacementSpec{Default: "{namespace}/all.yaml"},
			event:     created, why: "placement.default is set",
		},
		"a read outside spec.path": {
			files: map[string]string{
				echoTarget + "/kustomization.yaml": kustomization("resources:\n- ../shared\n"),
				"shared/kustomization.yaml":        kustomization("resources:\n- cm.yaml\n"),
				"shared/cm.yaml":                   echoDocument("shared", "s"),
			},
			event: created, why: "the folder reads files outside spec.path",
		},
		"a folder that feeds a generator": {
			files: map[string]string{
				echoTarget + "/kustomization.yaml": kustomization("configMapGenerator:\n- name: g\n  files:\n  - data.txt\n"),
				echoTarget + "/data.txt":           "data\n",
			},
			event: created, why: "the folder feeds a configMapGenerator",
		},
		"an unsupported kustomization": {
			files: map[string]string{echoTarget + "/kustomization.yaml": kustomization(
				"secretGenerator:\n- name: s\n  literals:\n  - a=b\n")},
			event: created, why: "the folder holds a kustomization the writer does not model",
		},
		"a delete of an object no document holds": {
			files: map[string]string{
				echoTarget + "/kust/kustomization.yaml": kustomization("resources:\n- k.yaml\n"),
				echoTarget + "/kust/k.yaml":             echoDocument("k", "k1"),
			},
			event: deleted, why: "a delete of an object no document holds, in a folder with a kustomization",
		},
		"a new document at a taken path": {
			files: map[string]string{echoFile("fresh"): "note: not a manifest\n"},
			event: created, why: "a new document at a path that is taken",
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				echoTarget + "/README.md":                  "readme\n",
				echoTarget + "/" + gitTargetIgnoreFileName: "\n",
			}
			maps.Copy(files, tc.files)
			f := newEchoFixtureWith(t, "footprint-"+strings.ReplaceAll(name, " ", "-"),
				configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"}, files)
			if tc.placement != nil {
				var target configv1alpha3.GitTarget
				require.NoError(t, f.worker.Client.Get(f.worker.ctx, keyOf(echoTarget), &target))
				target.Spec.Placement = tc.placement
				require.NoError(t, f.worker.Client.Update(f.worker.ctx, &target))
			}
			got := f.loop.proveEvents(echoTarget, "default", []Event{tc.event})
			assert.False(t, got.known())
			assert.Equal(t, tc.why, got.unknown)
		})
	}
}

func TestWindowAdmission_ACommitOutsideItsFootprintIsRefusedAndTheWorkerFallsBack(t *testing.T) {
	f := newAdmissionFixture(t, "admission-violation", nil)
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	head := gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD")

	pendingWrite, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{echoEvent("x", "x9")})
	require.NoError(t, err)
	rev, ok := f.loop.currentRevision(echoTarget, "default")
	require.True(t, ok)
	// A proof that names the wrong file, as a footprint bug would.
	pendingWrite.footprint = &footprint{paths: []string{echoFile("z")}, revision: rev}
	id := commitRequestID{Namespace: "default", Name: crName, UID: "uid-" + crName}
	pendingWrite.CommitRequest = &id

	err = f.worker.commitPendingWrites([]PendingWrite{*pendingWrite})
	var violation *footprintViolationError
	require.ErrorAs(t, err, &violation)
	assert.Equal(t, []string{echoFile("x")}, violation.outside)
	require.ErrorIs(t, err, errWriteFailed, "terminal for the write")
	assert.Equal(t, head, gitIn(t, f.worker.repoPath(), "rev-parse", "HEAD"), "nothing was committed")
	assert.Empty(t, gitIn(t, f.worker.repoPath(), "status", "--porcelain"), "and the worktree was undone")

	f.loop.pendingWrites = []PendingWrite{*pendingWrite}
	f.loop.settleFailed(0, err)

	assert.True(t, f.loop.singleWindow, "one window at a time for the rest of the worker's life")
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(echoTarget, "default")}, asked,
		"a resync brings Git back to the cluster")
	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), echoFile("x"), "the save says which path broke the footprint")
}

// TestWindowAdmission_AFallbackLeavesOneWindowOpen pins the fallback closing the windows the wrong
// rules admitted side by side: every one but the newest, oldest first, with footprint_fallback.
func TestWindowAdmission_AFallbackLeavesOneWindowOpen(t *testing.T) {
	f := newAdmissionFixture(t, "admission-fallback-closes", nil)
	reader := captureInfo(f.worker)
	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	f.writeBy("carol", "q", "q3")
	require.Len(t, f.loop.windows.all(), 3)
	decided := len(f.loop.pendingWrites)

	refused, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{echoEvent("w", "w1")})
	require.NoError(t, err)
	f.loop.pendingWrites = append(f.loop.pendingWrites, *refused)
	violation := &footprintViolationError{outside: []string{echoFile("w")}, footprint: footprint{}}
	f.loop.settleFailed(len(f.loop.pendingWrites)-1, fmt.Errorf("%w: %w", errWriteFailed, violation))

	require.True(t, f.loop.singleWindow)
	assert.Equal(t, []string{"carol"}, authorsOf(f.loop.windows.all()), "at most one window stays open")
	require.Len(t, f.loop.pendingWrites, decided+2, "alice's and bob's windows are decided")
	assert.Equal(t, "alice", f.loop.pendingWrites[decided].Author(), "oldest first")
	assert.Equal(t, "bob", f.loop.pendingWrites[decided+1].Author())
	assert.Len(t, reader.matching(`"msg"="Finalizing open commit window" "reason"="footprint-fallback"`), 2)
}

// TestWindowAdmission_TheGaugesShowConcurrencyAndAFallback pins what an operator sees of concurrent
// windows: how many are open side by side, and that a worker has fallen back to one at a time.
func TestWindowAdmission_TheGaugesShowConcurrencyAndAFallback(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newAdmissionFixture(t, "admission-gauges", nil)
	manager := &WorkerManager{Log: logr.Discard(), workers: map[BranchKey]*BranchWorker{{Branch: "main"}: f.worker}}
	manager.setPublicationGaugeSources()
	t.Cleanup(func() {
		for _, name := range publicationGauges {
			telemetry.SetGaugeSource(name, nil)
		}
	})
	gauge := func(name string) int64 {
		t.Helper()
		value, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_"+name, map[string]string{
			"gitbranch_namespace": f.worker.GitBranch.Namespace,
			"gitbranch_name":      f.worker.GitBranch.Name,
		})
		require.True(t, ok, name)
		return value
	}

	f.writeBy("alice", "x", "x2")
	f.writeBy("bob", "z", "z2")
	assert.Equal(t, int64(2), gauge(telemetry.GaugeGitCommitWindowsOpen))
	assert.Zero(t, gauge(telemetry.GaugeGitCommitWindowsSingle))

	refused, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{echoEvent("w", "w1")})
	require.NoError(t, err)
	f.loop.pendingWrites = append(f.loop.pendingWrites, *refused)
	violation := &footprintViolationError{outside: []string{echoFile("w")}, footprint: footprint{}}
	f.loop.settleFailed(len(f.loop.pendingWrites)-1, fmt.Errorf("%w: %w", errWriteFailed, violation))
	f.loop.publishLoopState(0)

	assert.Equal(t, int64(1), gauge(telemetry.GaugeGitCommitWindowsOpen))
	assert.Equal(t, int64(1), gauge(telemetry.GaugeGitCommitWindowsSingle), "the fallback lasts")
}

func TestFootprint_OverlapUnionAndOutside(t *testing.T) {
	a := footprint{paths: []string{"a", "b"}}
	b := footprint{paths: []string{"c"}}
	unknown := footprint{unknown: "why"}

	assert.False(t, a.overlaps(b))
	assert.True(t, a.overlaps(footprint{paths: []string{"b"}}))
	assert.True(t, a.overlaps(unknown), "unknown overlaps everything")
	assert.True(t, unknown.overlaps(footprint{}))
	assert.Equal(t, []string{"a", "b", "c"}, a.union(b).paths)
	assert.False(t, a.union(unknown).known())
	assert.Equal(t, []string{"d"}, a.outside([]string{"a", "d"}))
	assert.Equal(t, "[a b]", a.String())
	assert.Equal(t, "unknown: why", unknown.String())
	assert.Equal(t, "unproven", footprintString(nil))
}
