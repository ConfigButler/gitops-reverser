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
// A publication can also be written through at once (PersistApplyTriggerStatus): the dispatcher
// does so before it starts an apply, so the operation is on the trigger before anything can apply.
// Writes are made one at a time, each of the latest publication, so a publication is never written
// over a later one.
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

	mu      sync.Mutex
	pending map[git.TriggerRef]git.ApplyTriggerStatus
	wake    chan struct{}
}

// errTriggerReleased is a write to a trigger that is gone, or being deleted.
var errTriggerReleased = errors.New("the trigger is gone or being deleted")

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
		pending:       map[git.TriggerRef]git.ApplyTriggerStatus{},
		wake:          make(chan struct{}, 1),
	}
}

// NeedLeaderElection is true: the branch workers that publish run only on the leader.
func (w *StatusWriter) NeedLeaderElection() bool { return true }

// PublishApplyTriggerStatus holds status for trigger until it is written, replacing any not yet
// written. It does not block.
func (w *StatusWriter) PublishApplyTriggerStatus(trigger git.TriggerRef, status git.ApplyTriggerStatus) {
	w.mu.Lock()
	if status.Operation != nil {
		status.Operation = stored(status.Operation)
	}
	w.pending[trigger] = status
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

// PersistApplyTriggerStatus writes the trigger's latest publication now, unless it was written
// already, and returns once it is. A trigger that is gone or being deleted is an error: nothing may
// start an apply for it.
func (w *StatusWriter) PersistApplyTriggerStatus(ctx context.Context, trigger git.TriggerRef) error {
	w.writing.Lock()
	defer w.writing.Unlock()
	w.mu.Lock()
	status, held := w.pending[trigger]
	delete(w.pending, trigger)
	w.mu.Unlock()
	if !held {
		return nil
	}
	err := w.write(ctx, trigger, status)
	if err != nil && !errors.Is(err, errTriggerReleased) {
		w.mu.Lock()
		if _, newer := w.pending[trigger]; !newer {
			w.pending[trigger] = status
		}
		w.mu.Unlock()
	}
	return err
}

// writePending writes every publication held, and reports whether one failed and is held again.
func (w *StatusWriter) writePending(ctx context.Context) bool {
	w.writing.Lock()
	defer w.writing.Unlock()
	w.mu.Lock()
	batch := w.pending
	w.pending = map[git.TriggerRef]git.ApplyTriggerStatus{}
	w.mu.Unlock()

	failed := false
	for trigger, status := range batch {
		err := w.write(ctx, trigger, status)
		if err == nil || errors.Is(err, errTriggerReleased) {
			continue
		}
		w.log.Info("could not write an apply trigger's status; trying again", "trigger", trigger.String(),
			"error", err.Error())
		w.mu.Lock()
		if _, newer := w.pending[trigger]; !newer {
			w.pending[trigger] = status
			failed = true
		}
		w.mu.Unlock()
	}
	return failed
}

// write sets one trigger's status.operation (unless the publication carries none, or an older
// operation than the status holds) and status.repairOwed. A trigger that is gone, or being deleted,
// takes the publication with it: errTriggerReleased.
func (w *StatusWriter) write(ctx context.Context, ref git.TriggerRef, status git.ApplyTriggerStatus) error {
	for range maxStatusConflicts {
		trigger, err := w.read(ctx, ref)
		if err != nil {
			return err
		}
		operation := operationToWrite(status.Operation, trigger.Operation())
		if trigger.RepairOwed() == status.RepairOwed && equality.Semantic.DeepEqual(trigger.Operation(), operation) {
			return nil
		}
		trigger.SetOperation(operation, status.RepairOwed)
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

// operationToWrite is the operation a write leaves on a trigger whose status holds current: the
// published one, unless it is none or one requested before current, which it never replaces.
func operationToWrite(published, current *v1alpha3.ApplyOperation) *v1alpha3.ApplyOperation {
	if published == nil || (current != nil && published.RequestedAt.Before(&current.RequestedAt)) {
		return current
	}
	return published
}
