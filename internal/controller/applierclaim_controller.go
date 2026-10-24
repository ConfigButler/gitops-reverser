// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// ApplierClaimConditionConfigured reports whether an applier claim's applier is one GitOps Reverser can
// drive: it exists in its cluster, applies a source that follows a branch, may be written, and (an
// Argo CD Application) has automated sync off or a switch the applier claim may hold. The applier claim's KubeProvider must admit its
// namespace first, and no applier claim in line before it may name the same applier
// (applier.ReasonHeldByAnotherClaim).
const ApplierClaimConditionConfigured = "Configured"

// ReasonOperationUnresolved is Ready=False on an applier claim whose operation has not completed in the
// time it was given. It clears when the operation completes.
const ReasonOperationUnresolved = "OperationUnresolved"

// ApplierClaimConditionHeld reports whether an applier claim holds its applier. A FluxClaim holds its
// Kustomization suspended under GitOps Reverser's field manager, with the last suspend's token
// handled, so nothing that started before the suspend is still running, or resumed for an apply of
// GitOps Reverser's own. An ArgoCDClaim holds its Application's automated sync off
// (argocdclaim_hold.go). An applier claim is Ready only once it holds its applier.
const ApplierClaimConditionHeld = applier.ApplierClaimConditionHeld

// Held reasons, and the Events of the hold lifecycle on an applier claim.
const (
	// ReasonSuspended is Held=True: the Kustomization is suspended, the last suspend handled, and
	// nothing applies.
	ReasonSuspended = "Suspended"
	// ReasonApplying is Held=True while an operation of a branch worker is in flight and followed:
	// the Kustomization is resumed for it, and suspended again when it ends.
	ReasonApplying = "Applying"
	// ReasonSuspending is Held=False while the token of the suspend is not handled yet.
	ReasonSuspending = "Suspending"
	// ReasonNotConfigured is Held=Unknown on an applier claim that is not Configured: nothing is held, or
	// the Kustomization is left suspended as the applier claim last held it (the message says which).
	ReasonNotConfigured = "NotConfigured"
	// ReasonReleasing is Held=False on an applier claim that held its Kustomization and is handing it back:
	// it is being deleted, or no eligible GitTarget references it any more. Held=False with
	// applier.ReasonNotReferenced is an applier claim with nothing to hold.
	ReasonReleasing = "Releasing"
	// ReasonRevoking is Held=False on an applier claim that held its applier and is handing it back
	// because its KubeProvider's spec.allowedApplierClaims no longer admit it. The hand-back finishes
	// even if a grant admits the applier claim again meanwhile, which then adopts its applier anew.
	ReasonRevoking = "Revoking"
	// EventReasonAdopted is a Kustomization suspended, or an Application's automated sync switched
	// off, for the first time.
	EventReasonAdopted = "Adopted"
	// EventReasonClaimedResourceReconciled is a held applier found changed by somebody else, and put
	// back as the applier claim holds it: a Kustomization resumed (`flux resume`) and suspended again, or an
	// Application's automated sync switched on and off again.
	EventReasonClaimedResourceReconciled = "ClaimedResourceReconciled"
	// EventReasonReleased is an applier handed back to Flux or Argo CD, or left held for the applier claim
	// next in line, when its applier claim was deleted or no eligible GitTarget references it any more.
	EventReasonReleased = "Released"
	// EventReasonReleaseFailed is a retiring applier claim that could not hand its applier back.
	EventReasonReleaseFailed = "ReleaseFailed"
)

// heldClaimSteadyInterval is how often an applier claim that is Ready reads its applier again: that read
// is what finds a `flux resume`, or automated sync switched on in Argo CD, so it is more often than
// other kinds settle.
const heldClaimSteadyInterval = time.Minute

// applierClaimSuspendingInterval is how soon an applier claim waiting on its Kustomization's suspend is
// read again: a suspended reconcile handles the token in tens of milliseconds.
const applierClaimSuspendingInterval = 2 * time.Second

// applierCheckTimeout bounds the Configured check: at most two GETs and two access reviews
// against the applier's cluster. Its client has no request timeout, because it also carries the
// watches, and a cluster that accepts the connection and never answers would otherwise hold this
// controller's worker, not only this applier claim.
const applierCheckTimeout = 10 * time.Second

// Why the applier claim reconciler reads the applier on every reconcile rather than watching it: Flux or
// Argo CD may not be installed in the applier's cluster at all, and an informer on a kind that is
// not served would be a failure to report rather than a check that answers "not found". The
// cadence is its own: heldClaimSteadyInterval,
// applierClaimSuspendingInterval while a Kustomization has not handled its suspend yet, or
// applierClaimSettleInterval while something is expected to clear on its own, such as an applier
// created after its applier claim.

// applierClaimSettleInterval is how soon an applier claim that is not Ready, and not stalled, is read again.
const applierClaimSettleInterval = 15 * time.Second

// ApplierClaimReconciler reconciles one applier claim kind: it checks that the applier the applier claim
// holds is one GitOps Reverser can drive, and publishes what it applies (status.source) for the
// GitTargets naming the applier claim. It also holds the applier (a Kustomization suspended,
// fluxclaim_hold.go; an Application's automated sync off, argocdclaim_hold.go), and hands it
// back once the applier claim is deleted. It never starts an apply: the
// branch workers decide when, and write status.operation and status.repairPending. It writes
// status.operation only for what no worker follows: an external resume, and an operation nobody
// follows any more.
type ApplierClaimReconciler struct {
	client.Client

	// Kind is the applier claim kind this reconciler serves (v1alpha3.ApplierClaimKind*).
	Kind     string
	Recorder record.EventRecorder

	// ApplierClients resolves the dynamic client of an applier claim's cluster. Nil leaves Configured
	// Unknown (tests, and the CLI).
	ApplierClients applier.Clients
	// APIReader reads an applier claim uncached before its Kustomization is suspended, so an operation a
	// branch worker recorded a moment ago is never taken for an external resume. Nil reads through
	// Client.
	APIReader client.Reader
	// Follower says which branch-worker operations this process follows (the apply dispatcher). A
	// FluxClaim's Kustomization is left resumed for an operation in flight only while it is
	// followed, or for UnfollowedGrace (DefaultUnfollowedGrace when zero) after it was first seen
	// followed by nobody. Nil follows nothing, so only the grace applies.
	Follower        OperationFollower
	UnfollowedGrace time.Duration
	unfollowed      unfollowedOperations
	// newApplier builds the client that checks an applier of a kind; nil is applier.NewClient. A
	// test sets it to answer the access review without a real API server.
	newApplier func(kind string, client dynamic.Interface) applier.Client
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxclaims;argocdclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxclaims;argocdclaims,verbs=update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxclaims/finalizers;argocdclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxclaims/status;argocdclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=kubeproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets;gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile publishes one applier claim's Configured and Held conditions, its source and the readiness
// trio.
func (r *ApplierClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := configbutleraiv1alpha3.NewApplierClaim(r.Kind)
	if claim == nil {
		return ctrl.Result{}, fmt.Errorf("no applier claim kind %q", r.Kind)
	}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			telemetry.ForgetResourceConditions(r.Kind, req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	st := beginStatus(r.Client, r.Recorder, claim)
	switch t := claim.(type) {
	case *configbutleraiv1alpha3.FluxClaim:
		return r.reconcileFlux(ctx, st, t)
	case *configbutleraiv1alpha3.ArgoCDClaim:
		return r.reconcileArgoCD(ctx, st, t)
	}
	return ctrl.Result{}, fmt.Errorf("no applier claim kind %q", r.Kind)
}

// reconcileFlux is Reconcile for a FluxClaim: one being deleted, or that no eligible GitTarget
// references any more, hands its Kustomization back; any other holds it while it is Configured.
func (r *ApplierClaimReconciler) reconcileFlux(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.FluxClaim,
) (ctrl.Result, error) {
	if !claim.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(claim, configbutleraiv1alpha3.FluxClaimFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.retire(ctx, st, claim, nil)
	}
	check := r.configuredVerdict(ctx, claim)
	publishCheck(st, claim, check)
	if check.retiring(claim) {
		return r.retire(ctx, st, claim, &check)
	}
	held, err := r.holdFlux(ctx, st, claim, check)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.setValue(ApplierClaimConditionHeld, held)
	return r.commitClaim(ctx, st, r.claimReadiness(claim, check.configured, &held), &held,
		heldClaimSteadyInterval)
}

// publishCheck sets Configured, and the source when the check was made.
func publishCheck(st *reconcileStatus, claim configbutleraiv1alpha3.ApplierClaim, check claimCheck) {
	st.setValue(ApplierClaimConditionConfigured, check.configured)
	if check.checked {
		setClaimSource(claim, check.source)
	}
}

// commitClaim writes the trio from rd and the status, and returns the cadence to read the
// applier claim again on: steady, or sooner while a suspend is waited for or something may clear on its
// own.
func (r *ApplierClaimReconciler) commitClaim(
	ctx context.Context, st *reconcileStatus, rd *readiness, held *conditionValue, steady time.Duration,
) (ctrl.Result, error) {
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	requeue := steady
	switch {
	case held != nil && held.Reason == ReasonSuspending:
		requeue = applierClaimSuspendingInterval
	case rd.converging():
		requeue = applierClaimSettleInterval
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(requeue)}, nil
}

// holdFlux returns a FluxClaim's Held condition. A Configured applier claim holds its Kustomization,
// with its finalizer put in place before the first adoption write, and then publishes its source
// consumers. One that is not Configured holds nothing,
// and one in line behind another holder lets go of a finalizer it carries, handing nothing back.
func (r *ApplierClaimReconciler) holdFlux(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.FluxClaim, check claimCheck,
) (conditionValue, error) {
	configured := check.configured
	ref := claim.Applier()
	switch {
	case configured.Status == metav1.ConditionTrue:
		if err := r.setFinalizer(ctx, st, claim, true); err != nil {
			return conditionValue{}, err
		}
		held := r.holdKustomization(ctx, st, claim)
		if check.checked {
			r.publishConsumers(ctx, claim)
		}
		return held, nil
	case configured.Reason == applier.ReasonNotReferenced:
		return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
			Message: "nothing to hold: no GitTarget names this applier claim"}, nil
	case configured.Reason == applier.ReasonHeldByAnotherClaim && applier.Holding(claim):
		// The holder took it, or two adopted at once: spec.suspend is held under the same field
		// manager either way, so there is nothing of this applier claim's to hand back.
		if err := r.setFinalizer(ctx, st, claim, false); err != nil {
			return conditionValue{}, err
		}
	case applier.Holding(claim):
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
			"Kustomization %s is left suspended under GitOps Reverser's field manager, as this applier claim last held "+
				"it: it is held again once the applier claim is Configured, and handed back once no eligible GitTarget "+
				"references the applier claim or the applier claim is deleted",
			ref,
		)}, nil
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
		Message: "the Kustomization is held once the applier claim is Configured"}, nil
}

// claimCheck is what configuredVerdict found.
type claimCheck struct {
	// configured is the Configured condition.
	configured conditionValue
	// source is what the applier applies, published only while the binding is satisfied.
	source *applier.Source
	// checked is false when the check could not be made: the published verdict and source stay.
	checked bool
	// refs are the GitTargets naming the applier claim, judged; judged is false when they could not be.
	refs   applier.References
	judged bool
	// denied is a conclusive refusal by the KubeProvider's spec.allowedApplierClaims: the applier
	// claim's namespace is not admitted, or its applier is outside the applierNamespaces it is given.
	// A read error is never a denial.
	denied bool
}

// retiring reports whether claim holds its applier and must hand it back, conclusively: its
// KubeProvider's grants refuse it, or no eligible GitTarget references it any more (none names it,
// or each is refused). A hand-back that began on a denial finishes even once a grant admits the
// applier claim again; it then adopts the applier anew. A reference that cannot be judged yet keeps
// the hold, since a release is not undone: Flux applies at once, and Argo CD syncs.
func (c claimCheck) retiring(claim configbutleraiv1alpha3.ApplierClaim) bool {
	if !applier.Holding(claim) {
		return false
	}
	return c.denied || revoking(claim) || (c.judged && (!c.refs.Named() || c.refs.Refused()))
}

// configuredVerdict judges the GitTargets naming the applier claim, and, while one may be eligible,
// checks the applier claim's applier in its cluster and the binding against what it read. An applier claim no
// GitTarget names is NotReferenced, and reads nothing in the applier's cluster. A check that could
// not be made keeps the verdict and the source already published, since a blip reading that cluster
// says nothing about the applier; with nothing published it is Unknown.
func (r *ApplierClaimReconciler) configuredVerdict(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim,
) claimCheck {
	cluster := claim.KubeProviderKey()
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	kept := func(message string) claimCheck { return keptCheck(claim, message) }

	targets, err := applier.NamingTargets(ctx, r.Client, claim)
	if err != nil {
		return kept(fmt.Sprintf("Cannot list the GitTargets naming the applier claim: %v", err))
	}
	if len(targets) == 0 {
		return claimCheck{configured: conditionValue{
			Status: metav1.ConditionUnknown, Reason: applier.ReasonNotReferenced,
			Message: "no GitTarget names this applier claim; it reads and writes nothing until one does",
		}, checked: true, judged: true}
	}
	refs, err := applier.JudgeReferences(ctx, r.Client, claim, targets, nil)
	if err != nil {
		return kept(err.Error())
	}

	decision, err := authz.ApplierClaimAdmitted(ctx, r.Client, claim)
	switch {
	case err != nil:
		return kept(fmt.Sprintf("Cannot check KubeProvider %s: %v", cluster, err))
	case !decision.Allowed:
		check := falseCheck(decision.Reason, decision.Message, refs)
		check.denied = decision.Reason == authz.ReasonNamespaceNotAuthorized ||
			decision.Reason == authz.ReasonApplierNamespaceNotAllowed
		return check
	case refs.Refused():
		return falseCheck(refs.Reason(), refs.Message(), refs)
	}
	return r.applierVerdict(ctx, claim, targets, refs)
}

// applierVerdict checks the applier claim's applier in its cluster, and judges the GitTargets naming the
// applier claim against the source it read: refs is their judgement on admission alone.
func (r *ApplierClaimReconciler) applierVerdict(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim, targets []*configbutleraiv1alpha3.GitTarget,
	refs applier.References,
) claimCheck {
	kind, ref, cluster := claim.ApplierKind(), claim.Applier(), claim.KubeProviderKey().String()
	if r.ApplierClients == nil {
		return claimCheck{configured: conditionValue{
			Status:  metav1.ConditionUnknown,
			Reason:  GitTargetReasonNotChecked,
			Message: fmt.Sprintf("%s %s has not been checked: no cluster client is wired", kind, ref),
		}}
	}
	dynamicClient, err := r.ApplierClients(ctx, cluster)
	if err != nil {
		return keptCheck(claim, fmt.Sprintf("Cannot reach cluster %q to check %s %s: %v", cluster, kind, ref, err))
	}
	newApplier := r.newApplier
	if newApplier == nil {
		newApplier = applier.NewClient
	}
	verdict := newApplier(kind, dynamicClient).Validate(ctx, ref)
	switch {
	case verdict.Err != nil:
		return keptCheck(claim, verdict.Message)
	case !verdict.OK:
		return falseCheck(verdict.Reason, verdict.Message, refs)
	}

	refs, err = applier.JudgeReferences(ctx, r.Client, claim, targets, verdict.Source)
	switch {
	case err != nil:
		return keptCheck(claim, err.Error())
	case refs.Eligible():
		return r.heldVerdict(ctx, claim, verdict, refs)
	case refs.Refused():
		return falseCheck(refs.Reason(), refs.Message(), refs)
	}
	return claimCheck{configured: conditionValue{
		Status: metav1.ConditionUnknown, Reason: applier.ReasonNotBound, Message: refs.Message(),
	}, checked: true, refs: refs, judged: true}
}

// keptCheck is a check that could not be made: the published verdict and source stay.
func keptCheck(claim configbutleraiv1alpha3.ApplierClaim, message string) claimCheck {
	return claimCheck{configured: keptCondition(*claim.StatusConditions(), ApplierClaimConditionConfigured,
		message)}
}

// falseCheck is Configured=False for reason, with the GitTargets naming the applier claim as judged.
func falseCheck(reason, message string, refs applier.References) claimCheck {
	return claimCheck{configured: conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message},
		checked: true, refs: refs, judged: true}
}

// heldVerdict is Configured for an applier claim whose applier passed every other check and an eligible
// GitTarget references: True when this applier claim holds it, False (HeldByAnotherClaim) when another
// applier claim in line comes first. Checked last, so an applier claim in line for an applier is one that can
// take it over as it is.
func (r *ApplierClaimReconciler) heldVerdict(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim, verdict applier.Verdict,
	refs applier.References,
) claimCheck {
	// The applier claims in line are judged against the source just read, which they share.
	setClaimSource(claim, verdict.Source)
	holder, err := applier.HeldByAnother(ctx, r.Client, claim)
	switch {
	case err != nil:
		return claimCheck{configured: keptCondition(*claim.StatusConditions(), ApplierClaimConditionConfigured,
			fmt.Sprintf("Cannot check which applier claim holds %s %s: %v", claim.ApplierKind(), claim.Applier(), err)),
			source: verdict.Source, checked: true, refs: refs, judged: true}
	case holder != nil:
		return claimCheck{configured: conditionValue{
			Status: metav1.ConditionFalse, Reason: applier.ReasonHeldByAnotherClaim,
			Message: applier.HeldByAnotherMessage(claim, holder),
		}, source: verdict.Source, checked: true, refs: refs, judged: true}
	}
	return claimCheck{
		configured: conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded, Message: verdict.Message},
		source:     verdict.Source, checked: true, refs: refs, judged: true,
	}
}

// setClaimSource publishes what the applier applies, or clears it when the check read nothing.
func setClaimSource(claim configbutleraiv1alpha3.ApplierClaim, source *applier.Source) {
	switch t := claim.(type) {
	case *configbutleraiv1alpha3.FluxClaim:
		t.Status.Source = nil
		if source != nil {
			t.Status.Source = &configbutleraiv1alpha3.FluxClaimSource{
				GitRepository: meta.NamespacedObjectReference{
					Namespace: source.GitRepository.Namespace, Name: source.GitRepository.Name,
				},
				URL:    source.URL,
				Branch: source.Revision,
			}
		}
	case *configbutleraiv1alpha3.ArgoCDClaim:
		t.Status.Source = nil
		if source != nil {
			t.Status.Source = &configbutleraiv1alpha3.ArgoCDClaimSource{
				RepoURL: source.URL, TargetRevision: source.Revision,
			}
		}
	}
}

// claimSourceSummary names what an applier applies, as the applier claim publishes it, for the Ready
// message.
func claimSourceSummary(claim configbutleraiv1alpha3.ApplierClaim) string {
	url, revision, known := claim.SourceBinding()
	if !known {
		return ""
	}
	return fmt.Sprintf("(revision %q of %s)", revision, url)
}

// claimReadiness is the trio's accumulator for an applier claim, its gates contributed.
func (r *ApplierClaimReconciler) claimReadiness(
	claim configbutleraiv1alpha3.ApplierClaim, configured conditionValue, held *conditionValue,
) *readiness {
	rd := newReadiness(fmt.Sprintf("%s %s %s can be asked to apply", claim.ApplierKind(), claim.Applier(),
		claimSourceSummary(claim)), claim.ClaimKind()+" is not stalled")
	applierClaimReadinessGates(rd, configured, held, claim.Operation())
	return rd
}

// applierClaimReadinessGates contributes Configured, the operation, and Held to the trio, in that
// order.
// An applier that is missing, or a KubeProvider that is, may be on its way (Flux and Argo CD
// objects are often created after the applier claim); anything else Configured=False needs a human. An
// operation that has not completed in time clears on its own once it does. An applier claim no GitTarget
// names is Ready (NotReferenced): it does all it is asked to, which is nothing, and a Flux health
// check that waits on it must not wait for the GitTarget that will name it.
func applierClaimReadinessGates(
	rd *readiness, configured conditionValue, held *conditionValue, operation *configbutleraiv1alpha3.ApplyOperation,
) {
	if configured.Reason == applier.ReasonNotReferenced {
		rd.convergesAs(conditionValue{Status: metav1.ConditionTrue, Reason: applier.ReasonNotReferenced,
			Message: "no GitTarget names this applier claim, so it does nothing"})
	}
	waits := configured.Reason == applier.ReasonApplierNotFound ||
		configured.Reason == authz.ReasonKubeProviderNotFound
	rd.stalledIf(configured.Status == metav1.ConditionFalse && !waits, configured.Reason, configured.Message)
	rd.progressingIf(configured.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		configured.Reason, configured.Message)
	rd.progressingIf(configured.Status == metav1.ConditionUnknown && configured.Reason != applier.ReasonNotReferenced,
		metav1.ConditionUnknown, configured.Reason, configured.Message)
	if operation != nil && operation.Phase == configbutleraiv1alpha3.ApplyOperationUnresolved {
		what := fmt.Sprintf("the %s apply of %s, asked for at", operation.Reason,
			applier.DescribeCommit(operation.Commit))
		if operation.Reason == "" {
			what = "an apply GitOps Reverser did not start, seen at"
		}
		rd.progressing(metav1.ConditionFalse, ReasonOperationUnresolved, fmt.Sprintf(
			"%s %s, has not completed: %s", what, operation.RequestedAt.UTC().Format(time.RFC3339), operation.Message))
	}
	if held != nil {
		rd.progressingIf(held.Status != metav1.ConditionTrue && held.Reason != applier.ReasonNotReferenced,
			held.Status, held.Reason, held.Message)
	}
}

// SetupWithManager sets up the controller for its kind with the Manager.
func (r *ApplierClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	claim := configbutleraiv1alpha3.NewApplierClaim(r.Kind)
	if claim == nil {
		return fmt.Errorf("no applier claim kind %q", r.Kind)
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The status is written by two parties: this reconciler (conditions, source) and the branch
		// workers (operation, repairPending). A status-only update is filtered, as everywhere, except
		// one that moves the operation's phase, which Ready reads.
		For(claim, builder.WithPredicates(applierClaimSpecOrOperationChanged())).
		// What makes a GitTarget eligible re-runs the check of the applier claim it names, and of the one it
		// named before: the GitTarget appearing, going, or changing what it names or writes.
		Watches(&configbutleraiv1alpha3.GitTarget{}, r.gitTargetToClaims()).
		// A GitBranch and its GitProvider's URL are what its GitTargets write.
		Watches(
			&configbutleraiv1alpha3.GitBranch{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToClaims),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToClaims),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// A KubeProvider admits the applier claim's namespace, and the namespaces of the GitTargets naming
		// applier claims, so a provider created, deleted, or with its spec changed re-runs every check.
		Watches(
			&configbutleraiv1alpha3.KubeProvider{},
			handler.EnqueueRequestsFromMapFunc(r.kubeProviderToClaims),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// A Selector grant reads namespace labels.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToClaims),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		// One applier claim holds an applier; the others naming it wait in line. A holder that is gone, or
		// lets go, and an applier claim that joins or leaves the line, re-run the others' check: nothing else
		// changes on them to reconcile them.
		Watches(
			claim,
			handler.EnqueueRequestsFromMapFunc(r.claimsInLine),
			builder.WithPredicates(claimLineChanged()),
		).
		Named(r.Kind).
		Complete(r)
}

// applierClaimSpecOrOperationChanged fires on create and delete, on a spec change, on a change of
// the reconcile-request annotation, and on a change of status.operation.phase.
func applierClaimSpecOrOperationChanged() predicate.Predicate {
	requested := reconcileRequestedOrSpecChanged()
	return predicate.Funcs{
		CreateFunc:  requested.Create,
		DeleteFunc:  requested.Delete,
		GenericFunc: requested.Generic,
		UpdateFunc: func(e event.UpdateEvent) bool {
			if requested.Update(e) {
				return true
			}
			before, ok1 := e.ObjectOld.(configbutleraiv1alpha3.ApplierClaim)
			after, ok2 := e.ObjectNew.(configbutleraiv1alpha3.ApplierClaim)
			return !ok1 || !ok2 || operationPhase(before) != operationPhase(after)
		},
	}
}

// operationPhase is an applier claim's status.operation.phase, or "" without one.
func operationPhase(claim configbutleraiv1alpha3.ApplierClaim) configbutleraiv1alpha3.ApplyOperationPhase {
	if operation := claim.Operation(); operation != nil {
		return operation.Phase
	}
	return ""
}

// listClaims lists this reconciler's kind, in namespace or everywhere when it is empty.
func (r *ApplierClaimReconciler) listClaims(
	ctx context.Context, namespace string,
) ([]configbutleraiv1alpha3.ApplierClaim, error) {
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	return applier.ListApplierClaims(ctx, r.Client, r.Kind, opts...)
}

// claimLineChanged passes the event of an applier claim that is gone, and an update that can move it in
// or out of line for its applier: its finalizer, its Configured condition, or its Held one.
func claimLineChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(configbutleraiv1alpha3.ApplierClaim)
			after, ok2 := e.ObjectNew.(configbutleraiv1alpha3.ApplierClaim)
			return !ok1 || !ok2 || lineSummary(before) != lineSummary(after)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// lineSummary is what of an applier claim decides its place in line for its applier.
func lineSummary(claim configbutleraiv1alpha3.ApplierClaim) string {
	var summary strings.Builder
	summary.WriteString(strconv.FormatBool(applier.Holding(claim)))
	for _, conditionType := range []string{ApplierClaimConditionConfigured, ApplierClaimConditionHeld} {
		if cond := findCondition(*claim.StatusConditions(), conditionType); cond != nil {
			summary.WriteString("|" + string(cond.Status) + "/" + cond.Reason)
		}
	}
	return summary.String()
}

// gitTargetToClaims enqueues the applier claims of this kind a GitTarget names, before and after an
// update, when it appears, goes, or changes anything eligibility reads: its spec, or its deletion.
func (r *ApplierClaimReconciler) gitTargetToClaims() handler.EventHandler {
	named := func(obj client.Object) []ctrlreconcile.Request {
		target, ok := obj.(*configbutleraiv1alpha3.GitTarget)
		if !ok || target.Spec.ApplierClaimRef == nil || target.Spec.ApplierClaimRef.Kind != r.Kind {
			return nil
		}
		return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
			Namespace: target.Namespace, Name: target.Spec.ApplierClaimRef.Name,
		}}}
	}
	add := func(q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request], objs ...client.Object) {
		for _, obj := range objs {
			for _, request := range named(obj) {
				q.Add(request)
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(q, e.Object)
		},
		UpdateFunc: func(_ context.Context, e event.UpdateEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			if e.ObjectOld.GetGeneration() == e.ObjectNew.GetGeneration() &&
				e.ObjectOld.GetDeletionTimestamp().IsZero() == e.ObjectNew.GetDeletionTimestamp().IsZero() {
				return
			}
			add(q, e.ObjectOld, e.ObjectNew)
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(q, e.Object)
		},
	}
}

// gitDestinationToClaims maps a GitBranch or GitProvider event to the applier claims of this kind named
// by the GitTargets that write through it.
func (r *ApplierClaimReconciler) gitDestinationToClaims(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	targets, err := gitTargetsOn(ctx, r.Client, obj)
	if err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}
	seen := map[k8stypes.NamespacedName]bool{}
	var requests []ctrlreconcile.Request
	for i := range targets {
		target := &targets[i]
		ref := target.Spec.ApplierClaimRef
		if ref == nil || ref.Kind != r.Kind {
			continue
		}
		key := k8stypes.NamespacedName{Namespace: target.Namespace, Name: ref.Name}
		if !seen[key] {
			seen[key] = true
			requests = append(requests, ctrlreconcile.Request{NamespacedName: key})
		}
	}
	return requests
}

// claimsInLine maps an applier claim that is gone, or that moved in line, to the other applier claims of this
// kind naming its applier, so the next in line takes it over.
func (r *ApplierClaimReconciler) claimsInLine(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	gone, ok := obj.(configbutleraiv1alpha3.ApplierClaim)
	if !ok || gone.ClaimKind() != r.Kind {
		return nil
	}
	others, err := applier.ClaimsOfApplier(ctx, r.Client, gone)
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(others))
	for _, other := range others {
		requests = append(requests, claimRequest(other))
	}
	return requests
}

// kubeProviderToClaims maps a KubeProvider event to every applier claim of this kind: a provider
// admits the applier claims that name it, and the GitTargets that mirror through it, which may name any
// applier claim in their namespace.
func (r *ApplierClaimReconciler) kubeProviderToClaims(
	ctx context.Context, obj client.Object,
) []ctrlreconcile.Request {
	claims, err := r.listClaims(ctx, "")
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(claims))
	for _, claim := range claims {
		requests = append(requests, claimRequest(claim))
	}
	return requests
}

// namespaceToClaims maps a Namespace label change to every applier claim of this kind in it.
func (r *ApplierClaimReconciler) namespaceToClaims(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	claims, err := r.listClaims(ctx, obj.GetName())
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(claims))
	for _, claim := range claims {
		requests = append(requests, claimRequest(claim))
	}
	return requests
}

func claimRequest(claim configbutleraiv1alpha3.ApplierClaim) ctrlreconcile.Request {
	return ctrlreconcile.Request{NamespacedName: k8stypes.NamespacedName{
		Namespace: claim.GetNamespace(), Name: claim.GetName(),
	}}
}
