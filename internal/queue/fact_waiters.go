// SPDX-License-Identifier: Apache-2.0

package queue

import "sync"

// factWaiterKey is one candidate the join would match on: the scope every key leads with, which
// structure it belongs to, and the value within it. A waiter registers the keys its watch event
// could resolve through, and the goroutine applying a fact wakes the keys that fact filled.
type factWaiterKey struct {
	scope factScope
	kind  factKind
	value string
}

// factWaiter is one blocked resolver. The channel is buffered and signalled without blocking, so
// the applying goroutine is never slowed by a waiter that has not looked yet, and a waiter that was
// signalled while it was re-checking finds the signal still there.
type factWaiter struct {
	ch   chan struct{}
	keys []factWaiterKey
}

// factWaiterRegistry maps candidate keys to the resolvers blocked on them.
//
// Register-then-check is the whole reason it exists, and the order is not an implementation detail:
// a resolver registers its keys BEFORE reading the index, so a fact applied in the gap between the
// two signals a waiter that is already listening. Checking first and registering after loses
// exactly that fact, which is the race the poll loop used to paper over by looking again.
type factWaiterRegistry struct {
	mu    sync.Mutex
	byKey map[factWaiterKey]map[*factWaiter]struct{}
}

func newFactWaiterRegistry() *factWaiterRegistry {
	return &factWaiterRegistry{byKey: map[factWaiterKey]map[*factWaiter]struct{}{}}
}

// register adds a waiter for every candidate key and returns it armed. The caller must unregister
// it, whatever it goes on to resolve to.
func (r *factWaiterRegistry) register(keys []factWaiterKey) *factWaiter {
	waiter := &factWaiter{ch: make(chan struct{}, 1), keys: keys}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		waiters, ok := r.byKey[key]
		if !ok {
			waiters = map[*factWaiter]struct{}{}
			r.byKey[key] = waiters
		}
		waiters[waiter] = struct{}{}
	}
	return waiter
}

// unregister drops a waiter from every key it was registered under.
func (r *factWaiterRegistry) unregister(waiter *factWaiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range waiter.keys {
		waiters, ok := r.byKey[key]
		if !ok {
			continue
		}
		delete(waiters, waiter)
		if len(waiters) == 0 {
			delete(r.byKey, key)
		}
	}
}

// wake signals every waiter registered under any of the keys a freshly applied fact filled.
func (r *factWaiterRegistry) wake(keys []factWaiterKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		for waiter := range r.byKey[key] {
			select {
			case waiter.ch <- struct{}{}:
			default:
			}
		}
	}
}

// len reports how many candidate keys currently have a waiter on them. It exists so a test can
// prove a resolver leaves nothing registered behind.
func (r *factWaiterRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}
