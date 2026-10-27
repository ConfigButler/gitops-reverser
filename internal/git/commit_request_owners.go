// SPDX-License-Identifier: Apache-2.0

package git

import "sync"

// commitRequestOwners records which branch worker accepted each CommitRequest's attach, so the
// controller's questions about a request reach the worker that can still act on it.
//
// The GitTarget cannot answer that. A worker serves every GitTarget on its (provider, branch), so it
// outlives a deleted target, and a retired worker is detached from the manager before it stops,
// so its last push runs while the manager no longer lists it. Asking "which worker does the target
// name now?" in either case finds none, and reading that as "nothing holds the request" is how a
// request was reported withdrawn while its worker could still publish it.
//
// An entry is claimed when an attach enters a worker's queue, and released when that worker
// forgets the request: when its outcome is collected, or when the worker exits without having
// acted on it. A request with no entry is therefore held by no worker, and only then may the
// controller fall back to the GitTarget.
//
// It is shared by the manager and every worker it creates. The lock is a leaf: callers may hold
// a worker's pendingResyncsMu or crOutcomesMu when they take it, never the other way round.
type commitRequestOwners struct {
	mu     sync.Mutex
	owners map[commitRequestID]*BranchWorker
}

// owner returns the worker holding the request, if any. A nil table (a worker built outside a
// manager) knows no owners.
func (o *commitRequestOwners) owner(id commitRequestID) (*BranchWorker, bool) {
	if o == nil {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.owners[id]
	return w, ok
}

// claim makes w the request's owner, unless another worker already is. It reports whether w owns
// the request afterwards. A nil table lets every worker claim.
func (o *commitRequestOwners) claim(id commitRequestID, w *BranchWorker) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if current, ok := o.owners[id]; ok {
		return current == w
	}
	if o.owners == nil {
		o.owners = map[commitRequestID]*BranchWorker{}
	}
	o.owners[id] = w
	return true
}

// release forgets the request, if w is still its owner.
func (o *commitRequestOwners) release(id commitRequestID, w *BranchWorker) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.owners[id] == w {
		delete(o.owners, id)
	}
}
