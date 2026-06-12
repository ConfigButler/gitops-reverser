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

// CommitRequestFinalizer is the EventRouter seam the reconciler drives: finalize
// the open commit window bound to the attributed author for a GitTarget.
// watch.EventRouter satisfies it without adaptation via FinalizeGitTargetWindow.
//
// There is no watermark barrier (docs/design/stream/commitrequest-design.md §6.3):
// UC1 is covered by the human gap between the edit and the save, UC2 by the
// collect-grace. The honest claim "Committed" comes from the finalize itself, not
// from a best-effort drain.
type CommitRequestFinalizer interface {
	FinalizeGitTargetWindow(ctx context.Context,
		author, gitTargetName, gitTargetNamespace, message string,
	) (git.FinalizeResult, error)
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

const (
	// commitRequestAttributionTimeout bounds the wait for the CommitRequest's
	// own create audit event. The audit ingest path normally delivers within
	// seconds; an event that has not arrived after this long indicates audit
	// ingestion is broken for this object, and the request fails closed.
	commitRequestAttributionTimeout = 60 * time.Second

	// commitRequestAttributionRetryDelay is the requeue cadence while waiting
	// for the audit event to be ingested.
	commitRequestAttributionRetryDelay = 2 * time.Second
)

// CommitRequestReconciler drives a CommitRequest through its state machine
// (C-B2 of docs/design/stream/canonical-stream-retirement.md):
//
//  1. ATTRIBUTE — wait until the CommitRequest's own create audit event
//     appears in the commitrequests per-type stream and take the author from
//     it. Every request enters through the audit ingestion path, so this also
//     ORDERS the finalize after the author's earlier changes: those entered
//     the path first and are already mirrored once attribution succeeds. An
//     event that never arrives fails the request (fail closed).
//  2. DELAY (optional) — spec.delaySeconds holds the finalize as an extra
//     collect window after creation.
//  3. FINALIZE — finalize the open window bound to the attributed author. A
//     window belonging to someone else (or no window) terminates as
//     NoOpenWindow with an explanatory message; the foreign window stays open.
type CommitRequestReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// APIReader performs uncached reads so a stale cache echo of our own
	// status stamp can never re-run a finalize that already reached a
	// terminal phase. Nil falls back to the (cached) Client.
	APIReader client.Reader

	// Finalizer finalizes the author-bound open window; AuthorLookup attributes
	// the author from the audit stream. When either is nil (partial test setups),
	// freshly created objects are stamped WaitingForAuditEvent and left there.
	Finalizer    CommitRequestFinalizer
	AuthorLookup CommitRequestAuthorLookup
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests/status,verbs=get;update;patch

// Reconcile advances one CommitRequest through attribute → delay → finalize →
// terminal status. The finalize runs inline in this one invocation; with
// MaxConcurrentReconciles=1 that serializes concurrent CommitRequests by
// construction — no two finalizes ever interleave, so an earlier CommitRequest's
// finalize is always enqueued before a later one's (the multi-CommitRequest
// ordering invariant).
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

	// 2. DELAY: the optional extra collect window, anchored at creation time
	// so the total hold is what the user asked for regardless of attribution
	// latency.
	if delay := time.Duration(commitRequest.Spec.DelaySeconds) * time.Second; delay > 0 {
		if remaining := time.Until(commitRequest.CreationTimestamp.Add(delay)); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	// 3. FINALIZE: every earlier write for the GitTarget's worker already rode
	// the FIFO audit path ahead of this finalize, so "the open window" is
	// whichever window is open now (§6.1). No barrier — UC1 is covered by the
	// human gap, UC2 by the collect-grace.
	gitTargetName := commitRequest.Spec.GitTargetRef.Name
	result, finalizeErr := r.Finalizer.FinalizeGitTargetWindow(
		ctx,
		author,
		gitTargetName,
		commitRequest.Namespace,
		capCommitRequestMessage(commitRequest.Spec.Message),
	)
	if finalizeErr != nil {
		log.Error(finalizeErr, "Failed to finalize commit window for CommitRequest",
			"gitTarget", gitTargetName, "name", req.NamespacedName)
	}

	r.writeTerminalStatus(ctx, log, commitRequest, result, finalizeErr)
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
			log.Info("CommitRequest finalized",
				"phase", current.Status.Phase,
				"branch", current.Status.Branch,
				"sha", current.Status.SHA)
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
func (r *CommitRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.CommitRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("commitrequest").
		Complete(r)
}
