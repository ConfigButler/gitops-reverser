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

// DefaultStatusRetryInterval is how soon an applier claim status write that failed is tried again.
const DefaultStatusRetryInterval = 5 * time.Second

// maxStatusConflicts bounds how often one write re-reads an applier claim after losing a race with its
// reconciler, which writes the conditions and the source of the same status.
const maxStatusConflicts = 5

// StatusWriter writes what the branch workers publish on their applier claims: status.operation
// and status.repairPending. It implements git.ApplierClaimStatusWriter, and runs as a manager.Runnable
// beside the branch workers, on the leader as they are.
//
// A publication never blocks the worker. It is held until written, and a later publication for the
// same applier claim replaces one not yet written, so the status catches up with the worker's latest view
// rather than replaying each step. A write that fails is tried again after RetryInterval, unless a
// newer publication replaced it in between.
//
// An operation an attempt starts reaches the applier claim first through RecordApplyOperation, which the
// dispatcher calls just before the write that starts the apply, so the applier claim never shows an
// operation that nothing could have applied. Writes are made one at a time, each of the latest
// publication, so a publication is never written over a later one.
//
// The status may hold an operation somebody else recorded: an earlier process, or the applier claim's
// reconciler for an apply started outside GitOps Reverser. A publication replaces status.operation
// only on a status that holds none, or holds the same operation (sameOperation) at an earlier
// point, so it never writes over a record that is not the one it carries. An operation the worker
// withdraws, because its attempt was recorded and then asked for no apply, is replaced by what the
// status held before it was recorded.
//
// The rest of the status (the conditions, the source) belongs to the applier claim's reconciler. Each
// write reads the applier claim, sets these two fields and updates the status with the resourceVersion it
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
	// recorded is the operation RecordApplyOperation last wrote on each applier claim, with what the
	// status held before it. It is read and written under writing.
	recorded map[git.ApplierClaimRef]recordedOperation

	mu      sync.Mutex
	pending map[git.ApplierClaimRef]publication
	wake    chan struct{}
}

// publication is what is held for one applier claim until it is written: the worker's latest status, and
// every operation withdrawn since the last write, which a later publication never takes back.
type publication struct {
	status    git.ApplierClaimStatus
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
	// errClaimReleased is a write to an applier claim that is gone, or being deleted.
	errClaimReleased = errors.New("the applier claim is gone or being deleted")
	// errOperationInFlight is a record refused because the applier claim's status holds another operation
	// that has not completed.
	errOperationInFlight = errors.New("the applier claim records another operation that has not completed")
)

var (
	_ git.ApplierClaimStatusWriter   = (*StatusWriter)(nil)
	_ manager.Runnable               = (*StatusWriter)(nil)
	_ manager.LeaderElectionRunnable = (*StatusWriter)(nil)
)

// NewStatusWriter returns a StatusWriter that reads applier claims through reader, uncached so a write
// does not lose its race to a lagging cache, and writes their status through writer.
func NewStatusWriter(reader client.Reader, writer client.StatusClient, log logr.Logger) *StatusWriter {
	return &StatusWriter{
		reader:        reader,
		writer:        writer,
		log:           log,
		RetryInterval: DefaultStatusRetryInterval,
		recorded:      map[git.ApplierClaimRef]recordedOperation{},
		pending:       map[git.ApplierClaimRef]publication{},
		wake:          make(chan struct{}, 1),
	}
}

// NeedLeaderElection is true: the branch workers that publish run only on the leader.
func (w *StatusWriter) NeedLeaderElection() bool { return true }

// PublishApplierClaimStatus holds status for claim until it is written, replacing any not yet
// written, but not what that one withdrew. It does not block.
func (w *StatusWriter) PublishApplierClaimStatus(claim git.ApplierClaimRef, status git.ApplierClaimStatus) {
	next := publication{status: status}
	if status.Operation != nil {
		next.status.Operation = stored(status.Operation)
	}
	if status.Withdrawn != nil {
		next.withdrawn = []*v1alpha3.ApplyOperation{stored(status.Withdrawn)}
	}
	w.mu.Lock()
	w.pending[claim] = w.pending[claim].merged(next)
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

// RecordApplyOperation writes operation on the applier claim now, and returns once it is written. It
// first writes what is held for the applier claim, so the operation it replaces is the worker's latest
// view of it. It fails, so that nothing starts the apply, when the applier claim is gone or being
// deleted, and when its status holds another operation that has not completed: an apply started
// outside GitOps Reverser, which the applier claim's reconciler recorded, or one an earlier process
// recorded, which the worker follows before it asks for anything.
func (w *StatusWriter) RecordApplyOperation(
	ctx context.Context, ref git.ApplierClaimRef, operation *v1alpha3.ApplyOperation,
) error {
	w.writing.Lock()
	defer w.writing.Unlock()
	if err := w.writeHeld(ctx, ref); err != nil {
		return err
	}

	operation = stored(operation)
	for range maxStatusConflicts {
		claim, err := w.read(ctx, ref)
		if err != nil {
			return err
		}
		current := claim.Operation()
		switch {
		case current != nil && sameOperation(current, operation):
			return nil
		case current != nil && !current.Phase.Completed():
			return fmt.Errorf("record the operation on %s: %w (%s, requested %s)", ref, errOperationInFlight,
				current.Phase, current.RequestedAt.UTC().Format(time.RFC3339))
		}
		// Kept before the update: an update that reports an error may still have landed.
		w.recorded[ref] = recordedOperation{operation: operation, displaced: current.DeepCopy()}
		claim.SetOperation(operation, claim.RepairPending())
		err = w.writer.Status().Update(ctx, claim)
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return errClaimReleased
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("record the operation on %s: %w", ref, err)
		}
	}
	return fmt.Errorf("record the operation on %s: it kept changing under the write", ref)
}

// writeHeld writes what is held for one applier claim, if anything, and holds it again should the write
// fail. The caller holds writing.
func (w *StatusWriter) writeHeld(ctx context.Context, ref git.ApplierClaimRef) error {
	w.mu.Lock()
	held, ok := w.pending[ref]
	delete(w.pending, ref)
	w.mu.Unlock()
	if !ok {
		return nil
	}
	err := w.write(ctx, ref, held)
	if err != nil && !errors.Is(err, errClaimReleased) {
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
	w.pending = map[git.ApplierClaimRef]publication{}
	w.mu.Unlock()

	failed := false
	for claim, held := range batch {
		err := w.write(ctx, claim, held)
		if err == nil || errors.Is(err, errClaimReleased) {
			continue
		}
		w.log.Info("could not write an applier claim's status; trying again", "applier claim", claim.String(),
			"error", err.Error())
		if w.holdAgain(claim, held) {
			failed = true
		}
	}
	return failed
}

// holdAgain holds a publication whose write failed, under any made since, and reports whether none
// was: only then is a retry due, because a newer publication wakes the writer itself.
func (w *StatusWriter) holdAgain(ref git.ApplierClaimRef, failed publication) bool {
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

// write sets one applier claim's status.operation, as operationToWrite says, and status.repairPending. An
// applier claim that is gone, or being deleted, takes the publication with it: errClaimReleased.
func (w *StatusWriter) write(ctx context.Context, ref git.ApplierClaimRef, held publication) error {
	for range maxStatusConflicts {
		claim, err := w.read(ctx, ref)
		if err != nil {
			return err
		}
		operation := w.operationToWrite(ref, held, claim.Operation())
		repairPending := held.status.RepairPending
		if claim.RepairPending() == repairPending && equality.Semantic.DeepEqual(claim.Operation(), operation) {
			return nil
		}
		claim.SetOperation(operation, repairPending)
		err = w.writer.Status().Update(ctx, claim)
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return errClaimReleased
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("write the status of %s: %w", ref, err)
		}
	}
	return fmt.Errorf("write the status of %s: it kept changing under the write", ref)
}

// read reads an applier claim to write, and errClaimReleased when it is gone or being deleted.
func (w *StatusWriter) read(ctx context.Context, ref git.ApplierClaimRef) (v1alpha3.ApplierClaim, error) {
	claim := v1alpha3.NewApplierClaim(ref.Kind)
	if claim == nil {
		return nil, errClaimReleased
	}
	err := w.reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, claim)
	switch {
	case apierrors.IsNotFound(err):
		return nil, errClaimReleased
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", ref, err)
	case !claim.GetDeletionTimestamp().IsZero():
		return nil, errClaimReleased
	}
	return claim, nil
}

// operationToWrite is the operation a write of held leaves on an applier claim whose status holds current.
// An operation held withdraws is replaced by what the status held before this writer recorded it,
// or by none. Then the published operation is written over none, or over the same operation at an
// earlier point; never over another, which somebody else recorded, and never in flight over the
// same one completed: a worker that recovered an external resume publishes it as it was listed,
// and the applier claim's reconciler can have completed it since.
func (w *StatusWriter) operationToWrite(
	ref git.ApplierClaimRef, held publication, current *v1alpha3.ApplyOperation,
) *v1alpha3.ApplyOperation {
	operation := current
	if current != nil && holdsOperation(held.withdrawn, current) {
		operation = nil
		if recorded, ok := w.recorded[ref]; ok && sameOperation(recorded.operation, current) {
			operation = recorded.displaced
		}
	}
	published := held.status.Operation
	switch {
	case published == nil:
	case operation == nil:
		operation = published
	case sameOperation(operation, published) && (!operation.Phase.Completed() || published.Phase.Completed()):
		operation = published
	}
	return operation
}

// sameOperation reports whether a and b are one operation, perhaps at different points: an
// operation keeps the token and requestedAt it was recorded with. The requestedAt tells apart the
// operations the applier claim's reconciler records, which carry no token.
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
