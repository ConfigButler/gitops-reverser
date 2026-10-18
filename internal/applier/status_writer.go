// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// DefaultStatusRetryInterval is how soon a trigger status write that failed is tried again.
const DefaultStatusRetryInterval = 5 * time.Second

// maxStatusConflicts bounds how often one write re-reads a trigger after losing a race with its
// reconciler, which writes the conditions and the source of the same status.
const maxStatusConflicts = 5

// StatusWriter writes what the branch workers publish on their apply triggers: status.operation
// and status.repairOwed. It implements git.ApplyTriggerStatusWriter, and runs as a manager.Runnable
// beside the branch workers, on the leader as they are.
//
// A publication never blocks the worker. It is held until written, and a later publication for the
// same trigger replaces one not yet written, so the status catches up with the worker's latest view
// rather than replaying each step. A write that fails is tried again after RetryInterval, unless a
// newer publication replaced it in between.
//
// An operation an attempt starts reaches the trigger first through RecordApplyOperation, which the
// dispatcher calls just before the write that starts the apply, so the trigger never shows an
// operation that nothing could have applied. Writes are made one at a time, each of the latest
// publication, so a publication is never written over a later one.
//
// The status may hold an operation somebody else recorded: an earlier process, or the trigger's
// reconciler for an apply started outside GitOps Reverser. A publication replaces status.operation
// only on a status that holds none, or holds the same operation (sameOperation) at an earlier
// point, so it never writes over a record that is not the one it carries. An operation the worker
// withdraws, because its attempt was recorded and then asked for no apply, is replaced by what the
// status held before it was recorded.
//
// The rest of the status (the conditions, the source) belongs to the trigger's reconciler. Each
// write reads the trigger, sets these two fields and updates the status with the resourceVersion it
// read, so a status the reconciler wrote in between is never overwritten: the update conflicts and
// the write reads again.
type StatusWriter struct {
	reader client.Reader
	writer client.StatusClient
	log    logr.Logger

	// RetryInterval is how soon a failed write is tried again. Set before Start.
	RetryInterval time.Duration

	// writing is held for every write, and for the whole of a batch, so writes land in the order
	// their publications were made.
	writing sync.Mutex
	// recorded is the operation RecordApplyOperation last wrote on each trigger, with what the
	// status held before it. It is read and written under writing.
	recorded map[git.TriggerRef]recordedOperation

	mu      sync.Mutex
	pending map[git.TriggerRef]publication
	wake    chan struct{}
}

// publication is what is held for one trigger until it is written: the worker's latest status, and
// every operation withdrawn since the last write, which a later publication never takes back.
type publication struct {
	status    git.ApplyTriggerStatus
	withdrawn []*v1alpha3.ApplyOperation
}

// merged is newer, withdrawing what p withdrew as well.
func (p publication) merged(newer publication) publication {
	for _, operation := range p.withdrawn {
		if !holdsOperation(newer.withdrawn, operation) {
			newer.withdrawn = append(newer.withdrawn, operation)
		}
	}
	return newer
}

// recordedOperation is an operation RecordApplyOperation wrote, and the operation it replaced.
type recordedOperation struct {
	operation *v1alpha3.ApplyOperation
	displaced *v1alpha3.ApplyOperation
}

var (
	// errTriggerReleased is a write to a trigger that is gone, or being deleted.
	errTriggerReleased = errors.New("the trigger is gone or being deleted")
	// errOperationInFlight is a record refused because the trigger's status holds another operation
	// that has not completed.
	errOperationInFlight = errors.New("the trigger records another operation that has not completed")
)

var (
	_ git.ApplyTriggerStatusWriter   = (*StatusWriter)(nil)
	_ manager.Runnable               = (*StatusWriter)(nil)
	_ manager.LeaderElectionRunnable = (*StatusWriter)(nil)
)

// NewStatusWriter returns a StatusWriter that reads triggers through reader, uncached so a write
// does not lose its race to a lagging cache, and writes their status through writer.
func NewStatusWriter(reader client.Reader, writer client.StatusClient, log logr.Logger) *StatusWriter {
	return &StatusWriter{
		reader:        reader,
		writer:        writer,
		log:           log,
		RetryInterval: DefaultStatusRetryInterval,
		recorded:      map[git.TriggerRef]recordedOperation{},
		pending:       map[git.TriggerRef]publication{},
		wake:          make(chan struct{}, 1),
	}
}

// NeedLeaderElection is true: the branch workers that publish run only on the leader.
func (w *StatusWriter) NeedLeaderElection() bool { return true }

// PublishApplyTriggerStatus holds status for trigger until it is written, replacing any not yet
// written, but not what that one withdrew. It does not block.
func (w *StatusWriter) PublishApplyTriggerStatus(trigger git.TriggerRef, status git.ApplyTriggerStatus) {
	next := publication{status: status}
	if status.Operation != nil {
		next.status.Operation = stored(status.Operation)
	}
	if status.Withdrawn != nil {
		next.status.Withdrawn = stored(status.Withdrawn)
		next.withdrawn = []*v1alpha3.ApplyOperation{next.status.Withdrawn}
	}
	w.mu.Lock()
	w.pending[trigger] = w.pending[trigger].merged(next)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// stored is a copy of operation as the API server stores it: its times to the second, so a status
// that already holds it compares equal and is not written again.
func stored(operation *v1alpha3.ApplyOperation) *v1alpha3.ApplyOperation {
	operation = operation.DeepCopy()
	operation.RequestedAt = operation.RequestedAt.Rfc3339Copy()
	if operation.FinishedAt != nil {
		finished := operation.FinishedAt.Rfc3339Copy()
		operation.FinishedAt = &finished
	}
	return operation
}

// Start writes what is published until ctx ends.
func (w *StatusWriter) Start(ctx context.Context) error {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
		case <-retry:
		}
		retry = nil
		if w.writePending(ctx) {
			retry = time.After(w.RetryInterval)
		}
	}
}

// RecordApplyOperation writes operation on the trigger now, and returns once it is written. It
// first writes what is held for the trigger, so the operation it replaces is the worker's latest
// view of it. It fails, so that nothing starts the apply, when the trigger is gone or being
// deleted, and when its status holds another operation that has not completed: an apply started
// outside GitOps Reverser, which the trigger's reconciler recorded, or one an earlier process
// recorded, which the worker follows before it asks for anything.
func (w *StatusWriter) RecordApplyOperation(
	ctx context.Context, ref git.TriggerRef, operation *v1alpha3.ApplyOperation,
) error {
	w.writing.Lock()
	defer w.writing.Unlock()
	if err := w.writeHeld(ctx, ref); err != nil {
		return err
	}

	operation = stored(operation)
	for range maxStatusConflicts {
		trigger, err := w.read(ctx, ref)
		if err != nil {
			return err
		}
		current := trigger.Operation()
		switch {
		case current != nil && sameOperation(current, operation):
			return nil
		case current != nil && !current.Phase.Completed():
			return fmt.Errorf("record the operation on %s: %w (%s, requested %s)", ref, errOperationInFlight,
				current.Phase, current.RequestedAt.UTC().Format(time.RFC3339))
		}
		// Kept before the update: an update that reports an error may still have landed.
		w.recorded[ref] = recordedOperation{operation: operation, displaced: current.DeepCopy()}
		trigger.SetOperation(operation, trigger.RepairOwed())
		err = w.writer.Status().Update(ctx, trigger)
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return errTriggerReleased
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("record the operation on %s: %w", ref, err)
		}
	}
	return fmt.Errorf("record the operation on %s: it kept changing under the write", ref)
}

// writeHeld writes what is held for one trigger, if anything, and holds it again should the write
// fail. The caller holds writing.
func (w *StatusWriter) writeHeld(ctx context.Context, ref git.TriggerRef) error {
	w.mu.Lock()
	held, ok := w.pending[ref]
	delete(w.pending, ref)
	w.mu.Unlock()
	if !ok {
		return nil
	}
	err := w.write(ctx, ref, held)
	if err != nil && !errors.Is(err, errTriggerReleased) {
		w.holdAgain(ref, held)
	}
	return err
}

// writePending writes every publication held, and reports whether one failed and is held again.
func (w *StatusWriter) writePending(ctx context.Context) bool {
	w.writing.Lock()
	defer w.writing.Unlock()
	w.mu.Lock()
	batch := w.pending
	w.pending = map[git.TriggerRef]publication{}
	w.mu.Unlock()

	failed := false
	for trigger, held := range batch {
		err := w.write(ctx, trigger, held)
		if err == nil || errors.Is(err, errTriggerReleased) {
			continue
		}
		w.log.Info("could not write an apply trigger's status; trying again", "trigger", trigger.String(),
			"error", err.Error())
		if w.holdAgain(trigger, held) {
			failed = true
		}
	}
	return failed
}

// holdAgain holds a publication whose write failed, under any made since, and reports whether none
// was: only then is a retry due, because a newer publication wakes the writer itself.
func (w *StatusWriter) holdAgain(ref git.TriggerRef, failed publication) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	newer, ok := w.pending[ref]
	if !ok {
		w.pending[ref] = failed
		return true
	}
	w.pending[ref] = failed.merged(newer)
	return false
}

// write sets one trigger's status.operation, as operationToWrite says, and status.repairOwed. A
// trigger that is gone, or being deleted, takes the publication with it: errTriggerReleased.
func (w *StatusWriter) write(ctx context.Context, ref git.TriggerRef, held publication) error {
	for range maxStatusConflicts {
		trigger, err := w.read(ctx, ref)
		if err != nil {
			return err
		}
		operation := w.operationToWrite(ref, held, trigger.Operation())
		repairOwed := held.status.RepairOwed
		if trigger.RepairOwed() == repairOwed && equality.Semantic.DeepEqual(trigger.Operation(), operation) {
			return nil
		}
		trigger.SetOperation(operation, repairOwed)
		err = w.writer.Status().Update(ctx, trigger)
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return errTriggerReleased
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("write the status of %s: %w", ref, err)
		}
	}
	return fmt.Errorf("write the status of %s: it kept changing under the write", ref)
}

// read reads a trigger to write, and errTriggerReleased when it is gone or being deleted.
func (w *StatusWriter) read(ctx context.Context, ref git.TriggerRef) (v1alpha3.ApplyTrigger, error) {
	trigger := v1alpha3.NewApplyTrigger(ref.Kind)
	if trigger == nil {
		return nil, errTriggerReleased
	}
	err := w.reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, trigger)
	switch {
	case apierrors.IsNotFound(err):
		return nil, errTriggerReleased
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", ref, err)
	case !trigger.GetDeletionTimestamp().IsZero():
		return nil, errTriggerReleased
	}
	return trigger, nil
}

// operationToWrite is the operation a write of held leaves on a trigger whose status holds current.
// An operation held withdraws is replaced by what the status held before this writer recorded it,
// or by none. Then the published operation is written over none, or over the same operation at an
// earlier point; never over another, which somebody else recorded.
func (w *StatusWriter) operationToWrite(
	ref git.TriggerRef, held publication, current *v1alpha3.ApplyOperation,
) *v1alpha3.ApplyOperation {
	operation := current
	if current != nil && holdsOperation(held.withdrawn, current) {
		operation = nil
		if recorded, ok := w.recorded[ref]; ok && sameOperation(recorded.operation, current) {
			operation = recorded.displaced
		}
	}
	if published := held.status.Operation; published != nil &&
		(operation == nil || sameOperation(operation, published)) {
		operation = published
	}
	return operation
}

// sameOperation reports whether a and b are one operation, perhaps at different points: an
// operation keeps the token and requestedAt it was recorded with. The requestedAt tells apart the
// operations the trigger's reconciler records, which carry no token.
func sameOperation(a, b *v1alpha3.ApplyOperation) bool {
	return a.Token == b.Token && a.RequestedAt.Equal(&b.RequestedAt)
}

// holdsOperation reports whether operations holds operation, by sameOperation.
func holdsOperation(operations []*v1alpha3.ApplyOperation, operation *v1alpha3.ApplyOperation) bool {
	for _, held := range operations {
		if sameOperation(held, operation) {
			return true
		}
	}
	return false
}
