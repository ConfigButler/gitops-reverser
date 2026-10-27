// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
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
	// LookupCommitRequestApply reports where the apply a request with catchUp:
	// AfterPush asked for stands, and false while the worker knows nothing of it.
	LookupCommitRequestApply(ctx context.Context, attach git.AttachCommitRequest) (git.SaveApply, bool, error)
}

// windowMismatchMessage explains the author-bound refusal: an open window
// existed but was not this requester's, so it was deliberately left alone.
const saveInProgressMessage = "an earlier save of the same author on this GitTarget was still waiting for " +
	"or collecting a commit window, which carries one message; save again once it resolved"

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
	// timers: the wait for its author, the push cooldown and its retries.
	commitRequestPushAllowance = 120 * time.Second

	// The request window defaults mirror the +kubebuilder:default markers on CommitRequestWindow.
	// The API server fills them in, so these apply only to a client that bypasses defaulting;
	// resolving to the same values keeps the two paths from disagreeing.
	defaultAttachTimeout = 5 * time.Second
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

// progressRecorded reports whether the request's status already says this phase, and why it is
// held, so a poll writes only what changed. An empty phase says nothing new.
func progressRecorded(cr *configbutleraiv1alpha3.CommitRequest, phase git.CommitRequestPhase, held string) bool {
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

// CommitRequestReconciler is the control-cluster path of a CommitRequest: it picks up the requests
// on the cluster GitOps Reverser runs in, with the operator's own client, and hands each step to
// commitRequestCore, which serves both paths.
//
// It reads through APIReader (uncached), so a cache echo of its own status write can never re-drive a
// step that already happened: every step decides from the object as it is stored. Its For() carries
// no GenerationChangedPredicate, so a status or metadata write, such as an edit of the delete-after
// annotation, reconciles the request again.
type CommitRequestReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// APIReader performs uncached reads of the requests. Nil falls back to the (cached) Client.
	APIReader client.Reader

	// Finalizer attaches the request to its author's window and reports its outcome. Nil leaves
	// every request alone.
	Finalizer CommitRequestFinalizer

	// Authors finds a request's creator from audit facts, as a watched write's author is. Nil means
	// author attribution is off: a request then names no author, and nothing waits for one.
	Authors watch.AuthorResolver
	// FactStreams is the set the attribution fact follower reads; the reconciler adds the stream of
	// commitrequests on each audit route a creator is looked up on. Nil with attribution off.
	FactStreams *queue.FactStreamSet

	// TTL is how long a request is kept once it finishes. It is written onto the request as
	// CommitRequestDeleteAfterAnnotation at that moment; zero writes nothing, so the request stays.
	TTL time.Duration

	streamsOnce sync.Once
	streams     *commitRequestStreams
}

// CommitRequestDeleteAfterAnnotation carries the RFC 3339 time after which a finished CommitRequest is
// deleted. It is written once, when the request finishes, and from then on the annotation alone
// decides: remove it to keep the request, or edit it to move the deletion. A request that finished
// before this annotation existed carries none and is never deleted.
const CommitRequestDeleteAfterAnnotation = "configbutler.ai/delete-after"

// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=commitrequests/status,verbs=get;update;patch

// Reconcile takes one step of a CommitRequest on the control cluster.
func (r *CommitRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	after, err := r.core().serve(ctx, r.path(ctx), req.NamespacedName)
	return ctrl.Result{RequeueAfter: after}, err
}

// core is the shared core over this reconciler's clients.
func (r *CommitRequestReconciler) core() *commitRequestCore {
	r.streamsOnce.Do(func() { r.streams = &commitRequestStreams{set: r.FactStreams} })
	return &commitRequestCore{
		reader: r.Client, finalizer: r.Finalizer, authors: r.Authors, streams: r.streams, ttl: r.TTL,
	}
}

// path is the control-cluster path: the operator's own client, and finished requests are deleted.
func (r *CommitRequestReconciler) path(ctx context.Context) commitRequestPath {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	return commitRequestPath{
		store:   typedCommitRequestStore{reader: reader, writer: r.Client},
		deletes: true,
		log:     logf.FromContext(ctx).WithName("CommitRequestReconciler"),
	}
}

// typedCommitRequestStore reads and writes CommitRequests through a controller-runtime client. An
// Update carries the object's resourceVersion, so every write it makes is conditional on it.
type typedCommitRequestStore struct {
	reader client.Reader
	writer client.Client
}

func (s typedCommitRequestStore) get(
	ctx context.Context, key types.NamespacedName,
) (*configbutleraiv1alpha3.CommitRequest, error) {
	var cr configbutleraiv1alpha3.CommitRequest
	if err := s.reader.Get(ctx, key, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

func (s typedCommitRequestStore) updateStatus(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error {
	return s.writer.Status().Update(ctx, cr)
}

func (s typedCommitRequestStore) annotate(
	ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest, key, value string,
) error {
	patch := client.MergeFromWithOptions(cr.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[key] = value
	return s.writer.Patch(ctx, cr, patch)
}

func (s typedCommitRequestStore) remove(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error {
	uid, resourceVersion := cr.UID, cr.ResourceVersion
	return s.writer.Delete(ctx, cr, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
}

// SetupWithManager sets up the controller with the Manager. Each step decides from the stored object
// and the worker keys attaches by request identity, so steps of different requests may run at once;
// a step can wait out the attribution grace for its request's author, so they must, or one request's
// wait would hold up every other.
func (r *CommitRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.CommitRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: commitRequestConcurrency}).
		Named("commitrequest").
		Complete(r)
}

// commitRequestConcurrency is how many CommitRequest steps run at once.
const commitRequestConcurrency = 4
