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

// FinalizeAtWatermark is the CommitRequest barrier primitive (C-B1,
// docs/design/stream/canonical-stream-retirement.md §6): it drains the GitTarget's
// per-type audit tails to the per-type watermarks in snapshot — bounded by
// FinalizeBarrierTimeout — and then finalizes the open window. snapshot is assembled by
// TakeTypeSnapshot at CommitRequest creation time, giving each type its own independent
// watermark so no cross-type RV ordering is assumed. barrierReached=false reports the
// bounded degrade (Option A, commitrequest-barrier-timeout-decision.md): the finalize
// proceeded anyway and the caller must surface the missed guarantee in status.
func (r *EventRouter) FinalizeAtWatermark(
	ctx context.Context,
	author, gitTargetName, gitTargetNamespace, message string,
	snapshot map[schema.GroupVersionResource]string,
) (git.FinalizeResult, bool, error) {
	barrierReached := true
	if r.WatchManager != nil && len(snapshot) > 0 {
		barrierReached = r.WatchManager.DrainTailsToSnapshot(ctx, snapshot, FinalizeBarrierTimeout)
		if !barrierReached {
			r.Log.Info("finalize watermark barrier timed out; finalizing without the ordering guarantee",
				"gitTarget", gitTargetNamespace+"/"+gitTargetName)
		}
	}
	result, err := r.FinalizeGitTargetWindow(ctx, author, gitTargetName, gitTargetNamespace, message)
	return result, barrierReached, err
}

// TakeTypeSnapshot returns the current stream-top RV for each type the GitTarget claims.
// Delegates to WatchManager.TakeTypeSnapshot; call this at CommitRequest creation time to
// build the per-type watermark map that FinalizeAtWatermark will wait on.
func (r *EventRouter) TakeTypeSnapshot(
	ctx context.Context, gitTargetName, gitTargetNamespace string,
) map[schema.GroupVersionResource]string {
	if r.WatchManager == nil {
		return nil
	}
	return r.WatchManager.TakeTypeSnapshot(ctx, types.NewResourceReference(gitTargetName, gitTargetNamespace))
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

// EmitTypeReconcileForGitDest runs one per-type reconcile by SPLICING the materialized API in
// Redis (checkpoint + log) into that type's desired set and enqueuing a type-scoped resync (upserts
// the type's objects, sweeps only the type's orphans) — the R2 pivot. It replaces the live
// per-reconcile streaming gather with a fold over Redis, so a reconcile makes ZERO API calls and N
// GitTargets fan out from one capture. It is fire-and-forget — the worker reply is drained in the
// background — so the wake goroutine never blocks on a commit.
//
// It is self-gating and idempotent (the splice is a pure function of checkpoint + log), so it is
// safe to call from every wake: a type-activation edge, the materializer's TypeSynced, and an audit
// arrival. When the splice holds — the GitTarget does not watch the type, or its checkpoint is not
// yet Synced (§7 fail-closed) — it no-ops without enqueuing. A genuine fail-closed condition (an
// unobserved surface, a wobbling type, or a Redis/splice failure) is returned as an error.
func (r *EventRouter) EmitTypeReconcileForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
) error {
	snapshot, ready, err := r.WatchManager.SpliceSnapshotForType(ctx, gitDest, gvr)
	if err != nil {
		return err
	}
	if !ready {
		// Benign hold: not watched, or the checkpoint is not Synced yet. The TypeSynced wake
		// re-fires the reconcile the instant the checkpoint becomes serviceable.
		return nil
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
		// Count an applied per-type RECONCILE as a completed GitTarget reconcile so the
		// per-pod counter advances after a restart — the drain signal the restart-snapshot
		// e2e gate reads (a sweep is excluded; it is a removal, not a steady-state reconcile).
		if kind == "reconcile" && r.WatchManager != nil {
			r.WatchManager.recordTargetReconcileCompleted(gitDest, "type_reconcile")
		}
	case <-time.After(resyncSignalTimeout):
		r.Log.Error(nil, "per-type "+kind+" timed out", "gitDest", gitDest.String(), "gvr", gvr.String())
		r.recordBackgroundResyncFailure(gitDest)
	}
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
