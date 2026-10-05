// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// These tests pin the write gates every CommitRequest outcome passes (write_gate.go): a request
// that ends on a target that refuses writes fails with the reason, whatever path it ends on and
// whatever whenNothingToCommit says. They drive the transitions — a gate that closes after the
// attach, after the local commit, on a sibling target, across a failed push — and not only the
// steady states.

// suspendTarget sets spec.suspend on the request's GitTarget, crTarget, in the worker's client.
func suspendTarget(t *testing.T, worker *BranchWorker) {
	t.Helper()
	var target configv1alpha3.GitTarget
	key := k8stypes.NamespacedName{Name: crTarget, Namespace: "default"}
	require.NoError(t, worker.Client.Get(worker.ctx, key, &target))
	target.Spec.Suspend = true
	require.NoError(t, worker.Client.Update(worker.ctx, &target))
}

// closeRenderGate registers crTarget's configmaps scope with a fresh gate, which holds it Unknown —
// writes closed — until a result arrives. Any other target stays unregistered, and so writable.
func closeRenderGate(worker *BranchWorker) {
	gate := NewRenderFidelityGate()
	restartAll(gate, types.NewResourceReference(crTarget, "default"), fidelityScope("", "configmaps"))
	worker.renderFidelityGate = gate
}

// requestFor is a request named name, for target, by alice.
func requestFor(name, target string, commitEmpty bool) *AttachCommitRequest {
	req := attachReq("alice", time.Hour)
	req.Name, req.UID = name, "uid-"+name
	req.GitTargetName = target
	req.Message = "save " + name
	req.CommitEmpty = commitEmpty
	return req
}

// expire runs out every waiting request's attach deadline and services the loop.
func expire(loop *branchWorkerEventLoop) {
	past := time.Now().Add(-time.Millisecond)
	for _, pcr := range loop.pendingCRs {
		if !pcr.attached {
			pcr.attachDeadline = past
		}
	}
	loop.endWake(0)
}

// remoteHead is the remote's main.
func remoteHead(t *testing.T, serverRepo *gogit.Repository) plumbing.Hash {
	t.Helper()
	ref, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	return ref.Hash()
}

// seededLoop is a worker serving team-a whose branch already holds one pushed commit, so an empty
// commit has a parent and the push cooldown is running.
func seededLoop(t *testing.T) (*BranchWorker, *gogit.Repository, *branchWorkerEventLoop) {
	t.Helper()
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	t.Cleanup(loop.stopTimers)
	writeTo(loop, "existing")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()
	require.Empty(t, loop.pendingWrites)
	return worker, serverRepo, loop
}

// failPushes makes every push fail on a transport error, until the returned restore runs.
func failPushes(t *testing.T, worker *BranchWorker) func() {
	t.Helper()
	originalPush, originalFetch := pushAtomicFn, fetchRemoteBranchHashFn
	pushAtomicFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.Hash, _ plumbing.ReferenceName, _ []gitclient.Option,
	) (PushOutcome, error) {
		return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
	}
	fetchRemoteBranchHashFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ []gitclient.Option,
	) (plumbing.Hash, error) {
		return worker.pushCycleRootHash, nil // unmoved: not contention, so no replay
	}
	restore := func() { pushAtomicFn, fetchRemoteBranchHashFn = originalPush, originalFetch }
	t.Cleanup(restore)
	return restore
}

// TestWriteGate_ARequestOnARefusingTargetFails is the whole matrix of a request that ends on a target
// that refuses writes: each gate, each way a request can end, each whenNothingToCommit. Every cell
// fails with the gate's error and leaves the remote and the retained writes untouched. Before
// write_gate.go, three cells reported Ready=True: a suspended target's waiting save resolved
// NoWindow, an attached save on one resolved AlreadyPresent, and a waiting save whose writes the
// render-fidelity gate dropped resolved NoWindow — and, with CommitEmpty, recorded an empty commit
// saying no writes were seen.
func TestWriteGate_ARequestOnARefusingTargetFails(t *testing.T) {
	gates := []struct {
		name  string
		close func(t *testing.T, worker *BranchWorker)
		want  error
	}{
		{"suspended", func(t *testing.T, w *BranchWorker) { suspendTarget(t, w) }, errTargetSuspended},
		{"render fidelity", func(_ *testing.T, w *BranchWorker) { closeRenderGate(w) }, errRenderFidelityClosed},
	}
	endings := []struct {
		name string
		// run closes the gate at the point this ending puts it, and ends the request.
		run func(t *testing.T, loop *branchWorkerEventLoop, closeGate func(), req *AttachCommitRequest)
	}{
		{"no window before the attach timeout", func(_ *testing.T, loop *branchWorkerEventLoop, closeGate func(),
			req *AttachCommitRequest) {
			closeGate()
			serviceAttach(loop, req)
			expire(loop)
			loop.pushPending()
		}},
		{"attached, the gate closing before the window closes", func(t *testing.T, loop *branchWorkerEventLoop,
			closeGate func(), req *AttachCommitRequest) {
			serviceAttach(loop, req)
			writeTo(loop, "new")
			require.NotNil(t, loop.openWindow, "the request is attached to an open window")
			closeGate()
			forceDueNamed(loop, req)
			loop.pushPending()
		}},
		{"attached to a window that changed nothing", func(t *testing.T, loop *branchWorkerEventLoop,
			closeGate func(), req *AttachCommitRequest) {
			writeTo(loop, "existing")
			serviceAttach(loop, req)
			require.NotNil(t, loop.openWindow)
			closeGate()
			forceDueNamed(loop, req)
			loop.pushPending()
		}},
	}

	for _, gate := range gates {
		for _, ending := range endings {
			for _, commitEmpty := range []bool{false, true} {
				name := gate.name + "/" + ending.name + "/resolve"
				if commitEmpty {
					name = gate.name + "/" + ending.name + "/commit-empty"
				}
				t.Run(name, func(t *testing.T) {
					worker, serverRepo, loop := seededLoop(t)
					before := remoteHead(t, serverRepo)
					req := requestFor("save", "team-a", commitEmpty)

					ending.run(t, loop, func() { gate.close(t, worker) }, req)

					res, ok := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
					require.True(t, ok, "the request is resolved")
					require.ErrorIs(t, res.Err, gate.want, "a refused save is a failure, never Ready=True")
					assert.Empty(t, res.Commit)
					assert.Equal(t, before, remoteHead(t, serverRepo), "nothing reached the remote")
					assert.Empty(t, loop.pendingWrites, "nothing is retained for the request")
				})
			}
		}
	}
}

// forceDueNamed closes req's attached window at its maxDuration, as forceDue does for crName.
func forceDueNamed(loop *branchWorkerEventLoop, req *AttachCommitRequest) {
	id := req.id()
	if loop.openWindow != nil && loop.openWindow.pendingCR != nil && *loop.openWindow.pendingCR == id {
		loop.openWindow.timers.maxAt = time.Now().Add(-time.Millisecond)
		loop.closeOrArmWindow()
		loop.endWake(0)
	}
}

// TestWriteGate_ASaveWhoseWritesTheRenderGateDroppedSaysSo is the second #404 finding as it was
// found: the gate drops the save's writes on arrival, so no window ever opens, and a CommitEmpty save
// recorded "no writes were seen" over writes that were made and thrown away.
func TestWriteGate_ASaveWhoseWritesTheRenderGateDroppedSaysSo(t *testing.T) {
	worker, serverRepo, loop := seededLoop(t)
	before := remoteHead(t, serverRepo)
	closeRenderGate(worker)
	req := requestFor("save", "team-a", true)

	serviceAttach(loop, req)
	writeTo(loop, "dropped")
	require.Nil(t, loop.openWindow, "the gate dropped the write, so no window opened")
	expire(loop)
	loop.pushPending()

	res, ok := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
	require.True(t, ok)
	require.ErrorIs(t, res.Err, errRenderFidelityClosed)
	assert.Equal(t, before, remoteHead(t, serverRepo), "no empty commit claims the save saw nothing")
}

// TestWriteGate_AGateClosingAfterTheLocalCommitDoesNotRetractIt pins the cutover. The gates are read
// when the commit is made; a commit already made is pushed, and its request resolves with it, even
// if the push first fails and the gate closes while it is retried. Reading the gate at push time
// would strand a local commit in the worker's checkout, to resurface out of order later.
func TestWriteGate_AGateClosingAfterTheLocalCommitDoesNotRetractIt(t *testing.T) {
	gates := []struct {
		name  string
		close func(t *testing.T, worker *BranchWorker)
	}{
		{"suspended", func(t *testing.T, w *BranchWorker) { suspendTarget(t, w) }},
		{"render fidelity", func(_ *testing.T, w *BranchWorker) { closeRenderGate(w) }},
	}
	commits := []struct {
		name    string
		commit  func(loop *branchWorkerEventLoop, req *AttachCommitRequest)
		outcome FinalizeOutcome
	}{
		{"a window that changed files", func(loop *branchWorkerEventLoop, req *AttachCommitRequest) {
			serviceAttach(loop, req)
			writeTo(loop, "new")
			forceDueNamed(loop, req)
		}, FinalizeCommitted},
		{"the record of a request no window reached", func(loop *branchWorkerEventLoop, req *AttachCommitRequest) {
			serviceAttach(loop, req)
			expire(loop)
		}, FinalizeNoOpenWindow},
	}

	for _, gate := range gates {
		for _, c := range commits {
			t.Run(gate.name+"/"+c.name, func(t *testing.T) {
				worker, serverRepo, loop := seededLoop(t)
				req := requestFor("save", "team-a", true)
				restore := failPushes(t, worker)

				c.commit(loop, req)
				require.Len(t, loop.pendingWrites, 1, "the request rides one local commit")
				loop.pushPending()
				require.Len(t, loop.pendingWrites, 1, "a failed push retains it")

				gate.close(t, worker)
				loop.pushPending()
				_, resolved := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
				require.False(t, resolved, "the gate closing does not resolve a request the push still owes")

				restore()
				loop.pushPending()
				res, ok := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
				require.True(t, ok)
				require.NoError(t, res.Err)
				assert.Equal(t, c.outcome, res.Outcome)
				assert.Equal(
					t,
					remoteHead(t, serverRepo).String(),
					res.Commit,
					"the commit made before the gate closed",
				)
			})
		}
	}
}

// TestWriteGate_SiblingTargetsAreGatedApart pins that the gates are per GitTarget, not per worker:
// two targets share a branch, one refuses writes, and the other's save still records its message.
func TestWriteGate_SiblingTargetsAreGatedApart(t *testing.T) {
	gates := []struct {
		name  string
		close func(t *testing.T, worker *BranchWorker)
		want  error
	}{
		{"suspended", func(t *testing.T, w *BranchWorker) { suspendTarget(t, w) }, errTargetSuspended},
		{"render fidelity", func(_ *testing.T, w *BranchWorker) { closeRenderGate(w) }, errRenderFidelityClosed},
	}
	for _, gate := range gates {
		t.Run(gate.name, func(t *testing.T) {
			worker, serverRepo, loop := seededLoop(t)
			createPlainGitTarget(t, worker, "team-b", "team-b")
			gate.close(t, worker)

			refused := requestFor("save-a", "team-a", true)
			served := requestFor("save-b", "team-b", true)
			serviceAttach(loop, refused)
			serviceAttach(loop, served)
			expire(loop)
			loop.pushPending()

			res, ok := worker.LookupCommitRequestOutcome("default", refused.Name, refused.UID)
			require.True(t, ok)
			require.ErrorIs(t, res.Err, gate.want)

			res, ok = worker.LookupCommitRequestOutcome("default", served.Name, served.UID)
			require.True(t, ok)
			require.NoError(t, res.Err, "a sibling's gate is not this target's")
			assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
			requirePushedEmptyCommit(t, serverRepo, res, "save save-b")
		})
	}
}

// TestWriteGate_AWindowMismatchIsStillAMismatch pins the one ending the gates leave alone: a request
// that only saw another author's window was refused by that window, and says so, open gate or not.
func TestWriteGate_AWindowMismatchIsStillAMismatch(t *testing.T) {
	worker, _, loop := seededLoop(t)
	suspendTarget(t, worker)
	req := requestFor("save", "team-a", true)
	req.Author = "bob"

	serviceAttach(loop, req)
	writeTo(loop, "alices-edit")
	expire(loop)

	res, ok := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeWindowMismatch, res.Outcome)
}

// TestWriteGate_ARenderGateClosingDuringFinalizeStopsTheWholeWindow is the race the first look
// cannot see: the gate is open when the finalize starts and closes while it resolves the target's
// metadata. The request and the write must agree, so the window is dropped as a whole: the request
// fails, and nothing is committed, retained or pushed. Before, the request failed while the write
// it rode still committed, carrying the request's message.
func TestWriteGate_ARenderGateClosingDuringFinalizeStopsTheWholeWindow(t *testing.T) {
	for _, withRequest := range []bool{false, true} {
		name := "a live window"
		if withRequest {
			name = "a window a request is attached to"
		}
		t.Run(name, func(t *testing.T) {
			worker, serverRepo, loop := seededLoop(t)
			before := remoteHead(t, serverRepo)
			req := requestFor("save", crTarget, false)
			if withRequest {
				serviceAttach(loop, req)
			}
			writeTo(loop, "new")
			require.NotNil(t, loop.openWindow)
			require.True(t, worker.normalWritesAllowed(crTarget, "default"), "the first look finds the gate open")

			// Close the gate from inside the finalize, at the GitTarget read that resolves its metadata.
			inner, ok := worker.Client.(client.WithWatch)
			require.True(t, ok)
			worker.Client = interceptor.NewClient(inner, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if _, isTarget := obj.(*configv1alpha3.GitTarget); isTarget && worker.renderFidelityGate == nil {
						closeRenderGate(worker)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})

			if withRequest {
				forceDueNamed(loop, req)
			} else {
				require.False(t, loop.finalizeOpenWindow())
			}
			loop.pushPending()

			assert.Nil(t, loop.openWindow)
			assert.Empty(t, loop.pendingWrites, "nothing is retained")
			assert.Equal(t, before, remoteHead(t, serverRepo), "nothing reached the remote")
			if withRequest {
				res, ok := worker.LookupCommitRequestOutcome("default", req.Name, req.UID)
				require.True(t, ok)
				require.ErrorIs(t, res.Err, errRenderFidelityClosed)
			}
		})
	}
}
