// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// An apply window judges a write by when its event reaches the worker, not by when the write was
// made. A grace after the operation completed covers a watch that delivers within seconds. It does
// not cover one that is behind: every event waits for its audit fact in turn, and on a source cluster
// that sends none each waits the whole attribution grace, so the applier's writes can arrive long
// after the window closed. Mirrored then, a write that reverted an edit made during the apply put the
// revert in Git and lost the edit (docs/review/20261026-apply-window-late-events.md).
//
// So a window whose operation completed also waits for the watch. For each GitTarget the applier
// covers, the watch side lists every stream's collection and takes the newest resourceVersion among
// its objects: the position of the latest write to anything in it, the applier's writes included.
// Each stream answers once it has handled an event or a bookmark at or past that position. Its answer
// rides this worker's queue, behind every event the stream routed before it, so when every stream
// has answered, every write the applier made has reached the window. The window then closes at the
// end of its grace, or at once if that has passed.
//
// A write the applier deleted is not in the list, so its event can still arrive after the window: a
// DELETE is never withheld anyway, and the object it removes is one Git no longer holds. A barrier
// that cannot be taken (the source cannot be listed, or its resourceVersions are not numbers) leaves
// the grace alone, as before. A stream that stops, or never gets that far, holds the window for at
// most applyWindowBarrierLimit.

// applyWindowBarrierLimit bounds how long a window waits for the watch after its operation completed.
// Past it the window closes anyway, and the miss is logged and counted (barrierTimedOut).
const applyWindowBarrierLimit = 2 * time.Minute

// ApplyBarrierFunc asks the watch side to call deliver, once per running stream of target, when that
// stream has handled every write its collection held when the barrier was taken, and returns how many
// streams will. deliver must enqueue on the worker's queue, so it lands behind every event the stream
// routed before it. It is called off the worker's loop, and may list the source cluster.
type ApplyBarrierFunc func(ctx context.Context, target itypes.ResourceReference, deliver func()) (int, error)

// ApplyBarrier is one answer to a barrier an apply window took: a stream that got there (Passed), the
// number of streams that will answer (Registered), or the barrier failing (Err).
type ApplyBarrier struct {
	Applier ApplierRef
	Token   uint64

	Passed     bool
	Registered bool
	Expected   int
	Err        error
}

// applyBarrierWait is what a window waits for after its operation completed.
type applyBarrierWait struct {
	token       uint64
	completedAt time.Time
	// until bounds the wait: completedAt plus applyWindowBarrierLimit.
	until time.Time
	// expected is how many streams will answer, -1 until the registration arrived; passed is how many
	// have.
	expected int
	passed   int
}

// holds reports whether the wait still keeps its window open at now.
func (b *applyBarrierWait) holds(now time.Time) bool {
	return b != nil && now.Before(b.until)
}

// Apply-window barrier outcomes, the outcome label of apply_window_barriers_total.
const (
	barrierMet      = "met"
	barrierTimedOut = "timed_out"
	barrierFailed   = "failed"
)

// awaitApplyBarrier makes a window whose operation completed wait for the watch, when the worker has
// a barrier to take. The barrier is taken off the loop: listing a source cluster can take a while.
func (l *branchWorkerEventLoop) awaitApplyBarrier(state *applierState, now time.Time) {
	window := state.window
	w := l.w
	if w.applyBarrier == nil || window == nil || window.closesAt.IsZero() || len(state.targets) == 0 {
		return
	}
	l.barrierSeq++
	token := l.barrierSeq
	window.barrier = &applyBarrierWait{
		token: token, completedAt: now, until: now.Add(applyWindowBarrierLimit), expected: -1,
	}
	ref := state.ref
	targets := make([]itypes.ResourceReference, 0, len(state.targets))
	for _, target := range state.targets {
		targets = append(targets, target.ref)
	}
	base := w.ctx
	if base == nil {
		base = context.Background()
	}
	until := window.barrier.until
	go func() {
		// Taking the barrier lists the source clusters; past the window's limit nothing waits for it.
		ctx, cancel := context.WithDeadline(base, until)
		defer cancel()
		expected := 0
		for _, target := range targets {
			n, err := w.applyBarrier(ctx, target, func() {
				w.enqueueApplyBarrier(&ApplyBarrier{Applier: ref, Token: token, Passed: true})
			})
			if err != nil {
				w.enqueueApplyBarrier(&ApplyBarrier{Applier: ref, Token: token, Err: err})
				return
			}
			expected += n
		}
		w.enqueueApplyBarrier(&ApplyBarrier{Applier: ref, Token: token, Registered: true, Expected: expected})
	}()
}

// handleApplyBarrier counts one answer to a barrier the window waits for: its own, or one it carries
// from the window it replaced. An answer to a barrier nothing waits for any more (met, or timed out)
// is dropped.
func (l *branchWorkerEventLoop) handleApplyBarrier(answer *ApplyBarrier) {
	state := l.appliers[answer.Applier]
	if state == nil || state.window == nil {
		return
	}
	window := state.window
	wait := window.waitFor(answer.Token)
	if wait == nil {
		return
	}
	switch {
	case answer.Err != nil:
		window.dropWait(wait)
		l.w.Log.Info("Could not take the apply window's barrier; the window closes after its grace alone",
			"branch", l.w.Branch, "applier", state.ref.String(), "commit", window.commit, "err", answer.Err.Error())
		l.recordApplyBarrier(barrierFailed)
		return
	case answer.Registered:
		wait.expected = answer.Expected
	case answer.Passed:
		wait.passed++
	}
	if wait.expected < 0 || wait.passed < wait.expected {
		return
	}
	window.dropWait(wait)
	l.w.Log.V(1).Info("The watch delivered every write made before the apply completed",
		"branch", l.w.Branch, "applier", state.ref.String(), "commit", window.commit,
		"waited", time.Since(wait.completedAt).Round(time.Millisecond).String(), "streams", wait.expected)
	l.recordApplyBarrier(barrierMet)
}

// waitFor is the barrier with token that the window waits for, nil when none.
func (a *applyWindow) waitFor(token uint64) *applyBarrierWait {
	for _, wait := range a.pendingWaits() {
		if wait.token == token {
			return wait
		}
	}
	return nil
}

// dropWait stops the window waiting for a barrier.
func (a *applyWindow) dropWait(wait *applyBarrierWait) {
	if a.barrier == wait {
		a.barrier = nil
		return
	}
	a.carriedWaits = slices.DeleteFunc(a.carriedWaits, func(w *applyBarrierWait) bool { return w == wait })
}

// barrierTimedOut logs and counts the barriers a window still waited for when it closed.
func (l *branchWorkerEventLoop) barrierTimedOut(state *applierState) {
	window := state.window
	for _, wait := range window.pendingWaits() {
		window.dropWait(wait)
		l.w.Log.Info("The apply window closed before the watch delivered every write made before the apply "+
			"completed: a write of the applier's arriving later is mirrored as an edit",
			"branch", l.w.Branch, "applier", state.ref.String(), "commit", window.commit,
			"waited", time.Since(wait.completedAt).Round(time.Second).String(),
			"streams", wait.expected, "delivered", wait.passed)
		l.recordApplyBarrier(barrierTimedOut)
	}
}

// recordApplyBarrier counts one apply window barrier, by outcome.
func (l *branchWorkerEventLoop) recordApplyBarrier(outcome string) {
	if telemetry.ApplyWindowBarriersTotal == nil {
		return
	}
	telemetry.ApplyWindowBarriersTotal.Add(context.WithoutCancel(l.w.ctx), 1,
		metric.WithAttributes(l.w.gitBranchAttrs(attribute.String("outcome", outcome))...))
}

// enqueueApplyBarrier puts one barrier answer on the worker's queue. It never blocks: an answer the
// full queue drops leaves its window waiting until applyWindowBarrierLimit.
func (w *BranchWorker) enqueueApplyBarrier(answer *ApplyBarrier) {
	w.inflightItems.Add(1)
	w.pendingResyncsMu.Lock()
	defer w.pendingResyncsMu.Unlock()
	if w.stoppingLocked() {
		w.releaseInflightItem()
		return
	}
	select {
	case w.eventQueue <- WorkItem{ApplyBarrier: answer}:
	default:
		w.releaseInflightItem()
		w.Log.V(1).Info("Event queue full, apply window barrier answer dropped; the window closes at its limit",
			"branch", w.Branch, "applier", answer.Applier.String())
	}
}
