// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// SaveRequestFinalizer is the EventRouter seam the reconciler drives, using the
// attach-then-poll protocol (docs/spec/saverequest-design.md):
// ServiceSaveRequest registers the attach idempotently on the GitTarget's branch
// worker (attach it to the author's window with its message and timers)
// and returns the request's current outcome — resolved=false means keep polling.
// watch.EventRouter satisfies it without adaptation.
//
// There is no watermark barrier: the interactive case is covered by the human gap between the
// edit and the save, UC2 by the request's attach wait. The worker anchors that wait at its own
// registration and runs the window's timers, so the controller never holds the finalize itself.
type SaveRequestFinalizer interface {
	ServiceSaveRequest(ctx context.Context, attach git.AttachSaveRequest) (git.FinalizeResult, bool, error)
	// WithdrawSaveRequest cancels a request the worker has not acted on and reports the worker's
	// answer: resolved with git.ErrSaveRequestWithdrawn when it is cancelled, the real outcome
	// when the worker got there first, or resolved=false with the worker's phase while it decides.
	WithdrawSaveRequest(ctx context.Context, attach git.AttachSaveRequest) (git.FinalizeResult, bool, error)
	// LookupSaveRequestApply reports where the apply a request with catchUp:
	// AfterPush asked for stands, and false while the worker knows nothing of it.
	LookupSaveRequestApply(ctx context.Context, attach git.AttachSaveRequest) (git.SaveApply, bool, error)
}

// CommandAuthorLookup resolves the author of a SaveRequest from the submitter
// captured at admission by the validate-operator-types webhook, keyed by the persisted
// object's UID. *queue.CommandAuthorStore satisfies it without adaptation. The lookup
// is present-or-never (docs/spec/saverequest-admission-authorship.md): a miss is
// immediate and final — the webhook is not configured (or a best-effort write missed) —
// and the request claims no actor with no wait.
type CommandAuthorLookup interface {
	LookupCommandAuthor(ctx context.Context, uid types.UID) (queue.CommandAuthor, bool)
}

// windowMismatchMessage explains the author-bound refusal: an open window
// existed but was not this requester's, so it was deliberately left alone.
const saveInProgressMessage = "an earlier save of the same author on this GitTarget was still waiting for " +
	"or collecting a commit window, which carries one message; save again once it resolved"

const windowMismatchMessage = "the open commit window belongs to a different author or GitTarget; " +
	"nothing was committed for this request"

// resolveTimeoutMessage explains the fail-closed resolve bound: the worker never acted on the
// request within the safety window, so it was withdrawn rather than polled forever.
const resolveTimeoutMessage = "the SaveRequest finalize did not resolve within the safety window"

// noWorkerTimeoutMessage is resolveTimeoutMessage for a GitTarget that never started a branch
// worker, the one cause the controller can name without asking anybody.
const noWorkerTimeoutMessage = "the SaveRequest's GitTarget did not start a branch worker within the safety window"

const (
	// saveRequestPollInterval is the requeue cadence while polling the worker
	// for the attached request's outcome (attach-then-poll).
	saveRequestPollInterval = 2 * time.Second

	// saveRequestPushAllowance is what the safety bound allows on top of a request's own
	// timers, for the push cooldown and its retries. Authorship is settled synchronously at first
	// sight, so no attribution wait belongs here.
	saveRequestPushAllowance = 120 * time.Second

	// The request window defaults mirror the +kubebuilder:default markers on SaveRequestWindow.
	// The API server fills them in, so these apply only to a client that bypasses defaulting;
	// resolving to the same values keeps the two paths from disagreeing.
	defaultAttachTimeout = 2 * time.Second
	defaultMaxDuration   = 2 * time.Second
)

// requestWindow is a SaveRequest's window, resolved to the values the worker applies.
type requestWindow struct {
	attach        configbutleraiv1alpha3.AttachPolicy
	attachTimeout time.Duration
	idleTimeout   *time.Duration
	maxDuration   time.Duration
}

// resolveRequestWindow reads spec.window, falling back field by field to the schema defaults. An
// explicit "0s" stays zero, and an omitted idleTimeout stays nil (no idle close).
func resolveRequestWindow(spec configbutleraiv1alpha3.SaveRequestSpec) requestWindow {
	resolved := requestWindow{
		attach:        configbutleraiv1alpha3.AttachCurrentOrNext,
		attachTimeout: defaultAttachTimeout,
		maxDuration:   defaultMaxDuration,
	}
	window := spec.Window
	if window == nil {
		return resolved
	}
	if window.Attach != "" {
		resolved.attach = window.Attach
	}
	if window.AttachTimeout != nil {
		resolved.attachTimeout = window.AttachTimeout.Duration
	}
	if window.IdleTimeout != nil {
		idle := window.IdleTimeout.Duration
		resolved.idleTimeout = &idle
	}
	if window.MaxDuration != nil {
		resolved.maxDuration = window.MaxDuration.Duration
	}
	return resolved
}

// resolveTimeout bounds how long a request may go without the worker acting on it, measured from
// object creation: the request's own attach wait, then its collection, then the push. It does NOT
// bound a request the worker holds (attached to a window, or committed and waiting for the push):
// only the worker can say how that ends, however long a down remote or a recovery takes. Past the
// bound, an unheld request is withdrawn, and fails only once the worker agrees.
func (w requestWindow) resolveTimeout() time.Duration {
	return w.attachTimeout + w.maxDuration + saveRequestPushAllowance
}

// SaveRequestReconciler deliberately does NOT use reconcileStatus, which every other controller
// here shares. The difference is not stylistic, so it is worth saying once:
//
//   - Its For() carries no GenerationChangedPredicate, so a status write re-enqueues the object.
//     The lost-write hazard reconcileStatus exists to close — the winning status-only update being
//     filtered out, leaving nothing to correct the stale answer — cannot arise.
//   - It reads through APIReader (uncached) precisely so a cache echo cannot re-drive work, which
//     is the same lag that makes reconcileStatus lose its optimistic lock in the first place.
//   - The policies are opposites, and reconcileStatus's is the wrong one here. It drops a losing
//     write and asks the caller to come back and recompute; a re-run of THIS reconcile would
//     re-finalize an already-flushed window. So writeTerminalStatus retries in place, bounded, and
//     gives up rather than requeue. (The worker now defends the same boundary from its side: a
//     request whose window has committed stays identifiable until the push settles it, so a
//     re-sent attach is recognized rather than registered afresh. This policy is still the right
//     one, but a re-run is no longer the only thing standing between a flushed window and a
//     NoOpenWindow that never happened.)
//
// SaveRequestReconciler drives a SaveRequest through its state machine
// (docs/spec/saverequest-design.md and
// docs/spec/saverequest-admission-authorship.md):
//
//  1. ATTRIBUTE — a single synchronous read of the submitter captured at admission
//     (present-or-never). A hit names that submitter as the author
//     (AuthorAttributed=True); a miss claims no actor immediately
//     (AuthorAttributed=False). There is no wait and no requeue for the
//     author: the record is written before the object is visible, so waiting cannot
//     help.
//  2. ATTACH + POLL — the instant the author is settled, send the attach to the
//     GitTarget's worker, which attaches the request to the author's window with its
//     message and timers, and poll the outcome. The attach wait is anchored at the worker's
//     registration, so there is no controller-side delay. While it polls, the controller
//     reports the phase the worker reports. No window before the attach deadline resolves
//     NoWindow; a window belonging to someone else stays open for its own author.
type SaveRequestReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// APIReader performs uncached reads so a stale cache echo of our own
	// status stamp can never re-run a finalize that already reached a
	// terminal phase. Nil falls back to the (cached) Client.
	APIReader client.Reader

	// Finalizer attaches the request to the author-bound open window and reports
	// its outcome; AuthorLookup resolves the submitter captured at admission. When
	// AuthorLookup is nil (the validate-operator-types webhook is disabled), requests
	// claim no actor immediately, with AuthorAttributed=False. The attached window
	// determines the eventual Git author.
	Finalizer    SaveRequestFinalizer
	AuthorLookup CommandAuthorLookup

	// TTL is how long a request is kept once it finishes. It is written onto the request as
	// SaveRequestDeleteAfterAnnotation at that moment; zero writes nothing, so the request stays.
	TTL time.Duration
}

// SaveRequestDeleteAfterAnnotation carries the RFC 3339 time after which the controller deletes a
// finished SaveRequest. The controller writes it once, when the request finishes, and from then
// on the annotation alone decides: remove it to keep the request, or edit it to move the deletion.
// A request that finished before this annotation existed carries none and is never deleted.
const SaveRequestDeleteAfterAnnotation = "configbutler.ai/delete-after"

// +kubebuilder:rbac:groups=configbutler.ai,resources=saverequests,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=saverequests/status,verbs=get;update;patch

// Reconcile advances one SaveRequest through attribute → attach + poll →
// terminal status. With MaxConcurrentReconciles=1 concurrent SaveRequests are
// serialized by construction, and the worker keys attaches by request identity so
// re-sends across poll requeues are idempotent.
func (r *SaveRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("SaveRequestReconciler")

	saveRequest, done, err := r.loadActionableSaveRequest(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if done {
		if saveRequest != nil {
			if followsApply(saveRequest) {
				return r.followApply(ctx, saveRequest)
			}
			return r.expireFinished(ctx, saveRequest)
		}
		return ctrl.Result{}, nil
	}

	if r.Finalizer == nil {
		log.V(1).Info("SaveRequest finalize disabled: no Finalizer configured",
			"name", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if handled, err := r.refusePrunedGitTargetRef(ctx, saveRequest); handled || err != nil {
		return ctrl.Result{}, err
	}

	// 1. ATTRIBUTE: settle the request's actor synchronously (present-or-never).
	// A hit names the admission submitter; a miss claims no actor. Either way the
	// decision is final — there is no wait and no requeue for the author.
	author, attribution := r.attributeAuthor(ctx, saveRequest)

	claim, admitted, refusal, err := r.admitAtFirstSight(ctx, log, saveRequest, attribution)
	if err != nil || admitted == saveRequestRefused {
		return ctrl.Result{}, err
	}

	if err := r.stampFirstSightConditions(ctx, saveRequest, attribution); err != nil {
		return ctrl.Result{}, err
	}

	// The schema rejects an invalid literal message at admission, so a rejection here names an
	// object the schema could not have seen: one stored before the rule landed. It is terminal
	// rather than retried, because SaveRequest.spec is immutable and no apply can repair it.
	if err := git.ValidateLiteralCommitMessage(saveRequest.Spec.Message); err != nil {
		// The fallback identity: this fails before ServiceSaveRequest, so nothing has
		// confirmed the named GitTarget exists.
		r.writeTerminalStatus(ctx, log, saveRequest, git.FinalizeResult{}, err, attribution,
			saveRequestTarget{})
		return ctrl.Result{}, nil
	}

	// 2. ATTACH + POLL: register the attach idempotently the instant we attribute
	// (no controller-side delay — the worker anchors the wait at its registration)
	// and poll the outcome.
	window := resolveRequestWindow(saveRequest.Spec)
	attach := git.AttachSaveRequest{
		Namespace:          saveRequest.Namespace,
		Name:               saveRequest.Name,
		UID:                string(saveRequest.UID),
		Author:             author.Author,
		Attribution:        attribution.gitOutcome(),
		GitTargetName:      saveRequest.GitTargetKey().Name,
		GitTargetNamespace: saveRequest.GitTargetKey().Namespace,
		GitTargetUID:       saveRequest.Status.GitTargetUID,
		Message:            saveRequest.Spec.Message,
		Attach:             window.attach,
		AttachTimeout:      window.attachTimeout,
		IdleTimeout:        window.idleTimeout,
		MaxDuration:        window.maxDuration,
		CommitEmpty:        saveRequest.Spec.WhenNothingToCommit == configbutleraiv1alpha3.NothingToCommitCommitEmpty,
		CatchUpNow:         saveRequest.Spec.CatchUp == configbutleraiv1alpha3.CatchUpAfterPush,
		Unattributed:       claim,
	}
	if admitted == saveRequestReplaced {
		return r.refuseOnceUnheld(ctx, log, req, saveRequest, attribution, attach, refusal)
	}
	return r.sendSaveRequest(ctx, log, req, saveRequest, attribution, attach,
		time.Since(saveRequest.CreationTimestamp.Time) >= window.resolveTimeout(),
		admitted == saveRequestTargetMissing)
}

// sendSaveRequest sends the attach, or the withdraw once the request is past its bound, or, while
// its GitTarget does not exist and never admitted it, neither: nothing is attached before a GitTarget
// admitted the request, so one created later is asked first, on a later poll.
func (r *SaveRequestReconciler) sendSaveRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	attach git.AttachSaveRequest,
	pastTheBound, targetMissing bool,
) (ctrl.Result, error) {
	switch {
	case pastTheBound:
		// Past the bound the controller only withdraws: an attach sent now would sit behind the
		// withdraw on the worker's FIFO and could register the request after it was cancelled.
		return r.withdrawSaveRequest(ctx, log, req, saveRequest, attribution, attach)
	case targetMissing:
		return r.pollSaveRequest(ctx, saveRequest, attribution, git.PhaseWaitingForWorker, "")
	}
	return r.attachSaveRequest(ctx, log, req, saveRequest, attribution, attach)
}

// saveRequestAdmission is how admitAtFirstSight left a request.
type saveRequestAdmission int

const (
	// saveRequestAdmitted is a request its GitTarget admitted, now or before.
	saveRequestAdmitted saveRequestAdmission = iota
	// saveRequestTargetMissing is a request whose GitTarget does not exist and never admitted it.
	saveRequestTargetMissing
	// saveRequestRefused is a request now failed.
	saveRequestRefused
	// saveRequestReplaced is a request a GitTarget refuses that replaced the one that admitted it. An
	// attach may have been sent before, so it is refused only once no worker holds it.
	saveRequestReplaced
)

// admitAtFirstSight runs admitSaveRequest, records the GitTarget that admitted the request, and fails
// a request it refuses. A refusal is final unless that same GitTarget, by UID, admitted the request
// before: then its grants were lowered after an attach may have been sent, and the worker, which may
// hold the request, keeps what it was first sent. A GitTarget that did not exist when the request was
// first seen admitted nothing, so its refusal stands. One that was recreated since admitted nothing
// either, but the GitTarget it replaced did: its refusal is saveRequestReplaced, returned with the
// refusal, and waits for the worker that may hold the request. err is a read or write to retry.
func (r *SaveRequestReconciler) admitAtFirstSight(
	ctx context.Context,
	log logr.Logger,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
) (git.UnattributedClaim, saveRequestAdmission, error, error) {
	claim, targetUID, err := r.admitSaveRequest(ctx, saveRequest)
	recorded := saveRequest.Status.GitTargetUID
	switch {
	case err == nil && targetUID == "":
		if recorded != "" {
			// Its GitTarget is gone since it admitted the request: the attach path reports that.
			return claim, saveRequestAdmitted, nil, nil
		}
		return claim, saveRequestTargetMissing, nil, r.stampFirstSightConditions(ctx, saveRequest, attribution)
	case err == nil:
		if recorded == targetUID {
			return claim, saveRequestAdmitted, nil, nil
		}
		// Recorded before any attach is sent for this GitTarget, so a later refusal can tell the two
		// apart. At first sight the in-progress stamp that follows writes it.
		saveRequest.Status.GitTargetUID = targetUID
		if findCondition(saveRequest.Status.Conditions, ConditionTypeReady) == nil {
			return claim, saveRequestAdmitted, nil, nil
		}
		return claim, saveRequestAdmitted, nil, r.Status().Update(ctx, saveRequest)
	case !errors.Is(err, errSaveRequestNotAllowed) && !errors.Is(err, errTakeOverNotAllowed):
		return claim, saveRequestAdmitted, nil, err
	case recorded == targetUID:
		return git.UnattributedClaimNone, saveRequestAdmitted, nil, nil
	case recorded != "":
		return git.UnattributedClaimNone, saveRequestReplaced, err, nil
	}
	r.writeTerminalStatus(ctx, log, saveRequest, git.FinalizeResult{}, err, attribution,
		resolvedSaveRequestTarget(saveRequest))
	return claim, saveRequestRefused, nil, nil
}

// refuseOnceUnheld fails a request that a replacement of its GitTarget refuses. The GitTarget it
// replaced admitted the request, so an attach may have been sent, and the worker holding it may have
// committed it. That worker answers first: its outcome is reported, and a request it holds is waited
// for. The refusal is written only once the request is withdrawn, or nothing held it.
func (r *SaveRequestReconciler) refuseOnceUnheld(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	attach git.AttachSaveRequest,
	refusal error,
) (ctrl.Result, error) {
	result, resolved, withdrawErr := r.Finalizer.WithdrawSaveRequest(ctx, attach)
	if withdrawErr != nil || !resolved {
		if withdrawErr != nil {
			log.V(1).Info("SaveRequest withdraw not yet serviceable; will retry",
				"name", req.NamespacedName, "err", withdrawErr.Error())
		}
		return r.pollSaveRequest(ctx, saveRequest, attribution, result.Phase, result.Held)
	}
	finalizeErr := result.Err
	if errors.Is(finalizeErr, git.ErrSaveRequestWithdrawn) {
		result, finalizeErr = git.FinalizeResult{}, refusal
	}
	r.writeTerminalStatus(ctx, log, saveRequest, result, finalizeErr, attribution,
		resolvedSaveRequestTarget(saveRequest))
	return ctrl.Result{}, nil
}

// errTakeOverNotAllowed is a request that asks to claim more of a window whose writes name no author
// than the grants admitting it allow. It is terminal: the spec is immutable.
var errTakeOverNotAllowed = errors.New("spec.window.author asks for more than the GitTarget allows")

// errSaveRequestNotAllowed is a request whose GitTarget's spec.allowedSaveRequests admits no grant for
// its namespace. It is terminal: the spec is immutable, so a later grant reaches only a new request.
var errSaveRequestNotAllowed = errors.New("the GitTarget does not admit SaveRequests from this namespace")

// admitSaveRequest decides whether the request may save its GitTarget's windows, and what it may claim
// of a window whose writes name no author (git.AttachSaveRequest.Unattributed).
//
// A grant in the GitTarget's spec.allowedSaveRequests must admit the request's namespace, or the
// request is errSaveRequestNotAllowed. It then claims what spec.window.author asks for, and only while
// the GitTarget's KubeProvider has never received an audit fact: its AuditFactsReceived condition is
// not True, so no write watched on that cluster can name its author. On a cluster that has sent one, a
// write naming no author means its fact went missing, and nothing is claimed. A request that asks for
// more than the admitting grants' unattributedWindows allow is errTakeOverNotAllowed.
//
// A GitTarget that does not exist admits nothing yet, and is reported with an empty UID. Any other
// read error is returned as is, so the request is retried rather than refused.
func (r *SaveRequestReconciler) admitSaveRequest(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
) (git.UnattributedClaim, string, error) {
	var target configbutleraiv1alpha3.GitTarget
	if err := r.Get(ctx, saveRequest.GitTargetKey(), &target); err != nil {
		if apierrors.IsNotFound(err) {
			return git.UnattributedClaimNone, "", nil
		}
		return git.UnattributedClaimNone, "", fmt.Errorf("read GitTarget %s: %w", saveRequest.GitTargetKey(), err)
	}
	uid := string(target.UID)
	claim, err := r.claimOn(ctx, saveRequest, &target)
	return claim, uid, err
}

// claimOn is admitSaveRequest for a GitTarget that exists.
func (r *SaveRequestReconciler) claimOn(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	target *configbutleraiv1alpha3.GitTarget,
) (git.UnattributedClaim, error) {
	decision, admission, err := authz.SaveRequestAdmitted(ctx, r.Client, saveRequest, target)
	if err != nil {
		return git.UnattributedClaimNone, err
	}
	if !decision.Allowed {
		return git.UnattributedClaimNone, fmt.Errorf("%w: %s", errSaveRequestNotAllowed, decision.Message)
	}
	ask := configbutleraiv1alpha3.WindowAuthorOwn
	if window := saveRequest.Spec.Window; window != nil && window.Author != "" {
		ask = window.Author
	}
	if ask == configbutleraiv1alpha3.WindowAuthorOwn {
		return git.UnattributedClaimNone, nil
	}
	if !admission.UnattributedWindows.Allows(ask) {
		return git.UnattributedClaimNone, fmt.Errorf("%w: spec.window.author is %q, and the grants of GitTarget %s "+
			"that admit namespace %q allow unattributedWindows %q", errTakeOverNotAllowed, ask,
			saveRequest.GitTargetKey(), saveRequest.Namespace, admission.UnattributedWindows)
	}
	key := target.KubeProviderKey()
	if key.Name == "" {
		return git.UnattributedClaimNone, nil
	}
	var provider configbutleraiv1alpha3.KubeProvider
	if err := r.Get(ctx, key, &provider); err != nil {
		return git.UnattributedClaimNone, nil //nolint:nilerr // an unread provider claims nothing
	}
	if apimeta.IsStatusConditionTrue(provider.Status.Conditions, KubeProviderConditionAuditFactsReceived) {
		return git.UnattributedClaimNone, nil
	}
	if ask == configbutleraiv1alpha3.WindowAuthorTakeOver {
		return git.UnattributedClaimTakeOver, nil
	}
	return git.UnattributedClaimSavedBy, nil
}

// attachSaveRequest sends the attach and reports the outcome once the worker has one. Until then
// it records the worker's phase and polls; a service error is a not-yet-serviceable worker, so it
// polls on the same terms.
func (r *SaveRequestReconciler) attachSaveRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	attach git.AttachSaveRequest,
) (ctrl.Result, error) {
	result, resolved, serviceErr := r.Finalizer.ServiceSaveRequest(ctx, attach)
	if serviceErr != nil || !resolved {
		if serviceErr != nil {
			log.V(1).Info("SaveRequest attach not yet serviceable; will retry",
				"name", req.NamespacedName, "err", serviceErr.Error())
		}
		return r.pollSaveRequest(ctx, saveRequest, attribution, result.Phase, result.Held)
	}

	if result.Err != nil {
		log.Error(result.Err, "SaveRequest finalize failed",
			"gitTarget", saveRequest.GitTargetKey().String(), "name", req.NamespacedName)
	}
	r.writeTerminalStatus(ctx, log, saveRequest, result, result.Err, attribution,
		resolvedSaveRequestTarget(saveRequest))
	return ctrl.Result{}, nil
}

// withdrawSaveRequest handles a request past its safety window. A request the worker holds keeps
// polling with no bound of its own: only the worker can say how it ends. Any other request is
// withdrawn, and fails only on the worker's answer, so a request reported failed can never be
// committed afterwards. A withdraw that cannot be serviced yet polls on the same terms.
func (r *SaveRequestReconciler) withdrawSaveRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	attach git.AttachSaveRequest,
) (ctrl.Result, error) {
	result, resolved, withdrawErr := r.Finalizer.WithdrawSaveRequest(ctx, attach)
	if withdrawErr != nil || !resolved {
		if withdrawErr != nil {
			log.V(1).Info("SaveRequest withdraw not yet serviceable; will retry",
				"name", req.NamespacedName, "err", withdrawErr.Error())
		}
		// Not answered yet, or the worker holds the request: keep polling.
		return r.pollSaveRequest(ctx, saveRequest, attribution, result.Phase, result.Held)
	}

	if !errors.Is(result.Err, git.ErrSaveRequestWithdrawn) {
		// The worker resolved it on its own before the withdraw: report that outcome, as the
		// attach path would have.
		r.writeTerminalStatus(ctx, log, saveRequest, result, result.Err, attribution,
			resolvedSaveRequestTarget(saveRequest))
		return ctrl.Result{}, nil
	}

	// Withdrawn. A worker's answer, or a GitTarget that was read, carries the branch, and only
	// then is the request's identity known.
	target := saveRequestTarget{}
	if result.Branch != "" {
		target = resolvedSaveRequestTarget(saveRequest)
	}

	message := resolveTimeoutMessage
	switch {
	case errors.Is(result.Err, git.ErrBranchWorkerStopped):
		message = git.ErrBranchWorkerStopped.Error()
	case result.Phase == git.PhaseWaitingForWorker:
		message = noWorkerTimeoutMessage
	}
	log.Info("SaveRequest was not acted on within the safety window; withdrawn and failed closed",
		"name", req.NamespacedName, "reason", message)
	r.writeTerminalStatus(ctx, log, saveRequest, git.FinalizeResult{}, errors.New(message), attribution, target)
	return ctrl.Result{}, nil
}

// pollSaveRequest reports where the worker says the request stands and polls again.
func (r *SaveRequestReconciler) pollSaveRequest(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	phase git.SaveRequestPhase,
	held string,
) (ctrl.Result, error) {
	if err := r.recordWorkerPhase(ctx, saveRequest, attribution, phase, held); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: saveRequestPollInterval}, nil
}

// stampFirstSightConditions stamps the still-running conditions the first time a request is seen,
// so the object reports its progress (kstatus InProgress) and AuthorAttributed is settled
// immediately. A disabled controller returns before this, so it never stamps.
func (r *SaveRequestReconciler) stampFirstSightConditions(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
) error {
	if findCondition(saveRequest.Status.Conditions, ConditionTypeReady) != nil {
		return nil
	}
	markSaveRequestProgressing(saveRequest, attribution, "", "")
	if err := r.Status().Update(ctx, saveRequest); err != nil {
		return err
	}
	logf.FromContext(ctx).V(1).Info("Stamped SaveRequest in-progress conditions",
		"name", client.ObjectKeyFromObject(saveRequest))
	return nil
}

// refusePrunedGitTargetRef fails a SaveRequest whose spec names no GitTarget, which can only be
// one thing: an object stored before spec.targetRef was renamed to spec.gitTargetRef, whose old
// value the apiserver stopped serving the moment the new CRDs landed. Admission refuses an empty
// name on every path, so nothing else can produce it.
//
// It is terminal rather than retried, and it has to be, because the object cannot be repaired:
// SaveRequest.spec is wholly immutable, so no apply can put the name back. Retrying would spend
// the controller's attention on an object that will never resolve, and would report a transient
// "get GitTarget" error for a permanent condition. The message names the only fix there is.
func (r *SaveRequestReconciler) refusePrunedGitTargetRef(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
) (bool, error) {
	if saveRequest.Spec.GitTargetRef.Name != "" {
		return false, nil
	}

	// This path reaches a terminal state without going through writeTerminalStatus, so it carries
	// its own increment. It is rare and migration-only, which is exactly the kind of terminal
	// state that must not be missing from the counter. Note the status write below is returned for
	// requeue, so a failing write re-decides and increments again: see the instrument's doc
	// comment on what one increment means.
	recordSaveRequestOutcome(ctx, crOutcomeFailed, saveRequestTarget{})
	failSaveRequest(saveRequest, crReasonGitTargetRefPruned,
		"spec.gitTargetRef is empty: this request was created before spec.targetRef was renamed, "+
			"and its value was pruned by the upgrade. A SaveRequest spec is immutable, so this "+
			"one cannot be repaired — delete it and create a new one.")
	if err := r.Status().Update(ctx, saveRequest); err != nil {
		return true, err
	}
	r.stampDeleteAfter(ctx, saveRequest)
	logf.FromContext(ctx).Info(
		"SaveRequest names no GitTarget: created before the gitTargetRef rename",
		"name", client.ObjectKeyFromObject(saveRequest))
	return true, nil
}

// attributeAuthor settles the commit author with a single synchronous lookup of the
// submitter captured at admission (present-or-never). It never waits: a nil
// AuthorLookup (the validate-operator-types webhook is disabled) or a miss both claim no
// actor immediately. The miss case is final — the record is written before the object is
// visible, so there is no asynchronous arrival to wait for.
//
// The lookup result is logged at Info: it is the counterpart to the admission handler's
// "recorded command author" line, so a hit/miss pair makes the whole capture→read path
// legible (the first thing to check when a request unexpectedly claims no actor).
func (r *SaveRequestReconciler) attributeAuthor(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
) (queue.CommandAuthor, saveRequestAttribution) {
	log := logf.FromContext(ctx).WithName("SaveRequestReconciler")
	if r.AuthorLookup == nil {
		log.Info("command-author lookup disabled (validate-operator-types webhook off); request claims no actor",
			"name", client.ObjectKeyFromObject(saveRequest), "uid", saveRequest.UID)
		// Capture is off, so nothing was attempted — distinct from a capture that ran and
		// found no record, which is what attributionCommitter now means.
		return queue.CommandAuthor{}, attributionNotAttempted
	}
	if author, ok := r.AuthorLookup.LookupCommandAuthor(ctx, saveRequest.UID); ok {
		log.Info("command author resolved from admission record",
			"name", client.ObjectKeyFromObject(saveRequest), "uid", saveRequest.UID, "author", author.Author)
		return author, attributionFromAdmission
	}
	log.Info("no admission command-author record found; request claims no actor",
		"name", client.ObjectKeyFromObject(saveRequest), "uid", saveRequest.UID)
	return queue.CommandAuthor{}, attributionCommitter
}

// recordWorkerPhase reports the phase the worker says the request is in. It writes only on a
// change, so polling does not re-write status every interval. An empty phase — the worker has not
// registered the request yet — keeps whatever is shown, so the controller never reports a phase
// the worker has not confirmed.
func (r *SaveRequestReconciler) recordWorkerPhase(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	phase git.SaveRequestPhase,
	held string,
) error {
	if progressRecorded(saveRequest, phase, held) {
		return nil
	}
	markSaveRequestProgressing(saveRequest, attribution, phase, held)
	return r.Status().Update(ctx, saveRequest)
}

// progressRecorded reports whether the request's status already says this phase, and why it is
// held, so a poll writes only what changed. An empty phase says nothing new.
func progressRecorded(cr *configbutleraiv1alpha3.SaveRequest, phase git.SaveRequestPhase, held string) bool {
	current := findCondition(cr.Status.Conditions, ConditionTypeReconciling)
	if current == nil {
		return false
	}
	if phase == "" {
		return true
	}
	reason, message := progressFor(phase, held)
	return current.Reason == reason && current.Message == message
}

// loadActionableSaveRequest fetches the SaveRequest and short-circuits
// everything that must not reach the finalize: a deleted object, a terminal
// outcome, and — via an uncached re-read — a stale cache echo of our own
// terminal write from a previous invocation (work past this point must happen
// at most once per SaveRequest). done=true means the finalize has nothing
// further to do; the request is returned with it when it is terminal, so its
// expiry can be decided, and nil when it is gone.
func (r *SaveRequestReconciler) loadActionableSaveRequest(
	ctx context.Context,
	req ctrl.Request,
) (*configbutleraiv1alpha3.SaveRequest, bool, error) {
	var saveRequest configbutleraiv1alpha3.SaveRequest
	if err := r.Get(ctx, req.NamespacedName, &saveRequest); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	if saveRequestIsTerminal(&saveRequest) {
		return &saveRequest, true, nil
	}

	if r.APIReader != nil {
		if err := r.APIReader.Get(ctx, req.NamespacedName, &saveRequest); err != nil {
			return nil, true, client.IgnoreNotFound(err)
		}
		if saveRequestIsTerminal(&saveRequest) {
			return &saveRequest, true, nil
		}
	}

	return &saveRequest, false, nil
}

// expireFinished deletes a finished request once the time in its delete-after annotation has
// passed, and otherwise comes back when it will have. No annotation means keep: the TTL flag only
// decides what is WRITTEN at finish, so changing it never reaches back to a request already stamped,
// and a request whose annotation someone removed stays.
//
// The delete is preconditioned on the resourceVersion that was judged, so an edit to the annotation
// that races it fails the delete with a conflict, and the requeue reads the edit. The edit itself
// needs no extra wiring: this controller has no generation filter, so a metadata change reconciles
// the request again.
func (r *SaveRequestReconciler) expireFinished(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
) (ctrl.Result, error) {
	value, ok := saveRequest.Annotations[SaveRequestDeleteAfterAnnotation]
	if !ok || !saveRequest.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithName("SaveRequestReconciler")
	deleteAfter, parsed := parseDeleteAfter(value)
	if !parsed {
		// Kept rather than guessed at: an annotation a person mistyped is not consent to delete.
		log.Info("SaveRequest delete-after annotation is not an RFC 3339 time; keeping the request",
			"name", client.ObjectKeyFromObject(saveRequest), "value", value)
		return ctrl.Result{}, nil
	}

	if remaining := time.Until(deleteAfter); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	uid, resourceVersion := saveRequest.UID, saveRequest.ResourceVersion
	err := r.Delete(ctx, saveRequest, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Deleted finished SaveRequest past its delete-after time",
		"name", client.ObjectKeyFromObject(saveRequest), "deleteAfter", value)
	return ctrl.Result{}, nil
}

// parseDeleteAfter reads the annotation's RFC 3339 time, reporting false for anything else.
func parseDeleteAfter(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, value)
	return t, err == nil
}

// stampDeleteAfter writes the delete-after annotation onto a request that has just finished. It
// runs only on the transition to terminal, never on a later reconcile of a terminal request, which
// is what keeps requests that finished before this existed, and requests someone un-stamped, alone.
//
// A value that is already there wins: a submitter may set its own delete-after at creation, and a
// person may extend it in the moment between the terminal status write and this stamp. So the patch
// carries the resourceVersion it was computed from, and a conflict re-reads the request rather than
// overwriting; if the annotation appeared meanwhile, it is left exactly as it is.
//
// A write that still fails is logged, not requeued: the request is then simply kept, the safe
// direction, and a requeue would re-enter the finalize this controller must run at most once.
func (r *SaveRequestReconciler) stampDeleteAfter(
	ctx context.Context,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
) {
	if r.TTL <= 0 {
		return
	}
	log := logf.FromContext(ctx).WithName("SaveRequestReconciler")
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	deleteAfter := time.Now().Add(r.TTL).UTC().Format(time.RFC3339)

	current := saveRequest
	for attempt := 1; attempt <= saveRequestStatusUpdateAttempts; attempt++ {
		if _, set := current.Annotations[SaveRequestDeleteAfterAnnotation]; set {
			return
		}
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[SaveRequestDeleteAfterAnnotation] = deleteAfter
		err := r.Patch(ctx, current, patch)
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "Failed to stamp delete-after on a finished SaveRequest; it will be kept",
				"name", client.ObjectKeyFromObject(saveRequest))
			return
		}
		var fresh configbutleraiv1alpha3.SaveRequest
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(saveRequest), &fresh); getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				log.Error(getErr, "Failed to re-read SaveRequest to stamp delete-after; it will be kept",
					"name", client.ObjectKeyFromObject(saveRequest))
			}
			return
		}
		if fresh.UID != saveRequest.UID {
			return
		}
		current = &fresh
	}
	log.Error(nil, "Gave up stamping delete-after after repeated conflicts; the request will be kept",
		"name", client.ObjectKeyFromObject(saveRequest))
}

// saveRequestStatusUpdateAttempts bounds the terminal-status conflict retry.
const saveRequestStatusUpdateAttempts = 3

// writeTerminalStatus records the finalize outcome on the SaveRequest,
// retrying on optimistic-concurrency conflicts. Like the audit-consumer path
// it replaces, a permanently failing write is logged and given up on rather
// than returned for requeue: re-running the reconcile would re-finalize an
// already-flushed window and mis-report the outcome as NoOpenWindow.
func (r *SaveRequestReconciler) writeTerminalStatus(
	ctx context.Context,
	log logr.Logger,
	saveRequest *configbutleraiv1alpha3.SaveRequest,
	result git.FinalizeResult,
	finalizeErr error,
	attribution saveRequestAttribution,
	target saveRequestTarget,
) {
	expectedUID := saveRequest.UID
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	// Once, here, and deliberately not inside the loop below: result and finalizeErr are inputs
	// that do not change across attempts, so this is the point the outcome is settled. Recording
	// per attempt would over-count every request that hit a conflict.
	recordSaveRequestOutcome(ctx, saveRequestOutcome(result, finalizeErr), target)

	current := saveRequest
	for attempt := 1; attempt <= saveRequestStatusUpdateAttempts; attempt++ {
		applyFinalizeResultToStatus(current, result, finalizeErr, attribution)

		err := r.Status().Update(ctx, current)
		if err == nil {
			r.stampDeleteAfter(ctx, current)
			finalizeError := ""
			if finalizeErr != nil {
				finalizeError = finalizeErr.Error()
			}
			readyReason, readyMessage := saveRequestReadyReason(current)
			log.Info("SaveRequest finalized",
				"name", client.ObjectKeyFromObject(current),
				"ready", saveRequestConditionStatus(current, ConditionTypeReady),
				"reason", readyReason,
				"message", readyMessage,
				"authorAttributed", saveRequestConditionStatus(current, ConditionTypeAuthorAttributed),
				"pushed", saveRequestConditionStatus(current, ConditionTypePushed),
				"branch", current.Status.Branch,
				"commit", current.Status.Commit,
				"outcome", result.Outcome,
				"finalizeError", finalizeError,
				"gitTarget", current.GitTargetKey().String(),
				"age", time.Since(current.CreationTimestamp.Time).String())
			return
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "Failed to write SaveRequest status")
			return
		}

		log.V(1).Info("Conflict writing SaveRequest status; retrying", "attempt", attempt)
		var fresh configbutleraiv1alpha3.SaveRequest
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(saveRequest), &fresh); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				log.Info("SaveRequest deleted before status could be written; skipping")
				return
			}
			log.Error(getErr, "Failed to re-read SaveRequest for status update")
			return
		}
		// Never stamp the outcome onto a different incarnation or over a
		// terminal outcome another writer got in first.
		if fresh.UID != expectedUID {
			log.Info("SaveRequest UID changed before status could be written; skipping",
				"expectedUID", expectedUID, "objectUID", fresh.UID)
			return
		}
		if saveRequestIsTerminal(&fresh) {
			return
		}
		current = &fresh
	}

	log.Error(nil, "Gave up writing SaveRequest status after repeated conflicts")
}

// SetupWithManager sets up the controller with the Manager.
// MaxConcurrentReconciles is pinned to 1 on purpose: the single worker IS the
// multi-SaveRequest ordering design — concurrent SaveRequests for the same
// GitTarget are serialized exactly as a dedicated finalize-coordinator
// goroutine would serialize them, without the extra moving parts (see
// docs/spec/saverequest-design.md).
//
// Restart recovery is best-effort by design: the
// message is durable in spec.message, so on restart any non-terminal request is
// re-reconciled — author-resolved from the admission cache when present and
// re-attached — which heals the common cases. The one knowingly-accepted gap is a request whose
// commit was already pushed but whose terminal status was not yet written: the
// in-memory outcome is gone, the re-driven attach finds the work already mirrored,
// and it resolves Rejected/AlreadyPresent. We do not build a durable record to
// close that.
func (r *SaveRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.SaveRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("saverequest").
		Complete(r)
}
