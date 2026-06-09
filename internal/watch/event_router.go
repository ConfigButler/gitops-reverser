/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// finalizeSignalTimeout bounds how long FinalizeGitTargetWindow waits for a
// branch worker to process a finalize signal before giving up. The signal
// rides the worker's event queue, so a healthy worker replies promptly.
const finalizeSignalTimeout = 30 * time.Second

// resyncSignalTimeout bounds how long a resync waits for the worker to apply and
// commit the snapshot. It is generous because the first resync can clone/pull the
// repository before committing; the reconcile context cancels sooner if it must.
const resyncSignalTimeout = 5 * time.Minute

// EventRouter orchestrates control flow between components. It dispatches live events
// to BranchWorkers, routes them through per-GitTarget event streams for buffering and
// deduplication, and drives the synchronous streaming-snapshot resync (M8).
type EventRouter struct {
	WorkerManager *git.WorkerManager
	WatchManager  *Manager
	Client        client.Client
	Log           logr.Logger

	// Registry of GitTargetEventStreams by gitDest key
	gitTargetStreams map[string]*reconcile.GitTargetEventStream
	streamsMu        sync.RWMutex
}

// NewEventRouter creates a new event router.
func NewEventRouter(
	workerManager *git.WorkerManager,
	watchManager *Manager,
	client client.Client,
	log logr.Logger,
) *EventRouter {
	return &EventRouter{
		WorkerManager:    workerManager,
		WatchManager:     watchManager,
		Client:           client,
		Log:              log,
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
}

// RouteEvent sends an event to the worker for (provider, branch).
// The target info is used to lookup the worker, then the event is queued.
// Returns an error if no worker exists for the given (provider, branch) combination.
func (r *EventRouter) RouteEvent(
	providerName, providerNamespace string,
	branch string,
	event git.Event,
) error {
	worker, exists := r.WorkerManager.GetWorkerForTarget(
		providerName, providerNamespace, branch,
	)

	if !exists {
		return fmt.Errorf("no worker for provider=%s/%s branch=%s",
			providerNamespace, providerName, branch)
	}

	worker.Enqueue(event)

	r.Log.V(1).Info("Event routed to worker",
		"provider", providerName,
		"namespace", providerNamespace,
		"branch", branch,
		"operation", event.Operation,
		"path", event.Path)

	return nil
}

// FinalizeGitTargetWindow finalizes the open commit window for the branch
// worker backing the named GitTarget. It resolves the GitTarget's
// (provider, branch), enqueues a finalize signal on that worker's event
// queue, and blocks until the worker reports the outcome.
//
// The signal is bound to author and target: a worker is keyed only by
// provider and branch, so it finalizes the window only when the open window
// belongs to the same author and GitTarget.
//
// When no worker exists for the GitTarget there is, by definition, no open
// commit window, so the result is NoOpenWindow rather than an error. When the
// worker reports a finalize failure (e.g. a failed local commit or a
// saturated queue) that failure is returned as an error.
func (r *EventRouter) FinalizeGitTargetWindow(
	ctx context.Context,
	author, gitTargetName, gitTargetNamespace, message string,
) (git.FinalizeResult, error) {
	var gitTarget configv1alpha1.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      gitTargetName,
		Namespace: gitTargetNamespace,
	}, &gitTarget); err != nil {
		return git.FinalizeResult{}, fmt.Errorf("get GitTarget %s/%s: %w",
			gitTargetNamespace, gitTargetName, err)
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.ProviderRef.Name,
		gitTarget.Namespace, // provider is in the same namespace as the target
		gitTarget.Spec.Branch,
	)
	if !exists {
		r.Log.V(1).Info("FinalizeGitTargetWindow: no worker for GitTarget, nothing to finalize",
			"gitTarget", gitTargetNamespace+"/"+gitTargetName)
		return git.FinalizeResult{
			Outcome: git.FinalizeNoOpenWindow,
			Branch:  gitTarget.Spec.Branch,
		}, nil
	}

	resultCh := make(chan git.FinalizeResult, 1)
	worker.EnqueueFinalize(&git.FinalizeSignal{
		CommitMessage:      message,
		Author:             author,
		GitTargetName:      gitTargetName,
		GitTargetNamespace: gitTargetNamespace,
		Result:             resultCh,
	})

	select {
	case result := <-resultCh:
		// A finalize failure (failed commit, saturated queue) must surface as
		// an error so callers do not mistake it for a benign terminal outcome.
		if result.Err != nil {
			return result, result.Err
		}
		return result, nil
	case <-ctx.Done():
		return git.FinalizeResult{}, ctx.Err()
	case <-time.After(finalizeSignalTimeout):
		return git.FinalizeResult{}, fmt.Errorf("timed out finalizing window for GitTarget %s/%s",
			gitTargetNamespace, gitTargetName)
	}
}

// EmitResyncForGitDest runs one content-derived, mark-and-sweep resync for gitDest and
// blocks until the worker has applied it (M8). It is the replacement for the old
// two-snapshot handshake: it gathers the GitTarget's complete watched resource set via
// the streaming-list watch, hands that revision-pinned snapshot to the branch worker as
// a synchronous resync request, and returns the change counts the worker computed.
//
// The gather fails closed on a partial stream (StreamClusterSnapshotForGitDest aborts),
// so a resync is enqueued only for a complete snapshot — the worker can never sweep on
// partial knowledge. The call is synchronous so the caller (the GitTarget snapshot gate
// or ReconcileForRuleChange) learns the outcome and can order live-event flushing after
// the snapshot commit.
func (r *EventRouter) EmitResyncForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) (git.ResyncStats, error) {
	resultCh, err := r.gatherAndEnqueueResync(ctx, gitDest)
	if err != nil {
		return git.ResyncStats{}, err
	}

	select {
	case result := <-resultCh:
		if result.Err != nil {
			return git.ResyncStats{}, result.Err
		}
		r.logResyncApplied(gitDest, result.Stats)
		return result.Stats, nil
	case <-ctx.Done():
		return git.ResyncStats{}, ctx.Err()
	case <-time.After(resyncSignalTimeout):
		return git.ResyncStats{}, fmt.Errorf("timed out resyncing %s", gitDest.String())
	}
}

// TriggerResyncForGitDest gathers and enqueues a resync without blocking on the commit.
// It is the rule-change path's entry point: that path only needs each affected target's
// resync STARTED (not its stats), so many targets' commits proceed in parallel at their
// own workers instead of serializing on the single reconcile goroutine — matching the
// old fire-and-forget snapshot behaviour. The synchronous gather still fails closed, so
// an unobservable API surface is returned as an error before anything is enqueued.
//
// Delivery is marked by the caller as soon as the resync is ENQUEUED (not when it
// commits). An earlier version gated delivery on the apply completing, but that turned a
// slow or failed apply into an unbounded re-resync loop: the target stayed pending, so
// every subsequent reconcile re-gathered the whole snapshot synchronously, starving the
// reconcile goroutine and piling resync requests onto the worker. A failed resync is
// instead recovered by the steady-state live-event path (which writes any subsequent
// change) and by the next genuine rule-set change, not by re-running the whole snapshot
// on a tight loop. The worker reply is drained in the background to log the outcome and,
// on failure/timeout, increment ResyncBackgroundFailuresTotal so the silently-recovered
// failures are observable/alertable without re-firing the gather.
func (r *EventRouter) TriggerResyncForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) error {
	resultCh, err := r.gatherAndEnqueueResync(ctx, gitDest)
	if err != nil {
		return err
	}
	go func() {
		select {
		case result := <-resultCh:
			if result.Err != nil {
				r.Log.Error(result.Err, "background resync failed", "gitDest", gitDest.String())
				r.recordBackgroundResyncFailure(gitDest)
				return
			}
			r.logResyncApplied(gitDest, result.Stats)
		case <-time.After(resyncSignalTimeout):
			r.Log.Error(nil, "background resync timed out", "gitDest", gitDest.String())
			r.recordBackgroundResyncFailure(gitDest)
		}
	}()
	return nil
}

// recordBackgroundResyncFailure counts a fire-and-forget resync whose apply failed or
// timed out at the worker, so the failure is observable even though delivery was already
// marked on enqueue. No-op until the counter is registered.
func (r *EventRouter) recordBackgroundResyncFailure(gitDest types.ResourceReference) {
	if telemetry.ResyncBackgroundFailuresTotal == nil {
		return
	}
	telemetry.ResyncBackgroundFailuresTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
	))
}

// gatherAndEnqueueResync resolves the GitTarget's worker, gathers the revision-pinned
// streaming snapshot, and enqueues the resync request, returning the buffered reply
// channel. It does not wait for the commit. A missing GitTarget or worker, or an
// unobservable API surface, is returned as an error before anything is enqueued.
func (r *EventRouter) gatherAndEnqueueResync(
	ctx context.Context,
	gitDest types.ResourceReference,
) (chan git.ResyncResult, error) {
	worker, err := r.resolveWorkerForGitDest(ctx, gitDest)
	if err != nil {
		return nil, err
	}

	snapshot, err := r.WatchManager.StreamClusterSnapshotForGitDest(ctx, gitDest)
	if err != nil {
		return nil, err
	}

	resultCh := make(chan git.ResyncResult, 1)
	worker.EnqueueResync(&git.ResyncRequest{
		Desired:            snapshot.Desired,
		Revision:           snapshot.Revision,
		GitTargetName:      gitDest.Name,
		GitTargetNamespace: gitDest.Namespace,
		Result:             resultCh,
	})
	return resultCh, nil
}

// resolveWorkerForGitDest looks up the branch worker that owns a GitTarget's provider/branch.
// A missing GitTarget (a rule briefly outliving its target during deletion) or a worker that
// is not yet live is returned as an error, before anything is gathered or enqueued.
func (r *EventRouter) resolveWorkerForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) (*git.BranchWorker, error) {
	var gitTarget configv1alpha1.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      gitDest.Name,
		Namespace: gitDest.Namespace,
	}, &gitTarget); err != nil {
		return nil, fmt.Errorf("get GitTarget %s: %w", gitDest.String(), err)
	}
	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.ProviderRef.Name,
		gitTarget.Namespace, // provider is in the same namespace as the target
		gitTarget.Spec.Branch,
	)
	if !exists {
		return nil, fmt.Errorf("no worker for %s", gitDest.String())
	}
	return worker, nil
}

// EmitTypeReconcileForGitDest runs one M12 per-type reconcile: it streams just gvr's resources
// for the GitTarget and enqueues a type-scoped resync (upserts that type's objects, sweeps only
// that type's orphans). It is fire-and-forget — the worker reply is drained in the background,
// like the rule-change resync — so the registry's event-drain goroutine never blocks on a
// commit. A type this GitTarget does not watch, or an unobservable surface, is returned as an
// error before anything is enqueued.
func (r *EventRouter) EmitTypeReconcileForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
) error {
	snapshot, err := r.WatchManager.StreamSnapshotForType(ctx, gitDest, gvr)
	if err != nil {
		return err
	}
	resultCh, err := r.enqueueScopedResync(ctx, gitDest, gvr, snapshot.Desired, snapshot.Revision)
	if err != nil {
		return err
	}
	go r.drainScopedResync(gitDest, gvr, "reconcile", resultCh)
	return nil
}

// EmitTypeSweepForGitDest runs one M12 per-type sweep: a type-scoped resync with an EMPTY
// desired set, so a removed type's managed documents are dropped and no sibling type is
// touched. It does NOT stream — the type is gone from the API, so its desired set is
// definitionally empty. Like the reconcile it is fire-and-forget. A GitTarget that holds no
// documents of the type produces a no-op commit.
func (r *EventRouter) EmitTypeSweepForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
) error {
	resultCh, err := r.enqueueScopedResync(ctx, gitDest, gvr, nil, "")
	if err != nil {
		return err
	}
	go r.drainScopedResync(gitDest, gvr, "sweep", resultCh)
	return nil
}

// enqueueScopedResync resolves the GitTarget's worker and enqueues a type-scoped resync,
// returning the buffered reply channel. The ScopeGVR restricts the worker's mark-and-sweep to
// the one type, so desired must carry only that type's objects (empty for a sweep).
func (r *EventRouter) enqueueScopedResync(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	desired []manifestanalyzer.DesiredResource,
	revision string,
) (chan git.ResyncResult, error) {
	worker, err := r.resolveWorkerForGitDest(ctx, gitDest)
	if err != nil {
		return nil, err
	}
	scope := gvr
	resultCh := make(chan git.ResyncResult, 1)
	worker.EnqueueResync(&git.ResyncRequest{
		Desired:            desired,
		Revision:           revision,
		GitTargetName:      gitDest.Name,
		GitTargetNamespace: gitDest.Namespace,
		ScopeGVR:           &scope,
		Result:             resultCh,
	})
	return resultCh, nil
}

// drainScopedResync logs a per-type reconcile/sweep's outcome and, on failure or timeout,
// counts it as a background resync failure so a silently-recovered fault stays observable. The
// steady-state live-event path and the next type transition recover a failed apply, so this
// never re-fires the gather.
func (r *EventRouter) drainScopedResync(
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	kind string,
	resultCh chan git.ResyncResult,
) {
	select {
	case result := <-resultCh:
		if result.Err != nil {
			r.Log.Error(result.Err, "per-type "+kind+" failed", "gitDest", gitDest.String(), "gvr", gvr.String())
			r.recordBackgroundResyncFailure(gitDest)
			return
		}
		r.Log.V(1).Info("per-type "+kind+" applied",
			"gitDest", gitDest.String(), "gvr", gvr.String(),
			"created", result.Stats.Created, "updated", result.Stats.Updated, "deleted", result.Stats.Deleted)
	case <-time.After(resyncSignalTimeout):
		r.Log.Error(nil, "per-type "+kind+" timed out", "gitDest", gitDest.String(), "gvr", gvr.String())
		r.recordBackgroundResyncFailure(gitDest)
	}
}

func (r *EventRouter) logResyncApplied(gitDest types.ResourceReference, stats git.ResyncStats) {
	r.Log.V(1).Info("Resync applied",
		"gitDest", gitDest.String(),
		"created", stats.Created,
		"updated", stats.Updated,
		"deleted", stats.Deleted,
		"skipped", stats.Skipped)
}

// RegisterGitTargetEventStream registers a GitTargetEventStream with the router.
// This allows routing events to specific GitTargetEventStreams for buffering and deduplication.
func (r *EventRouter) RegisterGitTargetEventStream(
	gitDest types.ResourceReference,
	stream *reconcile.GitTargetEventStream,
) {
	key := gitDest.Key()
	r.streamsMu.Lock()
	defer r.streamsMu.Unlock()
	r.gitTargetStreams[key] = stream
	r.Log.Info("Registered GitTargetEventStream",
		"gitDest", gitDest.String(),
		"stream", stream.String())
}

// GetGitTargetEventStream returns the registered GitTargetEventStream for a GitTarget.
func (r *EventRouter) GetGitTargetEventStream(gitDest types.ResourceReference) *reconcile.GitTargetEventStream {
	key := gitDest.Key()
	r.streamsMu.RLock()
	defer r.streamsMu.RUnlock()
	return r.gitTargetStreams[key]
}

// BeginReconciliationForStream transitions the registered GitTargetEventStream for the
// given gitDest into RECONCILING state so that live informer ADDED events are buffered
// rather than processed individually.  It is a no-op when no stream is registered yet.
func (r *EventRouter) BeginReconciliationForStream(gitDest types.ResourceReference) {
	stream := r.GetGitTargetEventStream(gitDest)
	if stream == nil {
		r.Log.V(1).Info("No GitTargetEventStream registered, skipping BeginReconciliation",
			"gitDest", gitDest.String())
		return
	}
	stream.BeginReconciliation()
	r.Log.Info("BeginReconciliation called for GitTargetEventStream", "gitDest", gitDest.String())
}

// CompleteReconciliationForStream transitions the registered GitTargetEventStream for the
// given gitDest out of RECONCILING state and flushes any buffered live events.
// It is a no-op when no stream is registered.
func (r *EventRouter) CompleteReconciliationForStream(gitDest types.ResourceReference) {
	stream := r.GetGitTargetEventStream(gitDest)
	if stream == nil {
		r.Log.V(1).Info("No GitTargetEventStream registered, skipping CompleteReconciliation",
			"gitDest", gitDest.String())
		return
	}
	stream.OnReconciliationComplete()
	r.Log.Info("CompleteReconciliation called for GitTargetEventStream", "gitDest", gitDest.String())
}

// UnregisterGitTargetEventStream removes a GitTargetEventStream from the router.
// This is called during GitTarget deletion cleanup.
func (r *EventRouter) UnregisterGitTargetEventStream(gitDest types.ResourceReference) {
	key := gitDest.Key()
	r.streamsMu.Lock()
	defer r.streamsMu.Unlock()
	if _, exists := r.gitTargetStreams[key]; exists {
		delete(r.gitTargetStreams, key)
		r.Log.Info("Unregistered GitTargetEventStream", "gitDest", gitDest.String())
	}
}

// RouteToGitTargetEventStream routes an event to a specific GitTargetEventStream.
// This replaces direct routing to BranchWorkers, enabling event buffering and deduplication.
func (r *EventRouter) RouteToGitTargetEventStream(
	event git.Event,
	gitDest types.ResourceReference,
) error {
	key := gitDest.Key()
	r.streamsMu.RLock()
	stream, exists := r.gitTargetStreams[key]
	r.streamsMu.RUnlock()

	if !exists {
		return fmt.Errorf("no GitTargetEventStream registered for %s", key)
	}

	stream.OnWatchEvent(event)

	r.Log.V(1).Info("Event routed to GitTargetEventStream",
		"gitDest", gitDest.String(),
		"operation", event.Operation,
		"path", event.Path,
		"resource", event.Identifier.String())

	return nil
}
