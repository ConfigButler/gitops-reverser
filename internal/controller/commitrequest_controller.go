// SPDX-License-Identifier: Apache-2.0

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

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// CommitRequestFinalizer is the EventRouter seam the reconciler drives, using the
// attach-then-poll protocol (docs/spec/commitrequest-design.md):
// ServiceCommitRequest registers the attach idempotently on the GitTarget's branch
// worker (attach it to the author's window with its message and timers)
// and returns the request's current outcome — resolved=false means keep polling.
// watch.EventRouter satisfies it without adaptation.
//
// There is no watermark barrier: the interactive case is covered by the human gap between the
// edit and the save, UC2 by the request's attach wait. The worker anchors that wait at its own
// registration and runs the window's timers, so the controller never holds the finalize itself.
type CommitRequestFinalizer interface {
	ServiceCommitRequest(ctx context.Context, attach git.AttachCommitRequest) (git.FinalizeResult, bool, error)
	// WithdrawCommitRequest cancels a request the worker has not acted on and reports the worker's
	// answer: resolved with git.ErrCommitRequestWithdrawn when it is cancelled, the real outcome
	// when the worker got there first, or resolved=false with the worker's phase while it decides.
	WithdrawCommitRequest(ctx context.Context, attach git.AttachCommitRequest) (git.FinalizeResult, bool, error)
}

// CommandAuthorLookup resolves the author of a CommitRequest from the submitter
// captured at admission by the validate-operator-types webhook, keyed by the persisted
// object's UID. *queue.CommandAuthorStore satisfies it without adaptation. The lookup
// is present-or-never (docs/spec/commitrequest-admission-authorship.md): a miss is
// immediate and final — the webhook is not configured (or a best-effort write missed) —
// and the request claims no actor with no wait.
type CommandAuthorLookup interface {
	LookupCommandAuthor(ctx context.Context, uid types.UID) (queue.CommandAuthor, bool)
}

// windowMismatchMessage explains the author-bound refusal: an open window
// existed but was not this requester's, so it was deliberately left alone.
const windowMismatchMessage = "the open commit window belongs to a different author or GitTarget; " +
	"nothing was committed for this request"

// resolveTimeoutMessage explains the fail-closed resolve bound: the worker never acted on the
// request within the safety window, so it was withdrawn rather than polled forever.
const resolveTimeoutMessage = "the CommitRequest finalize did not resolve within the safety window"

// noWorkerTimeoutMessage is resolveTimeoutMessage for a GitTarget that never started a branch
// worker, the one cause the controller can name without asking anybody.
const noWorkerTimeoutMessage = "the CommitRequest's GitTarget did not start a branch worker within the safety window"

const (
	// commitRequestPollInterval is the requeue cadence while polling the worker
	// for the attached request's outcome (attach-then-poll).
	commitRequestPollInterval = 2 * time.Second

	// commitRequestPushAllowance is what the safety bound allows on top of a request's own
	// timers, for the push cooldown and its retries. Authorship is settled synchronously at first
	// sight, so no attribution wait belongs here.
	commitRequestPushAllowance = 120 * time.Second

	// The request window defaults mirror the +kubebuilder:default markers on CommitRequestWindow.
	// The API server fills them in, so these apply only to a client that bypasses defaulting;
	// resolving to the same values keeps the two paths from disagreeing.
	defaultAttachTimeout = 2 * time.Second
	defaultMaxDuration   = 2 * time.Second
)

// requestWindow is a CommitRequest's window, resolved to the values the worker applies.
type requestWindow struct {
	attach        configbutleraiv1alpha3.AttachPolicy
	attachTimeout time.Duration
	idleTimeout   *time.Duration
	maxDuration   time.Duration
}

// resolveRequestWindow reads spec.window, falling back field by field to the schema defaults. An
// explicit "0s" stays zero, and an omitted idleTimeout stays nil (no idle close).
func resolveRequestWindow(spec configbutleraiv1alpha3.CommitRequestSpec) requestWindow {
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
	return w.attachTimeout + w.maxDuration + commitRequestPushAllowance
}

// CommitRequestReconciler deliberately does NOT use reconcileStatus, which every other controller
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
// CommitRequestReconciler drives a CommitRequest through its state machine
// (docs/spec/commitrequest-design.md and
// docs/spec/commitrequest-admission-authorship.md):
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
type CommitRequestReconciler struct {
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
	Finalizer    CommitRequestFinalizer
	AuthorLookup CommandAuthorLookup

	// TTL is how long a request is kept once it finishes. It is written onto the request as
	// CommitRequestDeleteAfterAnnotation at that moment; zero writes nothing, so the request stays.
	TTL time.Duration
}

// CommitRequestDeleteAfterAnnotation carries the RFC 3339 time after which the controller deletes a
// finished CommitRequest. The controller writes it once, when the request finishes, and from then
// on the annotation alone decides: remove it to keep the request, or edit it to move the deletion.
// A request that finished before this annotation existed carries none and is never deleted.
const CommitRequestDeleteAfterAnnotation = "configbutler.ai/delete-after"

// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests/status,verbs=get;update;patch

// Reconcile advances one CommitRequest through attribute → attach + poll →
// terminal status. With MaxConcurrentReconciles=1 concurrent CommitRequests are
// serialized by construction, and the worker keys attaches by request identity so
// re-sends across poll requeues are idempotent.
func (r *CommitRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")

	commitRequest, done, err := r.loadActionableCommitRequest(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if done {
		if commitRequest != nil {
			return r.expireFinished(ctx, commitRequest)
		}
		return ctrl.Result{}, nil
	}

	if r.Finalizer == nil {
		log.V(1).Info("CommitRequest finalize disabled: no Finalizer configured",
			"name", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if handled, err := r.refusePrunedGitTargetRef(ctx, commitRequest); handled || err != nil {
		return ctrl.Result{}, err
	}

	// 1. ATTRIBUTE: settle the request's actor synchronously (present-or-never).
	// A hit names the admission submitter; a miss claims no actor. Either way the
	// decision is final — there is no wait and no requeue for the author.
	author, attribution := r.attributeAuthor(ctx, commitRequest)

	if err := r.stampFirstSightConditions(ctx, commitRequest, attribution); err != nil {
		return ctrl.Result{}, err
	}

	// The schema rejects an invalid literal message at admission, so a rejection here names an
	// object the schema could not have seen: one stored before the rule landed. It is terminal
	// rather than retried, because CommitRequest.spec is immutable and no apply can repair it.
	if err := git.ValidateLiteralCommitMessage(commitRequest.Spec.Message); err != nil {
		// The fallback identity: this fails before ServiceCommitRequest, so nothing has
		// confirmed the named GitTarget exists.
		r.writeTerminalStatus(ctx, log, commitRequest, git.FinalizeResult{}, err, attribution,
			commitRequestTarget{})
		return ctrl.Result{}, nil
	}

	// 2. ATTACH + POLL: register the attach idempotently the instant we attribute
	// (no controller-side delay — the worker anchors the wait at its registration)
	// and poll the outcome.
	window := resolveRequestWindow(commitRequest.Spec)
	attach := git.AttachCommitRequest{
		Namespace:          commitRequest.Namespace,
		Name:               commitRequest.Name,
		UID:                string(commitRequest.UID),
		Author:             author.Author,
		Attribution:        attribution.gitOutcome(),
		GitTargetName:      commitRequest.Spec.GitTargetRef.Name,
		GitTargetNamespace: commitRequest.Namespace,
		Message:            commitRequest.Spec.Message,
		Attach:             window.attach,
		AttachTimeout:      window.attachTimeout,
		IdleTimeout:        window.idleTimeout,
		MaxDuration:        window.maxDuration,
		CommitEmpty:        commitRequest.Spec.WhenNothingToCommit == configbutleraiv1alpha3.NothingToCommitCommitEmpty,
	}
	if time.Since(commitRequest.CreationTimestamp.Time) >= window.resolveTimeout() {
		// Past the bound the controller only withdraws: an attach sent now would sit behind the
		// withdraw on the worker's FIFO and could register the request after it was cancelled.
		return r.withdrawCommitRequest(ctx, log, req, commitRequest, attribution, attach)
	}
	return r.attachCommitRequest(ctx, log, req, commitRequest, attribution, attach)
}

// attachCommitRequest sends the attach and reports the outcome once the worker has one. Until then
// it records the worker's phase and polls; a service error is a not-yet-serviceable worker, so it
// polls on the same terms.
func (r *CommitRequestReconciler) attachCommitRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	attribution commitRequestAttribution,
	attach git.AttachCommitRequest,
) (ctrl.Result, error) {
	result, resolved, serviceErr := r.Finalizer.ServiceCommitRequest(ctx, attach)
	if serviceErr != nil || !resolved {
		if serviceErr != nil {
			log.V(1).Info("CommitRequest attach not yet serviceable; will retry",
				"name", req.NamespacedName, "err", serviceErr.Error())
		}
		return r.pollCommitRequest(ctx, commitRequest, attribution, result.Phase)
	}

	if result.Err != nil {
		log.Error(result.Err, "CommitRequest finalize failed",
			"gitTarget", commitRequest.Spec.GitTargetRef.Name, "name", req.NamespacedName)
	}
	r.writeTerminalStatus(ctx, log, commitRequest, result, result.Err, attribution,
		resolvedCommitRequestTarget(commitRequest))
	return ctrl.Result{}, nil
}

// withdrawCommitRequest handles a request past its safety window. A request the worker holds keeps
// polling with no bound of its own: only the worker can say how it ends. Any other request is
// withdrawn, and fails only on the worker's answer, so a request reported failed can never be
// committed afterwards. A withdraw that cannot be serviced yet polls on the same terms.
func (r *CommitRequestReconciler) withdrawCommitRequest(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	attribution commitRequestAttribution,
	attach git.AttachCommitRequest,
) (ctrl.Result, error) {
	result, resolved, withdrawErr := r.Finalizer.WithdrawCommitRequest(ctx, attach)
	if withdrawErr != nil || !resolved {
		if withdrawErr != nil {
			log.V(1).Info("CommitRequest withdraw not yet serviceable; will retry",
				"name", req.NamespacedName, "err", withdrawErr.Error())
		}
		// Not answered yet, or the worker holds the request: keep polling.
		return r.pollCommitRequest(ctx, commitRequest, attribution, result.Phase)
	}

	if !errors.Is(result.Err, git.ErrCommitRequestWithdrawn) {
		// The worker resolved it on its own before the withdraw: report that outcome, as the
		// attach path would have.
		r.writeTerminalStatus(ctx, log, commitRequest, result, result.Err, attribution,
			resolvedCommitRequestTarget(commitRequest))
		return ctrl.Result{}, nil
	}

	// Withdrawn. A worker's answer, or a GitTarget that was read, carries the branch, and only
	// then is the request's identity known.
	target := commitRequestTarget{}
	if result.Branch != "" {
		target = resolvedCommitRequestTarget(commitRequest)
	}

	message := resolveTimeoutMessage
	switch {
	case errors.Is(result.Err, git.ErrBranchWorkerStopped):
		message = git.ErrBranchWorkerStopped.Error()
	case result.Phase == git.PhaseWaitingForWorker:
		message = noWorkerTimeoutMessage
	}
	log.Info("CommitRequest was not acted on within the safety window; withdrawn and failed closed",
		"name", req.NamespacedName, "reason", message)
	r.writeTerminalStatus(ctx, log, commitRequest, git.FinalizeResult{}, errors.New(message), attribution, target)
	return ctrl.Result{}, nil
}

// pollCommitRequest reports where the worker says the request stands and polls again.
func (r *CommitRequestReconciler) pollCommitRequest(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	attribution commitRequestAttribution,
	phase git.CommitRequestPhase,
) (ctrl.Result, error) {
	if err := r.recordWorkerPhase(ctx, commitRequest, attribution, phase); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: commitRequestPollInterval}, nil
}

// stampFirstSightConditions stamps the still-running conditions the first time a request is seen,
// so the object reports its progress (kstatus InProgress) and AuthorAttributed is settled
// immediately. A disabled controller returns before this, so it never stamps.
func (r *CommitRequestReconciler) stampFirstSightConditions(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	attribution commitRequestAttribution,
) error {
	if findCondition(commitRequest.Status.Conditions, ConditionTypeReady) != nil {
		return nil
	}
	markCommitRequestProgressing(commitRequest, attribution, "")
	if err := r.Status().Update(ctx, commitRequest); err != nil {
		return err
	}
	logf.FromContext(ctx).V(1).Info("Stamped CommitRequest in-progress conditions",
		"name", client.ObjectKeyFromObject(commitRequest))
	return nil
}

// refusePrunedGitTargetRef fails a CommitRequest whose spec names no GitTarget, which can only be
// one thing: an object stored before spec.targetRef was renamed to spec.gitTargetRef, whose old
// value the apiserver stopped serving the moment the new CRDs landed. Admission refuses an empty
// name on every path, so nothing else can produce it.
//
// It is terminal rather than retried, and it has to be, because the object cannot be repaired:
// CommitRequest.spec is wholly immutable, so no apply can put the name back. Retrying would spend
// the controller's attention on an object that will never resolve, and would report a transient
// "get GitTarget" error for a permanent condition. The message names the only fix there is.
func (r *CommitRequestReconciler) refusePrunedGitTargetRef(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
) (bool, error) {
	if commitRequest.Spec.GitTargetRef.Name != "" {
		return false, nil
	}

	// This path reaches a terminal state without going through writeTerminalStatus, so it carries
	// its own increment. It is rare and migration-only, which is exactly the kind of terminal
	// state that must not be missing from the counter. Note the status write below is returned for
	// requeue, so a failing write re-decides and increments again: see the instrument's doc
	// comment on what one increment means.
	recordCommitRequestOutcome(ctx, crOutcomeFailed, commitRequestTarget{})
	failCommitRequest(commitRequest, crReasonGitTargetRefPruned,
		"spec.gitTargetRef is empty: this request was created before spec.targetRef was renamed, "+
			"and its value was pruned by the upgrade. A CommitRequest spec is immutable, so this "+
			"one cannot be repaired — delete it and create a new one.")
	if err := r.Status().Update(ctx, commitRequest); err != nil {
		return true, err
	}
	r.stampDeleteAfter(ctx, commitRequest)
	logf.FromContext(ctx).Info(
		"CommitRequest names no GitTarget: created before the gitTargetRef rename",
		"name", client.ObjectKeyFromObject(commitRequest))
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
func (r *CommitRequestReconciler) attributeAuthor(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
) (queue.CommandAuthor, commitRequestAttribution) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")
	if r.AuthorLookup == nil {
		log.Info("command-author lookup disabled (validate-operator-types webhook off); request claims no actor",
			"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID)
		// Capture is off, so nothing was attempted — distinct from a capture that ran and
		// found no record, which is what attributionCommitter now means.
		return queue.CommandAuthor{}, attributionNotAttempted
	}
	if author, ok := r.AuthorLookup.LookupCommandAuthor(ctx, commitRequest.UID); ok {
		log.Info("command author resolved from admission record",
			"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID, "author", author.Author)
		return author, attributionFromAdmission
	}
	log.Info("no admission command-author record found; request claims no actor",
		"name", client.ObjectKeyFromObject(commitRequest), "uid", commitRequest.UID)
	return queue.CommandAuthor{}, attributionCommitter
}

// recordWorkerPhase reports the phase the worker says the request is in. It writes only on a
// change, so polling does not re-write status every interval. An empty phase — the worker has not
// registered the request yet — keeps whatever is shown, so the controller never reports a phase
// the worker has not confirmed.
func (r *CommitRequestReconciler) recordWorkerPhase(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	attribution commitRequestAttribution,
	phase git.CommitRequestPhase,
) error {
	current := findCondition(commitRequest.Status.Conditions, ConditionTypeReconciling)
	if current != nil && (phase == "" || current.Reason == string(phase)) {
		return nil
	}
	markCommitRequestProgressing(commitRequest, attribution, phase)
	return r.Status().Update(ctx, commitRequest)
}

// loadActionableCommitRequest fetches the CommitRequest and short-circuits
// everything that must not reach the finalize: a deleted object, a terminal
// outcome, and — via an uncached re-read — a stale cache echo of our own
// terminal write from a previous invocation (work past this point must happen
// at most once per CommitRequest). done=true means the finalize has nothing
// further to do; the request is returned with it when it is terminal, so its
// expiry can be decided, and nil when it is gone.
func (r *CommitRequestReconciler) loadActionableCommitRequest(
	ctx context.Context,
	req ctrl.Request,
) (*configbutleraiv1alpha3.CommitRequest, bool, error) {
	var commitRequest configbutleraiv1alpha3.CommitRequest
	if err := r.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
		return nil, true, client.IgnoreNotFound(err)
	}
	if commitRequestIsTerminal(&commitRequest) {
		return &commitRequest, true, nil
	}

	if r.APIReader != nil {
		if err := r.APIReader.Get(ctx, req.NamespacedName, &commitRequest); err != nil {
			return nil, true, client.IgnoreNotFound(err)
		}
		if commitRequestIsTerminal(&commitRequest) {
			return &commitRequest, true, nil
		}
	}

	return &commitRequest, false, nil
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
func (r *CommitRequestReconciler) expireFinished(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
) (ctrl.Result, error) {
	value, ok := commitRequest.Annotations[CommitRequestDeleteAfterAnnotation]
	if !ok || !commitRequest.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")
	deleteAfter, parsed := parseDeleteAfter(value)
	if !parsed {
		// Kept rather than guessed at: an annotation a person mistyped is not consent to delete.
		log.Info("CommitRequest delete-after annotation is not an RFC 3339 time; keeping the request",
			"name", client.ObjectKeyFromObject(commitRequest), "value", value)
		return ctrl.Result{}, nil
	}

	if remaining := time.Until(deleteAfter); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	uid, resourceVersion := commitRequest.UID, commitRequest.ResourceVersion
	err := r.Delete(ctx, commitRequest, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Deleted finished CommitRequest past its delete-after time",
		"name", client.ObjectKeyFromObject(commitRequest), "deleteAfter", value)
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
func (r *CommitRequestReconciler) stampDeleteAfter(
	ctx context.Context,
	commitRequest *configbutleraiv1alpha3.CommitRequest,
) {
	if r.TTL <= 0 {
		return
	}
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	deleteAfter := time.Now().Add(r.TTL).UTC().Format(time.RFC3339)

	current := commitRequest
	for attempt := 1; attempt <= commitRequestStatusUpdateAttempts; attempt++ {
		if _, set := current.Annotations[CommitRequestDeleteAfterAnnotation]; set {
			return
		}
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[CommitRequestDeleteAfterAnnotation] = deleteAfter
		err := r.Patch(ctx, current, patch)
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		if !apierrors.IsConflict(err) {
			log.Error(err, "Failed to stamp delete-after on a finished CommitRequest; it will be kept",
				"name", client.ObjectKeyFromObject(commitRequest))
			return
		}
		var fresh configbutleraiv1alpha3.CommitRequest
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(commitRequest), &fresh); getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				log.Error(getErr, "Failed to re-read CommitRequest to stamp delete-after; it will be kept",
					"name", client.ObjectKeyFromObject(commitRequest))
			}
			return
		}
		if fresh.UID != commitRequest.UID {
			return
		}
		current = &fresh
	}
	log.Error(nil, "Gave up stamping delete-after after repeated conflicts; the request will be kept",
		"name", client.ObjectKeyFromObject(commitRequest))
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
	commitRequest *configbutleraiv1alpha3.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
	attribution commitRequestAttribution,
	target commitRequestTarget,
) {
	expectedUID := commitRequest.UID
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	// Once, here, and deliberately not inside the loop below: result and finalizeErr are inputs
	// that do not change across attempts, so this is the point the outcome is settled. Recording
	// per attempt would over-count every request that hit a conflict.
	recordCommitRequestOutcome(ctx, commitRequestOutcome(result, finalizeErr), target)

	current := commitRequest
	for attempt := 1; attempt <= commitRequestStatusUpdateAttempts; attempt++ {
		applyFinalizeResultToStatus(current, result, finalizeErr, attribution)

		err := r.Status().Update(ctx, current)
		if err == nil {
			r.stampDeleteAfter(ctx, current)
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
				"commit", current.Status.Commit,
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
		var fresh configbutleraiv1alpha3.CommitRequest
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
// docs/spec/commitrequest-design.md).
//
// Restart recovery is best-effort by design: the
// message is durable in spec.message, so on restart any non-terminal request is
// re-reconciled — author-resolved from the admission cache when present and
// re-attached — which heals the common cases. The one knowingly-accepted gap is a request whose
// commit was already pushed but whose terminal status was not yet written: the
// in-memory outcome is gone, the re-driven attach finds the work already mirrored,
// and it resolves Rejected/AlreadyPresent. We do not build a durable record to
// close that.
func (r *CommitRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.CommitRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("commitrequest").
		Complete(r)
}
