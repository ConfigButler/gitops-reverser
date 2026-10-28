// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A save's outcome is decided by its branch worker and lives in the worker's memory until a step of
// the request reads it and writes it to the request's status. A clean shutdown is where those two
// part: the workers close their windows, push and settle every save they hold as they stop, but the
// CommitRequest controllers stop at the same moment, so nothing reads those outcomes, and the process
// exits with them. The next process sends the request's attach again, and its new worker, which never
// saw it, ends the save NoWindow, or attaches its message to a later window.
//
// So the shutdown writes them itself. Every request a path attached is remembered here with that
// path, and once every worker has stopped (git.WorkerManager.SetAfterWorkersStopped), Settle writes
// the outcome each worker decided through the request's own path: the same decide a step makes,
// conditional on the resourceVersion, re-applying the outcome on a conflict. A request no worker
// decided anything for is left as it is: the next process attaches it again, from its status.
//
// Nothing is persisted. A crash, or a shutdown that runs out of time, behaves as before.

// CommitRequestsInFlight remembers every CommitRequest a path of this process attached to a branch
// worker and has not finished, with that path, for Settle. Nil remembers nothing.
type CommitRequestsInFlight struct {
	mu       sync.Mutex
	requests map[inFlightKey]inFlightRequest
	// Log reports what Settle wrote and what it left.
	Log logr.Logger
}

// inFlightKey is one request on one path: the KubeProvider whose watch serves it, empty on the control
// cluster, and its name there.
type inFlightKey struct {
	provider types.NamespacedName
	request  types.NamespacedName
}

// inFlightRequest is how to take a step of a remembered request.
type inFlightRequest struct {
	core *commitRequestCore
	path commitRequestPath
}

// track remembers a request p attaches.
func (f *CommitRequestsInFlight) track(c *commitRequestCore, p commitRequestPath, key types.NamespacedName) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests == nil {
		f.requests = map[inFlightKey]inFlightRequest{}
	}
	f.requests[inFlightKey{provider: p.provider, request: key}] = inFlightRequest{core: c, path: p}
}

// forget drops a request that finished, or is gone.
func (f *CommitRequestsInFlight) forget(p commitRequestPath, key types.NamespacedName) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.requests, inFlightKey{provider: p.provider, request: key})
}

// remembered copies the requests out, for Settle to step through without the lock.
func (f *CommitRequestsInFlight) remembered() map[inFlightKey]inFlightRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[inFlightKey]inFlightRequest, len(f.requests))
	for key, request := range f.requests {
		out[key] = request
	}
	return out
}

// Settle writes, on ctx, the outcome each remembered request's worker decided, commitRequestConcurrency
// at a time. It runs at shutdown once every worker has stopped, so no worker decides anything more.
// What ctx leaves no time for is left for the next process, as a request with no outcome is.
func (f *CommitRequestsInFlight) Settle(ctx context.Context) {
	if f == nil {
		return
	}
	requests := f.remembered()
	if len(requests) == 0 {
		return
	}
	f.Log.Info("Writing the outcomes of the CommitRequests in flight at shutdown", "requests", len(requests))
	slots := make(chan struct{}, commitRequestConcurrency)
	var wg sync.WaitGroup
	for key, request := range requests {
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case slots <- struct{}{}:
			wg.Go(func() {
				defer func() { <-slots }()
				if err := request.core.settle(ctx, request.path, key.request); err != nil {
					request.path.log.Error(err, "Could not write a CommitRequest's outcome at shutdown; "+
						"the next process attaches it again", "name", key.request)
				}
			})
		}
	}
	wg.Wait()
	if left := f.remembered(); len(left) > 0 {
		names := make([]string, 0, len(left))
		for key := range left {
			names = append(names, key.request.String())
		}
		f.Log.Info("CommitRequests left without an outcome at shutdown; the next process attaches them again",
			"requests", names)
	}
}

// settle is a request's last step at shutdown: it writes the outcome the worker that held it decided,
// as drive would, and nothing else. It sends no attach and no withdraw, and records no phase: no worker
// runs any more, and a request no worker decided keeps its status for the next process to resume.
func (c *commitRequestCore) settle(ctx context.Context, p commitRequestPath, key types.NamespacedName) error {
	cr, err := p.store.get(ctx, key)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			c.inFlight.forget(p, key)
		}
		return client.IgnoreNotFound(err)
	}
	if !cr.DeletionTimestamp.IsZero() || !commitRequestClaimed(cr) {
		c.inFlight.forget(p, key)
		return nil
	}
	result, resolved, err := c.finalizer.LookupCommitRequestOutcome(ctx, c.attachFor(cr))
	if err != nil || !resolved {
		return err
	}
	p.log.Info("CommitRequest settled at shutdown", "name", key, "outcome", string(result.Outcome),
		"commit", result.Commit, "err", result.Err)
	_, err = c.decide(ctx, p, cr, commitRequestOutcome{result: result, err: result.Err}, true)
	return err
}
