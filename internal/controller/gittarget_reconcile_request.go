// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReconcileRequestAnnotation asks a GitTarget to be reconciled at once, and to re-read its Git
// folder rather than wait for the periodic pass. Set it to any value that changes — a timestamp
// is the convention — and the change is what triggers; the value itself carries no meaning.
//
// The spelling is Flux's, deliberately: `reconcile.fluxcd.io/requestedAt` is what this ecosystem
// already types, and `flux reconcile` is the muscle memory a user brings. Nothing else in this
// repository had a reconcile-request convention to be consistent with instead.
//
// It matters most for a folder someone else edits. The resolution only refreshes when this
// target scans, and a resolution that only refreshes on the periodic cadence is one you cannot iterate
// with: edit the folder, request a reconcile, read status.placement.
const ReconcileRequestAnnotation = "reconcile.configbutler.ai/requestedAt"

// reconcileRequestTracker remembers the last reconcile-request value acted on per object, so a
// standing annotation forces exactly one re-read rather than one on every reconcile after it.
//
// It is deliberately in MEMORY rather than in status. The alternative — a
// status.lastHandledReconcileAt echo — is more surface on an API this PR is otherwise only adding
// two things to, and the cost of not having it is bounded and small: after a controller restart a
// target carrying an old annotation is force-rechecked once. A re-check re-reads Git and writes
// nothing that was not going to be written anyway, so paying it once per restart is cheaper than
// a field every consumer then has to understand.
type reconcileRequestTracker struct {
	mu       sync.Mutex
	handled  map[string]string
	initOnce sync.Once
}

// take reports whether the object carries a reconcile request that has not been acted on yet, and
// records it as handled. An absent annotation is never a request.
func (t *reconcileRequestTracker) take(ref types.ResourceReference, requestedAt string) bool {
	if requestedAt == "" {
		return false
	}
	t.initOnce.Do(func() { t.handled = map[string]string{} })
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handled == nil {
		t.handled = map[string]string{}
	}
	if prior, had := t.handled[ref.Key()]; had && prior == requestedAt {
		return false
	}
	t.handled[ref.Key()] = requestedAt
	return true
}

// forget drops a deleted object's record so the map cannot grow without bound across the
// lifetime of the process.
func (t *reconcileRequestTracker) forget(ref types.ResourceReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.handled, ref.Key())
}

// reconcileRequestedOrSpecChanged is the For() predicate a reconcile request needs.
//
// GenerationChangedPredicate alone would filter the request out: an annotation edit does not bump
// metadata.generation, which is exactly why that predicate is safe against the controller's own
// status writes. So the request is admitted as an explicit exception — the annotation's VALUE
// changing — and everything else keeps the old behaviour.
func reconcileRequestedOrSpecChanged() predicate.Predicate {
	generation := predicate.GenerationChangedPredicate{}
	return predicate.Funcs{
		CreateFunc:  generation.Create,
		DeleteFunc:  generation.Delete,
		GenericFunc: generation.Generic,
		UpdateFunc: func(e event.UpdateEvent) bool {
			if generation.Update(e) {
				return true
			}
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetAnnotations()[ReconcileRequestAnnotation] !=
				e.ObjectNew.GetAnnotations()[ReconcileRequestAnnotation]
		},
	}
}

// reconcileRequestedAt reads the annotation off a GitTarget.
func reconcileRequestedAt(target *configbutleraiv1alpha3.GitTarget) string {
	return target.GetAnnotations()[ReconcileRequestAnnotation]
}
