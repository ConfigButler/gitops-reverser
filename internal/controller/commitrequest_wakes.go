// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// commitRequestWakeFallback is how often a request the worker has registered is looked at when its
// steps are woken by the worker's changes: only a change that was dropped waits this long.
const commitRequestWakeFallback = 10 * time.Second

// commitRequestWakeBuffer bounds the reconciles a burst of changes can queue; a full buffer drops the
// arriving one, and the fallback poll takes its step.
const commitRequestWakeBuffer = 256

// CommitRequestWakes turns each change a branch worker reports about a CommitRequest into the
// request's next step, at once instead of at its next poll: a reconcile for a request on the control
// cluster, and a wake of the goroutine that serves a request on a source cluster. Nil wakes nothing,
// and the steps poll.
type CommitRequestWakes struct {
	changes <-chan git.CommitRequestChange
	events  chan event.GenericEvent

	mu sync.Mutex
	// served holds, by UID, the wake of every goroutine that serves a source-cluster request now. A
	// request can have several: each KubeProvider watching its namespace on that cluster looks at it,
	// and one that does not own it must not take the owner's wake away.
	served map[types.UID]map[chan struct{}]struct{}
}

// NewCommitRequestWakes reads changes; nil changes make wakes that wake nothing.
func NewCommitRequestWakes(changes <-chan git.CommitRequestChange) *CommitRequestWakes {
	return &CommitRequestWakes{
		changes: changes,
		events:  make(chan event.GenericEvent, commitRequestWakeBuffer),
		served:  map[types.UID]map[chan struct{}]struct{}{},
	}
}

// Start hands each change on until ctx ends.
func (w *CommitRequestWakes) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case change, ok := <-w.changes:
			if !ok {
				return nil
			}
			w.wake(change)
		}
	}
}

// wake queues a reconcile of the request on the control cluster, and wakes every goroutine serving
// it on a source cluster. A change names one request, on whichever cluster it lives, and
// a reconcile of a name the control cluster does not hold reads nothing and ends.
func (w *CommitRequestWakes) wake(change git.CommitRequestChange) {
	request := &configbutleraiv1alpha3.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: change.Namespace, Name: change.Name},
	}
	select {
	case w.events <- event.GenericEvent{Object: request}:
	default:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.served[types.UID(change.UID)] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Events carries the control-cluster requests to reconcile. Nil on nil wakes.
func (w *CommitRequestWakes) Events() <-chan event.GenericEvent {
	if w == nil {
		return nil
	}
	return w.events
}

// watch returns the wake of the request uid while a goroutine serves it, and the func that stops it.
// A change while the goroutine is mid-step is kept for its next wait. Nil wakes return a channel that
// never fires.
func (w *CommitRequestWakes) watch(uid types.UID) (<-chan struct{}, func()) {
	if w == nil || uid == "" {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.served[uid] == nil {
		w.served[uid] = map[chan struct{}]struct{}{}
	}
	w.served[uid][ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.served[uid], ch)
		if len(w.served[uid]) == 0 {
			delete(w.served, uid)
		}
		w.mu.Unlock()
	}
}

// fallback is how often a step looks at a request its worker registered: the slow fallback when
// the worker's changes wake the steps, zero (the plain poll) when nothing does.
func (w *CommitRequestWakes) fallback() time.Duration {
	if w == nil || w.changes == nil {
		return 0
	}
	return commitRequestWakeFallback
}
