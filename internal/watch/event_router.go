// SPDX-License-Identifier: Apache-2.0

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

	// resyncWorker overrides the worker a scoped resync enters. nil resolves the GitTarget's branch
	// worker; tests set it to accept or refuse a snapshot without running one.
	resyncWorker func(ctx context.Context, gitDest types.ResourceReference) (resyncEnqueuer, error)
}

// resyncEnqueuer is the part of a branch worker a scoped resync enters through.
type resyncEnqueuer interface {
	EnqueueResync(request *git.ResyncRequest) bool
}

// intakePauser is the part of a branch worker that says whether it has paused intake. See
// git.BranchWorker.IntakePaused.
type intakePauser interface {
	IntakePaused() <-chan struct{}
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

// ServiceCommitRequest is the controller's attach-then-poll seam (§6.4.3): it
// resolves the worker that holds the request, or else the GitTarget's branch worker, registers the CommitRequest attach
// idempotently on that worker's FIFO event queue (attach it to the author's window
// with its message and timers), and returns the request's current
// outcome. resolved=false means the worker has not finished — the controller
// requeues and polls again.
//
// attach.GitTargetName/GitTargetNamespace name the GitTarget; the worker is keyed
// by its provider+branch. A worker that does not exist YET — at startup, or before the GitTarget's
// first reconcile — is not an answer: the request stays pending in phase WaitingForWorker and the
// controller polls again, within its safety window. Resolving NoOpenWindow there would end a save
// before the worker that could collect its writes, or record its CommitEmpty message, had started.
// It is reported as pending and NOT as an error, because the GitTarget did resolve: the controller
// reads a service error as "the target never resolved" and would count the request's eventual
// failure without its GitTarget labels. A GitTarget that cannot be read is that error.
func (r *EventRouter) ServiceCommitRequest(
	ctx context.Context,
	attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	worker, branch, err := r.commitRequestWorker(ctx, attach)
	if err != nil {
		return git.FinalizeResult{}, false, err
	}
	if worker == nil {
		r.Log.V(1).Info("ServiceCommitRequest: no branch worker for the GitTarget yet; will retry",
			"gitTarget", attach.GitTargetNamespace+"/"+attach.GitTargetName)
		return git.FinalizeResult{Branch: branch, Phase: git.PhaseWaitingForWorker}, false, nil
	}

	// Idempotent register (the worker keys by request identity and keeps the first
	// finalize deadline), then poll the outcome.
	worker.EnqueueAttach(&attach)
	result, resolved := worker.LookupCommitRequestOutcome(attach.Namespace, attach.Name, attach.UID)
	if !resolved {
		// Report where the worker says the request stands, so the controller never has to infer
		// a phase from having sent the attach.
		result.Phase = worker.LookupCommitRequestPhase(attach.Namespace, attach.Name, attach.UID)
		result.Held = heldBecause(worker, result.Phase)
	}
	return result, resolved, nil
}

// WithdrawCommitRequest is the controller's fail-closed seam: it cancels a request the worker has
// not acted on, and reports the worker's answer, so the controller never fails a request the worker
// could still commit.
//
//   - The worker that holds the request answers for it, even when its GitTarget is gone or the
//     worker is being retired. The withdraw rides its FIFO behind every attach already sent, and
//     is a no-op for a request it holds. Until it is handled, resolved=false and Phase says where
//     the request stands; a held phase means the controller keeps waiting.
//   - Nothing holds it, and the GitTarget is gone or names no worker: nothing can commit the
//     request, so it resolves at once as withdrawn. The controller sends no attach after it starts
//     withdrawing, so a worker that starts later never sees it.
func (r *EventRouter) WithdrawCommitRequest(
	ctx context.Context,
	attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	worker, branch, err := r.commitRequestWorker(ctx, attach)
	if errors.Is(err, errGitTargetGone) {
		// No worker holds the request, and its GitTarget can never start one.
		return git.FinalizeResult{Err: git.ErrCommitRequestWithdrawn}, true, nil
	}
	if err != nil {
		return git.FinalizeResult{}, false, err
	}
	if worker == nil {
		return git.FinalizeResult{
			Branch: branch,
			Phase:  git.PhaseWaitingForWorker,
			Err:    git.ErrCommitRequestWithdrawn,
		}, true, nil
	}

	if result, resolved := worker.LookupCommitRequestOutcome(attach.Namespace, attach.Name, attach.UID); resolved {
		return result, true, nil
	}
	phase := worker.LookupCommitRequestPhase(attach.Namespace, attach.Name, attach.UID)
	if phase.Held() {
		return git.FinalizeResult{Branch: branch, Phase: phase, Held: heldBecause(worker, phase)}, false, nil
	}
	worker.EnqueueWithdraw(&attach)
	// A worker whose loop has exited answers the withdraw at once.
	if result, resolved := worker.LookupCommitRequestOutcome(attach.Namespace, attach.Name, attach.UID); resolved {
		return result, true, nil
	}
	return git.FinalizeResult{Branch: branch, Phase: phase}, false, nil
}

// heldBecause is why a request waiting for its push has not reached the remote: the worker's
// publication report while it cannot publish, empty otherwise.
func heldBecause(worker *git.BranchWorker, phase git.CommitRequestPhase) string {
	if phase != git.PhaseWaitingForPush {
		return ""
	}
	return worker.Publication().Message()
}

// commitRequestWorker finds the worker to ask about a CommitRequest, and the branch it serves.
//
// The worker that holds the request comes first, whatever its GitTarget says now: a shared worker
// outlives a deleted target, and a retired one finishes its last push after the manager stopped
// listing it. Only a request no worker holds falls back to the GitTarget's current worker, which is
// nil while that worker has not started. A GitTarget that is gone is errGitTargetGone.
func (r *EventRouter) commitRequestWorker(
	ctx context.Context,
	attach git.AttachCommitRequest,
) (*git.BranchWorker, string, error) {
	if owner, ok := r.WorkerManager.CommitRequestOwner(attach.Namespace, attach.Name, attach.UID); ok {
		return owner, owner.Branch, nil
	}

	var gitTarget configv1alpha3.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      attach.GitTargetName,
		Namespace: attach.GitTargetNamespace,
	}, &gitTarget); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("%w: %s/%s", errGitTargetGone, attach.GitTargetNamespace, attach.GitTargetName)
		}
		return nil, "", fmt.Errorf("get GitTarget %s/%s: %w",
			attach.GitTargetNamespace, attach.GitTargetName, err)
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.GitProviderRef.Name,
		gitTarget.Namespace, // provider is in the same namespace as the target
		gitTarget.Spec.Branch,
	)
	if !exists {
		return nil, gitTarget.Spec.Branch, nil
	}
	return worker, gitTarget.Spec.Branch, nil
}

// recordBackgroundResyncFailure counts a fire-and-forget resync whose apply failed or
// timed out at the worker, so the failure is observable even though delivery was already
// marked on enqueue. No-op until the counter is registered.
func (r *EventRouter) recordBackgroundResyncFailure(gitDest types.ResourceReference) {
	if telemetry.GitResyncFailuresTotal == nil {
		return
	}
	telemetry.GitResyncFailuresTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
	))
}

// errGitTargetGone reports that a GitTarget no longer exists. It is terminal:
// nothing that depends on the target can succeed again, so a supervisor loop must
// stop rather than reconnect. Retrying instead is what let a deleted GitTarget's
// streams re-enqueue resyncs every backoff, filling the branch worker's shared
// queue and starving every other GitTarget on the same branch.
var errGitTargetGone = errors.New("GitTarget no longer exists")

// resolveWorkerForGitDest looks up the branch worker that owns a GitTarget's provider/branch.
// A missing GitTarget (a rule briefly outliving its target during deletion) or a worker that
// is not yet live is returned as an error, before anything is gathered or enqueued.
func (r *EventRouter) resolveWorkerForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) (*git.BranchWorker, error) {
	var gitTarget configv1alpha3.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      gitDest.Name,
		Namespace: gitDest.Namespace,
	}, &gitTarget); err != nil {
		if apierrors.IsNotFound(err) {
			// Distinguished from any other lookup failure because it is terminal:
			// the target is gone, so a caller retrying this forever is doing work
			// that can never succeed. See errGitTargetGone.
			return nil, fmt.Errorf("%w: %s", errGitTargetGone, gitDest.String())
		}
		return nil, fmt.Errorf("get GitTarget %s: %w", gitDest.String(), err)
	}
	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.GitProviderRef.Name,
		gitTarget.Namespace, // provider is in the same namespace as the target
		gitTarget.Spec.Branch,
	)
	if !exists {
		return nil, fmt.Errorf("no worker for %s", gitDest.String())
	}
	return worker, nil
}

// enqueueScopedResync resolves the GitTarget's worker and enqueues a scoped resync, returning
// the buffered reply channel and whether the resync actually entered the FIFO. The scope
// restricts the worker's mark-and-sweep to one type, and to one namespace when it names one, so
// desired MUST carry exactly that scope's objects (empty for a sweep) — passing a scope wider
// than the gather deletes managed documents outside it. heal marks a drift-correcting resync the
// worker defers while a commit window is open. enqueued is false when the worker's queue was full
// and dropped the request (its failure is still delivered on resultCh for the drain to record).
// resourceVersion is the LIST's collection version the desired set is pinned to.
func (r *EventRouter) enqueueScopedResync(
	ctx context.Context,
	gitDest types.ResourceReference,
	scope git.ResyncScope,
	sourceCollection types.CollectionKey,
	desired []manifestanalyzer.DesiredResource,
	resourceVersion string,
	heal bool,
) (chan git.ResyncResult, bool, error) {
	worker, err := r.resyncTarget(ctx, gitDest)
	if err != nil {
		return nil, false, err
	}
	resultCh := make(chan git.ResyncResult, 1)
	enqueued := worker.EnqueueResync(&git.ResyncRequest{
		Desired:            desired,
		ResourceVersion:    resourceVersion,
		GitTargetName:      gitDest.Name,
		GitTargetNamespace: gitDest.Namespace,
		Scope:              &scope,
		SourceCollection:   sourceCollection,
		Heal:               heal,
		Result:             resultCh,
	})
	return resultCh, enqueued, nil
}

// resyncTarget is the branch worker a GitTarget's scoped resyncs enter.
func (r *EventRouter) resyncTarget(ctx context.Context, gitDest types.ResourceReference) (resyncEnqueuer, error) {
	if r.resyncWorker != nil {
		return r.resyncWorker(ctx, gitDest)
	}
	worker, err := r.resolveWorkerForGitDest(ctx, gitDest)
	if err != nil {
		return nil, err
	}
	return worker, nil
}

// branchIntakePaused reports a GitTarget whose branch worker has paused intake: it returns a channel
// closed when intake reopens, or nil while it is open or the worker cannot be resolved, in which
// case nothing is known to wait for.
func (r *EventRouter) branchIntakePaused(ctx context.Context, gitDest types.ResourceReference) <-chan struct{} {
	worker, err := r.resyncTarget(ctx, gitDest)
	if err != nil {
		return nil
	}
	if pauser, ok := worker.(intakePauser); ok {
		return pauser.IntakePaused()
	}
	return nil
}

// resyncScopeForWatchKey is the single conversion from a watch key to the resync scope its
// replay must be swept under. It exists so the two halves of the invariant — the namespace a
// stream gathered, and the namespace its sweep may touch — are derived from ONE value and
// cannot drift apart at a call site.
//
// The scope carries the stream's whole collection, selector included, so two selections of one
// boundary never coalesce into one queue position. Its sweep stays structural (ResyncScope.Matches).
func resyncScopeForWatchKey(key targetWatchKey) git.ResyncScope {
	scope := git.ResyncScopeFor(key.GVR, key.Namespace)
	scope.Collection = key.Collection()
	return scope
}

// drainScopedResync logs a per-type reconcile/sweep's outcome and, on failure or timeout,
// counts it as a background resync failure so a silently-recovered fault stays observable. The
// steady-state live-event path and the next type transition recover a failed apply, so this
// never re-fires the gather.
func (r *EventRouter) drainScopedResync(
	gitDest types.ResourceReference,
	collection types.CollectionKey,
	kind string,
	renderFidelityEpoch uint64,
	resultCh chan git.ResyncResult,
) {
	select {
	case result := <-resultCh:
		if result.Err != nil {
			r.handleScopedResyncError(gitDest, collection, kind, renderFidelityEpoch, result.Err)
			return
		}
		r.Log.V(1).Info("per-type "+kind+" applied",
			"gitDest", gitDest.String(), "collection", collection.String(),
			"created", result.Stats.Created, "updated", result.Stats.Updated, "deleted", result.Stats.Deleted)
		if r.WatchManager != nil {
			r.WatchManager.MarkTargetGitPathScopeAccepted(gitDest, collection)
			r.WatchManager.MarkTargetRenderFidelityScopeClean(gitDest, renderFidelityEpoch, collection)
			// Recorded for every applied resync, including the ones that retained nothing: zero
			// is the converged signal and is only meaningful if it is published as actively as a
			// non-zero count.
			r.WatchManager.MarkTargetRetention(
				gitDest, collection, renderFidelityEpoch, result.Stats.PruneMode, result.Stats.Retained)
		}
		// Count an applied per-type RECONCILE as a completed GitTarget reconcile so the
		// per-pod counter advances after a restart — the drain signal the restart-reconcile
		// e2e gate reads (a sweep is excluded; it is a removal, not a steady-state reconcile).
		if kind == "reconcile" && r.WatchManager != nil {
			r.WatchManager.recordWatchRecovery(
				gitDest,
				collection.Group,
				collection.Resource,
				recoveryModeTypeReconcile,
			)
		}
	case <-time.After(resyncSignalTimeout):
		r.Log.Error(nil, "per-type "+kind+" timed out", "gitDest", gitDest.String(), "collection", collection.String())
		r.recordBackgroundResyncFailure(gitDest)
	}
}

// handleScopedResyncError classifies a failed resync. A path the acceptance gate refused
// is not a transient write fault: nothing was committed, the human must clean the Git path, so
// it is surfaced as target-level GitPathAccepted=False and is NOT counted as a background
// resync failure. Every other error stays a background failure so a silently-recovered fault
// remains observable.
func (r *EventRouter) handleScopedResyncError(
	gitDest types.ResourceReference,
	collection types.CollectionKey,
	kind string,
	renderFidelityEpoch uint64,
	err error,
) {
	var refused *manifestanalyzer.AcceptanceRefusedError
	if errors.As(err, &refused) {
		if refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderDoesNotMatchLive) {
			r.Log.Info("per-type "+kind+" found a render-vs-live divergence",
				"gitDest", gitDest.String(), "collection", collection.String(), "detail", refused.Error())
			if r.WatchManager != nil {
				r.WatchManager.MarkTargetRenderFidelityScopeDiverged(
					gitDest, renderFidelityEpoch, collection, renderFidelityDivergence(refused))
			}
			return
		}
		r.Log.Info("per-type "+kind+" refused: unsupported GitTarget path content",
			"gitDest", gitDest.String(), "collection", collection.String(), "detail", refused.Error())
		if r.WatchManager != nil {
			r.WatchManager.MarkTargetGitPathScopeRefused(
				gitDest, collection, gitPathRefusalReason(refused), refused.BlockMessage())
		}
		return
	}
	if errors.Is(err, git.ErrResyncSuperseded) {
		// A newer resync for the same scope replaced this one while it was queued and runs in its
		// place, so no WRITE was missed. Its REPORTS are skipped, on the reasoning that the
		// replacement marks acceptance, render fidelity and retention instead. That holds only if
		// the replacement's own report is then accepted, and a report is refused when it arrives
		// under a revision the plan has moved past.
		//
		// TEMPORARY at Info, restored. It is normally V(1) because coalescing is routine under
		// exactly the load it protects against, and it was lowered once on the assumption that the
		// render-fidelity condition had made every lost report visible. It has not: this path
		// skips the RETENTION report too, and lowering it re-blinded that path in the very local
		// reproduction of Failure B that followed. Lower it again only when B is closed.
		r.Log.Info("per-type "+kind+" superseded by a newer resync; its roll-up reports were skipped",
			"gitDest", gitDest.String(), "collection", collection.String())
		return
	}
	r.Log.Error(err, "per-type "+kind+" failed", "gitDest", gitDest.String(), "collection", collection.String())
	r.recordBackgroundResyncFailure(gitDest)
}

func renderFidelityDivergence(refused *manifestanalyzer.AcceptanceRefusedError) manifestanalyzer.RenderDivergence {
	for _, issue := range refused.Issues {
		if issue.Kind == manifestanalyzer.IssueRenderDoesNotMatchLive {
			return manifestanalyzer.RenderDivergence{Field: issue.Field, Token: issue.Token}
		}
	}
	return manifestanalyzer.RenderDivergence{}
}

// gitPathRefusalReason picks the GitTarget status reason for a refused path. Two refusal
// shapes are distinct enough to name, because they tell an operator something the umbrella
// reason does not:
//
//   - purely the .gittargetignore-shadows-a-write case (§4.3) — the unrecoverable footgun —
//     gets IgnoreShadowsManagedPath;
//   - purely write-boundary violations (a planned write escaping spec.path, an in-place edit
//     of a file more than one render root reaches, a write kustomize will not vouch for when the
//     folder is re-rendered with it applied, or an edit the projection could not place in the
//     source document) gets WriteBoundaryRefused: the folder content is fine, the *edit* had
//     nowhere safe to land.
//
// Any other refusal, and any mix of shapes, keeps the umbrella UnsupportedContent. The strings
// mirror the controller's GitTargetReason* constants (the watch package cannot import
// controller without a cycle), and all three are members of the controller's stalled-reason
// set, so every refusal surfaces as Stalled=True / kstatus Failed.
func gitPathRefusalReason(refused *manifestanalyzer.AcceptanceRefusedError) string {
	return manifestanalyzer.GitPathRefusalReason(refused)
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
