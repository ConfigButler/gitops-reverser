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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

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

	if !worker.Enqueue(event) {
		return fmt.Errorf("worker queue full for provider=%s/%s branch=%s; event dropped",
			providerNamespace, providerName, branch)
	}

	r.Log.V(1).Info("Event routed to worker",
		"provider", providerName,
		"namespace", providerNamespace,
		"branch", branch,
		"operation", event.Operation,
		"path", event.Path)

	return nil
}

// ServiceCommitRequest is the controller's attach-then-poll seam (§6.4.3): it
// resolves the GitTarget's branch worker, registers the CommitRequest attach
// idempotently on that worker's FIFO event queue (bind the message to the author's
// open window, finalize after the grace), and returns the request's current
// outcome. resolved=false means the worker has not finished — the controller
// requeues and polls again.
//
// attach.GitTargetName/GitTargetNamespace name the GitTarget; the worker is keyed
// by its provider+branch. When no worker exists there is, by definition, no window
// to collect into, so the request resolves NoOpenWindow (as before). A GitTarget
// that cannot be read is a transient error the controller surfaces and retries.
func (r *EventRouter) ServiceCommitRequest(
	ctx context.Context,
	attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	var gitTarget configv1alpha2.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      attach.GitTargetName,
		Namespace: attach.GitTargetNamespace,
	}, &gitTarget); err != nil {
		return git.FinalizeResult{}, false, fmt.Errorf("get GitTarget %s/%s: %w",
			attach.GitTargetNamespace, attach.GitTargetName, err)
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.ProviderRef.Name,
		gitTarget.Namespace, // provider is in the same namespace as the target
		gitTarget.Spec.Branch,
	)
	if !exists {
		r.Log.V(1).Info("ServiceCommitRequest: no worker for GitTarget, nothing to collect into",
			"gitTarget", attach.GitTargetNamespace+"/"+attach.GitTargetName)
		return git.FinalizeResult{
			Outcome: git.FinalizeNoOpenWindow,
			Branch:  gitTarget.Spec.Branch,
		}, true, nil
	}

	// Idempotent register (the worker keys by request identity and keeps the first
	// finalize deadline), then poll the outcome.
	worker.EnqueueAttach(&attach)
	result, resolved := worker.LookupCommitRequestOutcome(attach.Namespace, attach.Name, attach.UID)
	return result, resolved, nil
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
	var gitTarget configv1alpha2.GitTarget
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

// enqueueScopedResync resolves the GitTarget's worker and enqueues a type-scoped resync,
// returning the buffered reply channel and whether the resync actually entered the FIFO. The
// ScopeGVR restricts the worker's mark-and-sweep to the one type, so desired must carry only that
// type's objects (empty for a sweep). heal marks a drift-correcting resync the worker defers while
// a commit window is open (see EmitType*ForGitDest). enqueued is false when the worker's queue was
// full and dropped the request (its failure is still delivered on resultCh for the drain to record).
func (r *EventRouter) enqueueScopedResync(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	desired []manifestanalyzer.DesiredResource,
	revision string,
	heal bool,
) (chan git.ResyncResult, bool, error) {
	worker, err := r.resolveWorkerForGitDest(ctx, gitDest)
	if err != nil {
		return nil, false, err
	}
	scope := gvr
	resultCh := make(chan git.ResyncResult, 1)
	enqueued := worker.EnqueueResync(&git.ResyncRequest{
		Desired:            desired,
		Revision:           revision,
		GitTargetName:      gitDest.Name,
		GitTargetNamespace: gitDest.Namespace,
		ScopeGVR:           &scope,
		Heal:               heal,
		Result:             resultCh,
	})
	return resultCh, enqueued, nil
}

// drainScopedResync logs a per-type reconcile/sweep's outcome and, on failure or timeout,
// counts it as a background resync failure so a silently-recovered fault stays observable. The
// steady-state live-event path and the next type transition recover a failed apply, so this
// never re-fires the gather.
func (r *EventRouter) drainScopedResync(
	gitDest types.ResourceReference,
	key targetWatchKey,
	kind string,
	resultCh chan git.ResyncResult,
) {
	select {
	case result := <-resultCh:
		if result.Err != nil {
			r.handleScopedResyncError(gitDest, key, kind, result.Err)
			return
		}
		r.Log.V(1).Info("per-type "+kind+" applied",
			"gitDest", gitDest.String(), "gvr", key.GVR.String(),
			"created", result.Stats.Created, "updated", result.Stats.Updated, "deleted", result.Stats.Deleted)
		if r.WatchManager != nil {
			r.WatchManager.MarkTargetFolderAccepted(gitDest)
		}
		// Count an applied per-type RECONCILE as a completed GitTarget reconcile so the
		// per-pod counter advances after a restart — the drain signal the restart-reconcile
		// e2e gate reads (a sweep is excluded; it is a removal, not a steady-state reconcile).
		if kind == "reconcile" && r.WatchManager != nil {
			r.WatchManager.recordTargetReconcileCompleted(gitDest, "type_reconcile")
		}
	case <-time.After(resyncSignalTimeout):
		r.Log.Error(nil, "per-type "+kind+" timed out", "gitDest", gitDest.String(), "gvr", key.GVR.String())
		r.recordBackgroundResyncFailure(gitDest)
	}
}

// handleScopedResyncError classifies a failed resync. A folder the acceptance gate refused
// is not a transient write fault: nothing was committed, the human must clean the folder, so
// it is surfaced as target-level FolderAccepted=False and is NOT counted as a background
// resync failure. Every other error stays a background failure so a silently-recovered fault
// remains observable.
func (r *EventRouter) handleScopedResyncError(
	gitDest types.ResourceReference,
	key targetWatchKey,
	kind string,
	err error,
) {
	var refused *manifestanalyzer.AcceptanceRefusedError
	if errors.As(err, &refused) {
		r.Log.Info("per-type "+kind+" refused: unsupported GitTarget folder content",
			"gitDest", gitDest.String(), "gvr", key.GVR.String(), "detail", refused.Error())
		if r.WatchManager != nil {
			r.WatchManager.MarkTargetFolderRefused(gitDest, "UnsupportedContent", refused.BlockMessage())
		}
		return
	}
	r.Log.Error(err, "per-type "+kind+" failed", "gitDest", gitDest.String(), "gvr", key.GVR.String())
	r.recordBackgroundResyncFailure(gitDest)
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

	if err := stream.OnWatchEvent(event); err != nil {
		return err
	}

	r.Log.V(1).Info("Event routed to GitTargetEventStream",
		"gitDest", gitDest.String(),
		"operation", event.Operation,
		"path", event.Path,
		"resource", event.Identifier.String())

	return nil
}
