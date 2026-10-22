// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// GitBranchFinalizer holds a GitBranch until its branch worker has given every pending write and
// CommitRequest an outcome. It never deletes the branch on the remote.
const GitBranchFinalizer = "configbutler.ai/git-branch"

const (
	// GitBranchReasonGitProviderNotFound is the stall for a GitBranch whose GitProvider does not
	// exist in its namespace.
	GitBranchReasonGitProviderNotFound = "GitProviderNotFound"
	// GitBranchReasonGitProviderNotReady is the progress reason while the GitProvider is not Ready.
	GitBranchReasonGitProviderNotReady = "GitProviderNotReady"
	// GitBranchReasonBranchNotAllowed is the stall for a branch outside the GitProvider's
	// allowedBranches.
	GitBranchReasonBranchNotAllowed = "BranchNotAllowed"
	// GitBranchReasonPushSessionFailed is the progress reason while the credential cannot open a
	// push (receive-pack) session on the remote. A GitBranch that holds its branch keeps it.
	GitBranchReasonPushSessionFailed = "PushSessionFailed"
	// GitBranchReasonBranchConflict is the stall for a GitBranch whose repository branch another,
	// older GitBranch holds.
	GitBranchReasonBranchConflict = "BranchConflict"
	// GitBranchReasonParentBranchNotFound is the stall for a branch that cannot be created: the
	// remote carries neither it nor the parent it would be created from.
	GitBranchReasonParentBranchNotFound = "ParentBranchNotFound"
	// GitBranchReasonRecoveringParentBranch is progress while the parent branch has been found again
	// but the branch worker has not yet published the writes it kept while the parent was missing.
	GitBranchReasonRecoveringParentBranch = "RecoveringParentBranch"
)

// proofReuse is how long a successful push-session proof stands before the next reconcile proves
// the credential again. The branch worker wakes its GitBranch on every push, and proving on each of
// those would cost a round trip per push; the steady interval is how stale any other reconcile-time
// fact here is allowed to be.
const proofReuse = RequeueSteadyInterval

// GitBranchReconciler reconciles a GitBranch: it proves the credential, keeps the claim on the
// repository branch (git.BranchClaims), and publishes where the branch is on the remote.
type GitBranchReconciler struct {
	client.Client

	Scheme        *runtime.Scheme
	WorkerManager *git.WorkerManager
	// Recorder emits a Kubernetes Event on every persisted Ready transition. It may be nil in tests.
	Recorder record.EventRecorder

	// SSHHostKeys and CredentialPolicy resolve the GitProvider's credential exactly as its branch
	// worker does, so the credential proved is the one that will push.
	SSHHostKeys      git.SSHHostKeyConfig
	CredentialPolicy git.CredentialTransportPolicy

	// proveProvider stands in for opening a push session with a GitProvider's credential. Nil
	// means the real one; a test sets it.
	proveProvider func(provider *configbutleraiv1alpha3.GitProvider) error

	// proofs is the last proof per GitBranch UID, so a reconcile woken by a push does not prove the
	// credential again. Guarded by proofsMu.
	proofsMu sync.Mutex
	proofs   map[k8stypes.UID]proofRecord

	// remotePublications is what each GitBranch last wrote to status.remote. See
	// remotePublicationLedger.
	remotePublications remotePublicationLedger

	// clock is the source of "now", so a test can hold time still. Nil means time.Now.
	clock func() time.Time
}

// proofRecord is one proof's outcome, and the GitProvider object it was made with.
type proofRecord struct {
	at       time.Time
	provider k8stypes.UID
	err      error
}

func (r *GitBranchReconciler) clockNow() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitbranches,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitbranches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitbranches/finalizers,verbs=update

// newGitBranchReadiness starts the accumulator that owns this kind's kstatus trio.
func newGitBranchReadiness(key git.BranchKey) *readiness {
	return newReadiness(fmt.Sprintf("GitBranch holds %s", key), "GitBranch is not stalled")
}

// Reconcile proves the GitBranch's credential, keeps its claim, and publishes its status.
func (r *GitBranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var branch configbutleraiv1alpha3.GitBranch
	if err := r.Get(ctx, req.NamespacedName, &branch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !branch.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &branch)
	}

	st := beginStatus(r.Client, r.Recorder, &branch)
	if err := r.ensureFinalizer(ctx, st, &branch); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.countGitTargets(ctx, &branch); err != nil {
		return ctrl.Result{}, err
	}

	rd, heldFor, err := r.evaluate(ctx, st, &branch)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	cadence := RequeueStreamSettleInterval
	if rd.converged() {
		cadence = RequeueSteadyInterval
	}
	if heldFor > 0 && heldFor < cadence {
		cadence = heldFor
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(cadence)}, nil
}

// evaluate runs the gates in order and returns the verdict, and how long the publication floor
// holds back an answer about the remote.
func (r *GitBranchReconciler) evaluate(
	ctx context.Context,
	st *reconcileStatus,
	branch *configbutleraiv1alpha3.GitBranch,
) (*readiness, time.Duration, error) {
	var provider configbutleraiv1alpha3.GitProvider
	if err := r.Get(ctx, branch.GitProviderKey(), &provider); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, 0, err
		}
		// The holder keeps its claim (rule 3): the claim is left as it is, so its worker keeps the
		// writes it holds until the GitProvider is back or the GitBranch is deleted.
		rd := newReadiness("", "GitBranch is not stalled")
		rd.stalled(GitBranchReasonGitProviderNotFound,
			fmt.Sprintf("Referenced GitProvider '%s' not found", branch.GitProviderKey()))
		return rd, 0, nil
	}

	key := git.BranchKeyFor(provider.Spec.URL, branch.Spec.Branch)
	repository := repourl.Normalize(provider.Spec.URL)
	if branch.Status.Repository != "" && branch.Status.Repository != repository {
		// The GitProvider was recreated for another repository: a delivery for the one it left says
		// nothing about this one.
		branch.Status.LastPushNoticeAt = nil
	}
	branch.Status.Repository = repository
	rd := newGitBranchReadiness(key)
	claimant := git.BranchClaimant{
		GitBranch: client.ObjectKeyFromObject(branch),
		UID:       branch.UID,
		Created:   branch.CreationTimestamp.Time,
	}

	if !branchAllowed(provider.Spec.AllowedBranches, branch.Spec.Branch) {
		r.observeClaim(key, claimant, false)
		rd.stalled(GitBranchReasonBranchNotAllowed, fmt.Sprintf(
			"Branch '%s' does not match any pattern in allowedBranches list %v of GitProvider '%s'",
			branch.Spec.Branch, provider.Spec.AllowedBranches, branch.GitProviderKey()))
		return rd, 0, nil
	}

	proofErr := r.proveCredential(ctx, branch, &provider)
	holder, held := r.observeClaim(key, claimant, proofErr == nil)
	if held && holder.UID != branch.UID {
		rd.stalled(GitBranchReasonBranchConflict, fmt.Sprintf(
			"GitBranch '%s' holds repository %s, branch '%s'. Only one GitBranch writes a repository "+
				"branch; reference that one from your GitTargets, or delete it first",
			holder.GitBranch, key.Repository, key.Branch))
		return rd, 0, nil
	}
	if proofErr != nil {
		message := fmt.Sprintf("The credential of GitProvider '%s' cannot open a push session on the remote: %v",
			branch.GitProviderKey(), proofErr)
		if held {
			message += "; this GitBranch keeps the branch and its pending writes until it can"
		}
		rd.progressing(metav1.ConditionFalse, GitBranchReasonPushSessionFailed, message)
	}
	rd.progressingIf(!conditionIsTrue(provider.Status.Conditions, ConditionTypeReady), metav1.ConditionFalse,
		GitBranchReasonGitProviderNotReady, fmt.Sprintf("Referenced GitProvider '%s' is not Ready",
			branch.GitProviderKey()))

	if !held {
		return rd, 0, nil
	}
	heldFor := r.observeWorker(st, rd, branch, key, &provider)
	return rd, heldFor, nil
}

// observeWorker publishes what the holder's branch worker last proved about the remote, and the
// gates that come from it: a parent branch that is missing, and work held back while it was.
func (r *GitBranchReconciler) observeWorker(
	st *reconcileStatus,
	rd *readiness,
	branch *configbutleraiv1alpha3.GitBranch,
	key git.BranchKey,
	provider *configbutleraiv1alpha3.GitProvider,
) time.Duration {
	repo := repoIdentityOf(provider)
	observed, seen := r.observeRemote(key)
	now := r.clockNow()
	heldFor := r.publishRemote(st, branch, observed, seen, repo, now)
	if noticeHeldFor := publishPushNotice(branch, r.lastPushNotice(branch), now); noticeHeldFor > 0 &&
		(heldFor == 0 || noticeHeldFor < heldFor) {
		heldFor = noticeHeldFor
	}

	if seen && observed.ParentState == git.ParentMissing &&
		observed.ParentRequested == branch.Spec.ParentBranch && !differentRepository(observed.Repo, repo) {
		rd.stalled(GitBranchReasonParentBranchNotFound, parentBranchNotFoundMessage(branch, observed.ParentBranch))
	}
	if r.WorkerManager == nil {
		return heldFor
	}
	worker, live := r.WorkerManager.WorkerForGitBranch(client.ObjectKeyFromObject(branch))
	if !live {
		return heldFor
	}
	open, found := worker.ParentRecovery()
	rd.progressingIf(open && found, metav1.ConditionFalse, GitBranchReasonRecoveringParentBranch,
		"The parent branch is on the remote again; the branch worker is publishing the work it held back")
	return heldFor
}

// lastPushNotice is when a Git host last reported a push to the branch this GitBranch holds, zero
// when its worker has seen none.
func (r *GitBranchReconciler) lastPushNotice(branch *configbutleraiv1alpha3.GitBranch) time.Time {
	if r.WorkerManager == nil {
		return time.Time{}
	}
	worker, live := r.WorkerManager.WorkerForGitBranch(client.ObjectKeyFromObject(branch))
	if !live {
		return time.Time{}
	}
	return worker.LastPushNotice()
}

// observeClaim records the proof with the claim registry and returns the holder.
func (r *GitBranchReconciler) observeClaim(
	key git.BranchKey, claimant git.BranchClaimant, proven bool,
) (git.BranchClaimant, bool) {
	if r.WorkerManager == nil {
		return git.BranchClaimant{}, false
	}
	return r.WorkerManager.Claims().Observe(key, claimant, proven)
}

// proveCredential opens and closes a receive-pack session with the GitProvider's credential. A
// success stands for proofReuse, for the same GitProvider object; a failure is retried on every
// reconcile.
func (r *GitBranchReconciler) proveCredential(
	ctx context.Context,
	branch *configbutleraiv1alpha3.GitBranch,
	provider *configbutleraiv1alpha3.GitProvider,
) error {
	now := r.clockNow()
	r.proofsMu.Lock()
	last, proved := r.proofs[branch.UID]
	r.proofsMu.Unlock()
	if proved && last.err == nil && last.provider == provider.UID && now.Sub(last.at) < proofReuse {
		return nil
	}

	err := r.openPushSession(ctx, provider)
	r.proofsMu.Lock()
	if r.proofs == nil {
		r.proofs = map[k8stypes.UID]proofRecord{}
	}
	r.proofs[branch.UID] = proofRecord{at: now, provider: provider.UID, err: err}
	r.proofsMu.Unlock()
	return err
}

func (r *GitBranchReconciler) openPushSession(
	ctx context.Context,
	provider *configbutleraiv1alpha3.GitProvider,
) error {
	if r.proveProvider != nil {
		return r.proveProvider(provider)
	}
	auth, err := git.AuthForProvider(ctx, r.Client, provider, r.SSHHostKeys, r.CredentialPolicy)
	if err != nil {
		return err
	}
	return git.ProvePushSession(ctx, provider.Spec.URL, auth)
}

// countGitTargets publishes status.gitTargetCount: the live GitTargets, from any namespace, that
// reference this GitBranch. It is read from the cache.
func (r *GitBranchReconciler) countGitTargets(ctx context.Context, branch *configbutleraiv1alpha3.GitBranch) error {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		return err
	}
	self := client.ObjectKeyFromObject(branch)
	var count int32
	for i := range targets.Items {
		if targets.Items[i].DeletionTimestamp.IsZero() && targets.Items[i].GitBranchKey() == self {
			count++
		}
	}
	branch.Status.GitTargetCount = count
	return nil
}

// ensureFinalizer puts the finalizer on a GitBranch before it can hold a branch, and carries the
// metadata that write left into the status session.
func (r *GitBranchReconciler) ensureFinalizer(
	ctx context.Context, st *reconcileStatus, branch *configbutleraiv1alpha3.GitBranch,
) error {
	update := branch.DeepCopy()
	if !controllerutil.AddFinalizer(update, GitBranchFinalizer) {
		return nil
	}
	if err := r.Update(ctx, update); err != nil {
		return err
	}
	branch.SetFinalizers(update.GetFinalizers())
	branch.SetResourceVersion(update.GetResourceVersion())
	st.metadataWritten()
	return nil
}

// finalize gives up the branch. The claim goes first, so no GitTarget reconcile can start this
// GitBranch's worker again; then the worker is stopped, which closes its intake, pushes or refuses
// every pending write and settles every CommitRequest it holds (BranchWorker.Stop). Only then is
// the finalizer removed. Nothing deletes the branch on the remote.
func (r *GitBranchReconciler) finalize(ctx context.Context, branch *configbutleraiv1alpha3.GitBranch) error {
	if !controllerutil.ContainsFinalizer(branch, GitBranchFinalizer) {
		return nil
	}
	if r.WorkerManager != nil {
		claims := r.WorkerManager.Claims()
		key, _, held := claims.HeldBy(client.ObjectKeyFromObject(branch))
		claims.Release(branch.UID)
		r.WorkerManager.RetireGitBranch(client.ObjectKeyFromObject(branch))
		// The next candidate holds the branch now. It may be a GitBranch stalled on BranchConflict,
		// which nothing else would reconcile before its steady requeue.
		if next, taken := claims.Holder(key); held && taken {
			r.WorkerManager.WakeGitBranch(next.GitBranch)
		}
	}
	r.proofsMu.Lock()
	delete(r.proofs, branch.UID)
	r.proofsMu.Unlock()
	r.remotePublications.forget(gitBranchRef(branch))
	telemetry.ForgetResourceConditions(conditionKindGitBranch, branch.Namespace, branch.Name)

	update := branch.DeepCopy()
	controllerutil.RemoveFinalizer(update, GitBranchFinalizer)
	return client.IgnoreNotFound(r.Update(ctx, update))
}

// branchAllowed reports whether branch matches one of the GitProvider's allowedBranches patterns.
func branchAllowed(patterns []string, branch string) bool {
	for _, pattern := range patterns {
		if match, err := filepath.Match(pattern, branch); err == nil && match {
			return true
		}
	}
	return false
}

// parentBranchNotFoundMessage says which parent is missing and what fixes it.
func parentBranchNotFoundMessage(branch *configbutleraiv1alpha3.GitBranch, missing string) string {
	switch {
	case branch.Spec.ParentBranch != "":
		return fmt.Sprintf(
			"Branch '%s' does not exist on the remote, and neither does its parent branch '%s' to create "+
				"it from; nothing is written until one of them exists",
			branch.Spec.Branch, branch.Spec.ParentBranch)
	case missing != "":
		return fmt.Sprintf(
			"Branch '%s' does not exist on the remote, and the remote's default branch (HEAD -> '%s') does "+
				"not exist either; fix the remote's HEAD or set spec.parentBranch. Nothing is written until then",
			branch.Spec.Branch, missing)
	default:
		return fmt.Sprintf(
			"Branch '%s' does not exist on the remote, and the remote has no default branch to create it "+
				"from; set the remote's HEAD or spec.parentBranch. Nothing is written until then",
			branch.Spec.Branch)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitBranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.GitBranch{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Every GitProvider event, status included: its Ready flipping is what un-blocks a GitBranch,
		// and providers are few.
		Watches(&configbutleraiv1alpha3.GitProvider{}, handler.EnqueueRequestsFromMapFunc(r.gitProviderToGitBranches)).
		// gitBranchRef is immutable, so only a GitTarget appearing or going changes the count.
		Watches(&configbutleraiv1alpha3.GitTarget{}, handler.EnqueueRequestsFromMapFunc(gitTargetToGitBranch),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
				},
			})).
		Named("gitbranch")
	if r.WorkerManager != nil && r.WorkerManager.GitBranchEvents() != nil {
		b = b.WatchesRawSource(source.Channel(r.WorkerManager.GitBranchEvents(), &handler.EnqueueRequestForObject{}))
	}
	return b.Complete(r)
}

// gitProviderToGitBranches enqueues the GitBranches in the provider's namespace that name it.
func (r *GitBranchReconciler) gitProviderToGitBranches(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	var branches configbutleraiv1alpha3.GitBranchList
	if err := r.List(ctx, &branches, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range branches.Items {
		if branches.Items[i].Spec.GitProviderRef.Name == obj.GetName() {
			requests = append(requests, ctrlreconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&branches.Items[i]),
			})
		}
	}
	return requests
}

// gitTargetToGitBranch enqueues the GitBranch a GitTarget references.
func gitTargetToGitBranch(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	target, ok := obj.(*configbutleraiv1alpha3.GitTarget)
	if !ok {
		return nil
	}
	key := target.GitBranchKey()
	if key.Name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: key}}
}
