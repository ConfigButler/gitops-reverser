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

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// CommitRequestFinalizer is the EventRouter seam the reconciler drives, using the
// attach-then-poll protocol (docs/design/stream/commitrequest-design.md §6.4.3):
// ServiceCommitRequest registers the attach idempotently on the GitTarget's branch
// worker (bind the message to the author's open window, finalize after the grace)
// and returns the request's current outcome — resolved=false means keep polling.
// watch.EventRouter satisfies it without adaptation.
//
// There is no watermark barrier (§6.3): UC1 is covered by the human gap between the
// edit and the save, UC2 by the collect-grace. The grace is anchored at attribution
// — the worker stamps finalizeAt = receipt + delaySeconds (§6.4.4) — so the
// controller no longer holds the finalize itself.
type CommitRequestFinalizer interface {
	ServiceCommitRequest(ctx context.Context, attach git.AttachCommitRequest) (git.FinalizeResult, bool, error)
}

// CommandAuthorLookup resolves the author of a CommitRequest from the submitter
// captured at admission by the internal-commands webhook, keyed by the persisted
// object's UID. *queue.CommandAuthorStore satisfies it without adaptation. The lookup
// is present-or-never (docs/design/commitrequest-admission-authorship.md §2): a miss is
// immediate and final — the webhook is not configured (or a best-effort write missed) —
// and the controller finalizes as the committer with no wait.
type CommandAuthorLookup interface {
	LookupCommandAuthor(ctx context.Context, uid types.UID) (queue.CommandAuthor, bool)
}

// windowMismatchMessage explains the author-bound refusal: an open window
// existed but was not this requester's, so it was deliberately left alone.
const windowMismatchMessage = "the open commit window belongs to a different author or GitTarget; " +
	"nothing was committed for this request"

// resolveTimeoutMessage explains the fail-closed resolve bound: the worker never
// reported an outcome for the attached request within the safety window (e.g. the
// branch worker vanished), so the request fails closed rather than poll forever.
const resolveTimeoutMessage = "the CommitRequest finalize did not resolve within the safety window"

const (
	// commitRequestPollInterval is the requeue cadence while polling the worker
	// for the attached request's outcome (attach-then-poll, §6.4.3).
	commitRequestPollInterval = 2 * time.Second

	// commitRequestResolveTimeout bounds the attach-then-poll wait, measured from
	// object creation: it must cover the maximum collect-grace (delaySeconds ≤ 300s,
	// anchored at attribution) and the push cooldown plus retries. Authorship is now
	// settled synchronously at first sight (no attribution wait, §2), so the former
	// +60s attribution component is gone. Past it, a request the worker never resolved
	// (e.g. a vanished worker) fails closed instead of polling forever.
	commitRequestResolveTimeout = 300*time.Second + 120*time.Second
)

// CommitRequestReconciler drives a CommitRequest through its state machine
// (docs/design/stream/commitrequest-design.md §6.4 and
// docs/design/commitrequest-admission-authorship.md §5):
//
//  1. ATTRIBUTE — a single synchronous read of the submitter captured at admission
//     (present-or-never, §2). A hit names that submitter as the author
//     (AuthorAttributed=True); a miss falls back to the configured committer
//     immediately (AuthorAttributed=False). There is no wait and no requeue for the
//     author: the record is written before the object is visible, so waiting cannot
//     help.
//  2. ATTACH + POLL — the instant the author is settled, send the attach to the
//     GitTarget's worker (bind the message to the author's open window, finalize
//     after the grace) and poll the outcome. The grace is anchored at attribution
//     by the worker (finalizeAt = receipt + delaySeconds), so there is no
//     controller-side delay. A window belonging to someone else (or no window)
//     resolves NoOpenWindow; the foreign window stays open.
type CommitRequestReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// APIReader performs uncached reads so a stale cache echo of our own
	// status stamp can never re-run a finalize that already reached a
	// terminal phase. Nil falls back to the (cached) Client.
	APIReader client.Reader

	// Finalizer attaches the request to the author-bound open window and reports
	// its outcome; AuthorLookup resolves the submitter captured at admission. When
	// AuthorLookup is nil (the internal-commands webhook is disabled), requests
	// finalize as the configured committer — immediately, with AuthorAttributed=False.
	Finalizer    CommitRequestFinalizer
	AuthorLookup CommandAuthorLookup
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests/status,verbs=get;update;patch

// Reconcile advances one CommitRequest through attribute → attach + poll →
// terminal status. With MaxConcurrentReconciles=1 concurrent CommitRequests are
// serialized by construction, and the worker keys attaches by request identity so
// re-sends across poll requeues are idempotent.
func (r *CommitRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")

	commitRequest, done, err := r.loadActionableCommitRequest(ctx, req)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	if r.Finalizer == nil {
		log.V(1).Info("CommitRequest finalize disabled: no Finalizer configured",
			"name", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// 1. ATTRIBUTE: settle the commit author synchronously (present-or-never, §2).
	// A hit names the admission submitter; a miss is the configured committer. Either
	// way the decision is final — there is no wait and no requeue for the author.
	author, attribution := r.attributeAuthor(ctx, commitRequest)

	// First sight: stamp the still-running conditions so the object reports its
	// progress (kstatus InProgress) and AuthorAttributed is settled immediately. A
	// disabled controller returns above, so it never stamps.
	if conditionByType(commitRequest.Status.Conditions, ConditionTypeReady) == nil {
		markCommitRequestWaitingForCloseDelay(commitRequest, attribution)
		if err := r.Status().Update(ctx, commitRequest); err != nil {
			return ctrl.Result{}, err
		}
		log.V(1).Info("Stamped CommitRequest in-progress conditions", "name", req.NamespacedName)
	}

	// 2. ATTACH + POLL: register the attach idempotently the instant we attribute
	// (no controller-side delay — the worker anchors the grace at attribution,
	// §6.4.4) and poll the outcome.
	result, resolved, serviceErr := r.Finalizer.ServiceCommitRequest(ctx, git.AttachCommitRequest{
		Namespace:          commitRequest.Namespace,
		Name:               commitRequest.Name,
		UID:                string(commitRequest.UID),
		Author:             author.Author,
		GitTargetName:      commitRequest.Spec.TargetRef.Name,
		GitTargetNamespace: commitRequest.Namespace,
		Message:            capCommitRequestMessage(commitRequest.Spec.Message),
		CloseDelaySeconds:  commitRequest.Spec.CloseDelaySeconds,
	})
	if serviceErr != nil || !resolved {
		if serviceErr != nil {
			log.V(1).Info("CommitRequest attach not yet serviceable; will retry",
				"name", req.NamespacedName, "err", serviceErr.Error())
		}
		if time.Since(commitRequest.CreationTimestamp.Time) < commitRequestResolveTimeout {
			if err := r.recordCloseDelayWait(ctx, commitRequest, attribution); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: commitRequestPollInterval}, nil
		}
		log.Info("CommitRequest did not resolve within the safety window; failing closed",
			"name", req.NamespacedName)
		r.writeTerminalStatus(ctx, log, commitRequest,
			git.FinalizeResult{}, errors.New(resolveTimeoutMessage), attribution)
		return ctrl.Result{}, nil
	}

	if result.Err != nil {
		log.Error(result.Err, "CommitRequest finalize failed",
			"gitTarget", commitRequest.Spec.TargetRef.Name, "name", req.NamespacedName)
	}
	r.writeTerminalStatus(ctx, log, commitRequest, result, result.Err, attribution)
	return ctrl.Result{}, nil
}

// attributeAuthor settles the commit author with a single synchronous lookup of the
// submitter captured at admission (present-or-never, §2). It never waits: a nil
// AuthorLookup (the internal-commands webhook is disabled) or a miss both resolve to
// the configured committer immediately. The miss case is final — the record is written
// before the object is visible, so there is no asynchronous arrival to wait for.
//
// The lookup result is logged at Info: it is the counterpart to the admission handler's
// "recorded command author" line, so a hit/miss pair makes the whole capture→read path
// legible (the first thing to check when a CommitRequest commits as the committer).
func (r *CommitRequestReconciler) attributeAuthor(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha2.CommitRequest,
) (queue.CommandAuthor, commitRequestAttribution) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")
	if r.AuthorLookup == nil {
		log.Info("command-author lookup disabled (internal-commands webhook off); committing as committer",
			"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID)
		return queue.CommandAuthor{}, attributionCommitter
	}
	if author, ok := r.AuthorLookup.LookupCommandAuthor(ctx, commitRequest.UID); ok {
		log.Info("command author resolved from admission record",
			"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID, "author", author.Author)
		return author, attributionFromAdmission
	}
	log.Info("no admission command-author record found; committing as committer",
		"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID)
	return queue.CommandAuthor{}, attributionCommitter
}

// recordCloseDelayWait makes the post-attribution wait — the closeDelaySeconds
// collect window followed by the commit and push. First sight already stamps this
// state, so this is the post-restart re-stamp path: it writes once, at the transition,
// and is a no-op once the request is already showing the WaitingForCloseDelay reason
// (so polling does not re-write status every interval).
func (r *CommitRequestReconciler) recordCloseDelayWait(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha2.CommitRequest,
	attribution commitRequestAttribution,
) error {
	if c := conditionByType(commitRequest.Status.Conditions, ConditionTypeReconciling); c != nil &&
		c.Reason == crReasonWaitingForCloseDelay {
		return nil
	}
	markCommitRequestWaitingForCloseDelay(commitRequest, attribution)
	return r.Status().Update(ctx, commitRequest)
}

// loadActionableCommitRequest fetches the CommitRequest and short-circuits
// everything that must not reach the finalize: a deleted object, a terminal
// outcome, and — via an uncached re-read — a stale cache echo of our own
// terminal write from a previous invocation (work past this point must happen
// at most once per CommitRequest). done=true means the reconcile has nothing
// further to do.
func (r *CommitRequestReconciler) loadActionableCommitRequest(
	ctx context.Context,
	req ctrl.Request,
) (*configbutleraiv1alpha2.CommitRequest, bool, error) {
	var commitRequest configbutleraiv1alpha2.CommitRequest
	if err := r.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	if commitRequestIsTerminal(&commitRequest) {
		return nil, true, nil
	}

	if r.APIReader != nil {
		if err := r.APIReader.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
			return nil, true, client.IgnoreNotFound(err)
		}
		if commitRequestIsTerminal(&commitRequest) {
			return nil, true, nil
		}
	}

	return &commitRequest, false, nil
}

// commitRequestStatusUpdateAttempts bounds the terminal-status conflict retry.
const commitRequestStatusUpdateAttempts = 3

// writeTerminalStatus records the finalize outcome on the CommitRequest,
// retrying on optimistic-concurrency conflicts. Like the audit-consumer path
// it replaces, a permanently failing write is logged and given up on rather
// than returned for requeue: re-running the reconcile would re-finalize an
// already-flushed window and mis-report the outcome as NoOpenWindow.
func (r *CommitRequestReconciler) writeTerminalStatus(
	ctx context.Context,
	log logr.Logger,
	commitRequest *configbutleraiv1alpha2.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
	attribution commitRequestAttribution,
) {
	expectedUID := commitRequest.UID
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	current := commitRequest
	for attempt := 1; attempt <= commitRequestStatusUpdateAttempts; attempt++ {
		applyFinalizeResultToStatus(current, result, finalizeErr, attribution)

		err := r.Status().Update(ctx, current)
		if err == nil {
			finalizeError := ""
			if finalizeErr != nil {
				finalizeError = finalizeErr.Error()
			}
			readyReason, readyMessage := commitRequestReadyReason(current)
			log.Info("CommitRequest finalized",
				"name", client.ObjectKeyFromObject(current),
				"ready", commitRequestConditionStatus(current, ConditionTypeReady),
				"reason", readyReason,
				"message", readyMessage,
				"authorAttributed", commitRequestConditionStatus(current, ConditionTypeAuthorAttributed),
				"pushed", commitRequestConditionStatus(current, ConditionTypePushed),
				"branch", current.Status.Branch,
				"sha", current.Status.SHA,
				"outcome", result.Outcome,
				"finalizeError", finalizeError,
				"gitTarget", current.Spec.TargetRef.Name,
				"age", time.Since(current.CreationTimestamp.Time).String())
			return
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "Failed to write CommitRequest status")
			return
		}

		log.V(1).Info("Conflict writing CommitRequest status; retrying", "attempt", attempt)
		var fresh configbutleraiv1alpha2.CommitRequest
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(commitRequest), &fresh); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				log.Info("CommitRequest deleted before status could be written; skipping")
				return
			}
			log.Error(getErr, "Failed to re-read CommitRequest for status update")
			return
		}
		// Never stamp the outcome onto a different incarnation or over a
		// terminal outcome another writer got in first.
		if fresh.UID != expectedUID {
			log.Info("CommitRequest UID changed before status could be written; skipping",
				"expectedUID", expectedUID, "objectUID", fresh.UID)
			return
		}
		if commitRequestIsTerminal(&fresh) {
			return
		}
		current = &fresh
	}

	log.Error(nil, "Gave up writing CommitRequest status after repeated conflicts")
}

// SetupWithManager sets up the controller with the Manager.
// MaxConcurrentReconciles is pinned to 1 on purpose: the single worker IS the
// multi-CommitRequest ordering design — concurrent CommitRequests for the same
// GitTarget are serialized exactly as a dedicated finalize-coordinator
// goroutine would serialize them, without the extra moving parts (see
// docs/design/stream/commitrequest-multi-finalize-design.md).
//
// Restart recovery is best-effort by design (commitrequest-design.md §6.6): the
// message is durable in spec.message, so on restart any non-terminal request is
// re-reconciled — re-attributed from its durable audit event and re-attached —
// which heals the common cases. The one knowingly-accepted gap is a request whose
// commit was already pushed but whose terminal status was not yet written: the
// in-memory outcome is gone, the re-driven attach finds the work already mirrored,
// and it resolves Rejected/AlreadyPresent. We do not build a durable record to
// close that.
func (r *CommitRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha2.CommitRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("commitrequest").
		Complete(r)
}
