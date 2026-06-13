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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
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

// CommitRequestAuthorLookup resolves the author of a CommitRequest from its
// own create audit event in the commitrequests per-type audit stream.
// queue.RedisByTypeStreamQueue satisfies it without adaptation.
type CommitRequestAuthorLookup interface {
	LookupCommitRequestAuthor(ctx context.Context, namespace, name string, uid types.UID) (string, bool)
}

// windowMismatchMessage explains the author-bound refusal: an open window
// existed but was not this requester's, so it was deliberately left alone.
const windowMismatchMessage = "the open commit window belongs to a different author or GitTarget; " +
	"nothing was committed for this request"

// attributionFailedMessage explains the fail-closed attribution bound: a
// CommitRequest whose own audit event never arrived cannot be bound to an
// author, and an unattributable finalize is an error, not a guess.
const attributionFailedMessage = "could not attribute the CommitRequest to an author: " +
	"its create audit event was not observed within the attribution window"

// resolveTimeoutMessage explains the fail-closed resolve bound: the worker never
// reported an outcome for the attached request within the safety window (e.g. the
// branch worker vanished), so the request fails closed rather than poll forever.
const resolveTimeoutMessage = "the CommitRequest finalize did not resolve within the safety window"

const (
	// commitRequestAttributionTimeout bounds the wait for the CommitRequest's
	// own create audit event. The audit ingest path normally delivers within
	// seconds; an event that has not arrived after this long indicates audit
	// ingestion is broken for this object, and the request fails closed.
	commitRequestAttributionTimeout = 60 * time.Second

	// commitRequestAttributionRetryDelay is the requeue cadence while waiting
	// for the audit event to be ingested.
	commitRequestAttributionRetryDelay = 2 * time.Second

	// commitRequestPollInterval is the requeue cadence while polling the worker
	// for the attached request's outcome (attach-then-poll, §6.4.3).
	commitRequestPollInterval = 2 * time.Second

	// commitRequestResolveTimeout bounds the attach-then-poll wait, measured from
	// object creation: it must cover attribution latency, the maximum collect-grace
	// (delaySeconds ≤ 300s, anchored at attribution), and the push cooldown plus
	// retries. Past it, a request the worker never resolved (e.g. a vanished worker)
	// fails closed instead of polling forever.
	commitRequestResolveTimeout = commitRequestAttributionTimeout + 300*time.Second + 120*time.Second
)

// CommitRequestReconciler drives a CommitRequest through its state machine
// (docs/design/stream/commitrequest-design.md §6.4):
//
//  1. ATTRIBUTE — wait until the CommitRequest's own create audit event appears
//     in the commitrequests per-type stream and take the author from it. Every
//     request enters through the audit ingestion path, so this also ORDERS the
//     finalize after the author's earlier changes: those entered the path first
//     and are already mirrored once attribution succeeds. An event that never
//     arrives fails the request (fail closed).
//  2. ATTACH + POLL — the instant the author is known, send the attach to the
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
	// its outcome; AuthorLookup attributes the author from the audit stream. When
	// either is nil (partial test setups), freshly created objects are stamped
	// WaitingForAuditEvent and left there.
	Finalizer    CommitRequestFinalizer
	AuthorLookup CommitRequestAuthorLookup
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests/status,verbs=get;update;patch

// Reconcile advances one CommitRequest through attribute → attach + poll →
// terminal status. With MaxConcurrentReconciles=1 concurrent CommitRequests are
// serialized by construction, and the worker keys attaches by request identity so
// re-sends across poll requeues are idempotent.
func (r *CommitRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")

	commitRequest, done, err := r.loadActionableCommitRequest(ctx, log, req)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	if r.Finalizer == nil || r.AuthorLookup == nil {
		log.V(1).Info("CommitRequest finalize disabled: no Finalizer/AuthorLookup configured",
			"name", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// 1. ATTRIBUTE: bind the request to the author recorded in its own audit
	// event. Not observed yet → poll within the bound; past the bound → fail
	// closed rather than finalize someone's window on a guess.
	author, attributed := r.AuthorLookup.LookupCommitRequestAuthor(
		ctx, commitRequest.Namespace, commitRequest.Name, commitRequest.UID)
	if !attributed {
		if time.Since(commitRequest.CreationTimestamp.Time) < commitRequestAttributionTimeout {
			return ctrl.Result{RequeueAfter: commitRequestAttributionRetryDelay}, nil
		}
		log.Info("CommitRequest attribution timed out; failing closed", "name", req.NamespacedName)
		r.writeTerminalStatus(ctx, log, commitRequest,
			git.FinalizeResult{}, errors.New(attributionFailedMessage))
		return ctrl.Result{}, nil
	}

	// 2. ATTACH + POLL: register the attach idempotently the instant we attribute
	// (no controller-side delay — the worker anchors the grace at attribution,
	// §6.4.4) and poll the outcome.
	result, resolved, serviceErr := r.Finalizer.ServiceCommitRequest(ctx, git.AttachCommitRequest{
		Namespace:          commitRequest.Namespace,
		Name:               commitRequest.Name,
		UID:                string(commitRequest.UID),
		Author:             author,
		GitTargetName:      commitRequest.Spec.GitTargetRef.Name,
		GitTargetNamespace: commitRequest.Namespace,
		Message:            capCommitRequestMessage(commitRequest.Spec.Message),
		DelaySeconds:       commitRequest.Spec.DelaySeconds,
	})
	if serviceErr != nil || !resolved {
		if serviceErr != nil {
			log.V(1).Info("CommitRequest attach not yet serviceable; will retry",
				"name", req.NamespacedName, "err", serviceErr.Error())
		}
		if time.Since(commitRequest.CreationTimestamp.Time) < commitRequestResolveTimeout {
			return ctrl.Result{RequeueAfter: commitRequestPollInterval}, nil
		}
		log.Info("CommitRequest did not resolve within the safety window; failing closed",
			"name", req.NamespacedName)
		r.writeTerminalStatus(ctx, log, commitRequest, git.FinalizeResult{}, errors.New(resolveTimeoutMessage))
		return ctrl.Result{}, nil
	}

	if result.Err != nil {
		log.Error(result.Err, "CommitRequest finalize failed",
			"gitTarget", commitRequest.Spec.GitTargetRef.Name, "name", req.NamespacedName)
	}
	r.writeTerminalStatus(ctx, log, commitRequest, result, result.Err)
	return ctrl.Result{}, nil
}

// loadActionableCommitRequest fetches the CommitRequest and short-circuits
// everything that must not reach the finalize: a deleted object, a terminal
// phase, and — via an uncached re-read — a stale cache echo of our own
// terminal write from a previous invocation (work past this point must happen
// at most once per CommitRequest). A freshly created object is stamped with
// the initial phase on the way through. done=true means the reconcile has
// nothing further to do.
func (r *CommitRequestReconciler) loadActionableCommitRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
) (*configbutleraiv1alpha1.CommitRequest, bool, error) {
	var commitRequest configbutleraiv1alpha1.CommitRequest
	if err := r.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	if isTerminalCommitRequestPhase(commitRequest.Status.Phase) {
		return nil, true, nil
	}

	if r.APIReader != nil {
		if err := r.APIReader.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
			return nil, true, client.IgnoreNotFound(err)
		}
		if isTerminalCommitRequestPhase(commitRequest.Status.Phase) {
			return nil, true, nil
		}
	}

	if commitRequest.Status.Phase == "" {
		commitRequest.Status.Phase = configbutleraiv1alpha1.CommitRequestPhaseWaitingForAuditEvent
		if err := r.Status().Update(ctx, &commitRequest); err != nil {
			return nil, true, err
		}
		log.V(1).Info("Stamped CommitRequest as WaitingForAuditEvent", "name", req.NamespacedName)
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
	commitRequest *configbutleraiv1alpha1.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
) {
	now := metav1.Now()
	expectedUID := commitRequest.UID
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	current := commitRequest
	for attempt := 1; attempt <= commitRequestStatusUpdateAttempts; attempt++ {
		applyFinalizeResultToStatus(current, result, finalizeErr, now)

		err := r.Status().Update(ctx, current)
		if err == nil {
			finalizeError := ""
			if finalizeErr != nil {
				finalizeError = finalizeErr.Error()
			}
			log.Info("CommitRequest finalized",
				"name", client.ObjectKeyFromObject(current),
				"phase", current.Status.Phase,
				"reason", current.Status.Reason,
				"message", current.Status.Message,
				"branch", current.Status.Branch,
				"sha", current.Status.SHA,
				"outcome", result.Outcome,
				"finalizeError", finalizeError,
				"gitTarget", current.Spec.GitTargetRef.Name,
				"age", time.Since(current.CreationTimestamp.Time).String())
			return
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "Failed to write CommitRequest status")
			return
		}

		log.V(1).Info("Conflict writing CommitRequest status; retrying", "attempt", attempt)
		var fresh configbutleraiv1alpha1.CommitRequest
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(commitRequest), &fresh); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				log.Info("CommitRequest deleted before status could be written; skipping")
				return
			}
			log.Error(getErr, "Failed to re-read CommitRequest for status update")
			return
		}
		// Never stamp the outcome onto a different incarnation or over a
		// terminal phase another writer got in first.
		if fresh.UID != expectedUID {
			log.Info("CommitRequest UID changed before status could be written; skipping",
				"expectedUID", expectedUID, "objectUID", fresh.UID)
			return
		}
		if isTerminalCommitRequestPhase(fresh.Status.Phase) {
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
		For(&configbutleraiv1alpha1.CommitRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("commitrequest").
		Complete(r)
}
