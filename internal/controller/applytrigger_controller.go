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

// ApplyTriggerConditionConfigured reports whether a trigger's applier is one GitOps Reverser can
// drive: it exists in its cluster, applies a source that follows a branch, may be written, and (an
// Argo CD Application) has automated sync off or a switch the trigger may hold. The trigger's KubeProvider must admit its
// namespace first, and no trigger in line before it may name the same applier
// (applier.ReasonHeldByAnotherTrigger).
const ApplyTriggerConditionConfigured = "Configured"

// ReasonOperationUnresolved is Ready=False on a trigger whose operation has not completed in the
// time it was given. It clears when the operation completes.
const ReasonOperationUnresolved = "OperationUnresolved"

// ApplyTriggerConditionHeld reports whether a trigger holds its applier. A FluxTrigger holds its
// Kustomization suspended under GitOps Reverser's field manager, with the last suspend's token
// handled, so nothing that started before the suspend is still running, or resumed for an apply of
// GitOps Reverser's own. An ArgoCDTrigger holds its Application's automated sync off
// (argocdtrigger_hold.go). A trigger is Ready only once it holds its applier.
const ApplyTriggerConditionHeld = applier.TriggerConditionHeld

// Held reasons, and the Events of the hold lifecycle on a trigger.
const (
	// ReasonSuspended is Held=True: the Kustomization is suspended, the last suspend handled, and
	// nothing applies.
	ReasonSuspended = "Suspended"
	// ReasonApplying is Held=True while an operation of a branch worker is in flight and followed:
	// the Kustomization is resumed for it, and suspended again when it ends.
	ReasonApplying = "Applying"
	// ReasonSuspending is Held=False while the token of the suspend is not handled yet.
	ReasonSuspending = "Suspending"
	// ReasonNotConfigured is Held=Unknown on a trigger that is not Configured: nothing is held, or
	// the Kustomization is left suspended as the trigger last held it (the message says which).
	ReasonNotConfigured = "NotConfigured"
	// ReasonReleasing is Held=False on a trigger that held its Kustomization and is handing it back:
	// it is being deleted, or no eligible GitTarget references it any more. Held=False with
	// applier.ReasonNotReferenced is a trigger with nothing to hold.
	ReasonReleasing = "Releasing"
	// EventReasonAdopted is a Kustomization suspended, or an Application's automated sync switched
	// off, for the first time.
	EventReasonAdopted = "Adopted"
	// EventReasonClaimedResourceReconciled is a held applier found changed by somebody else, and put
	// back as the trigger holds it: a Kustomization resumed (`flux resume`) and suspended again, or an
	// Application's automated sync switched on and off again.
	EventReasonClaimedResourceReconciled = "ClaimedResourceReconciled"
	// EventReasonReleased is an applier handed back to Flux or Argo CD, or left held for the trigger
	// next in line, when its trigger was deleted or no eligible GitTarget references it any more.
	EventReasonReleased = "Released"
	// EventReasonReleaseFailed is a retiring trigger that could not hand its applier back.
	EventReasonReleaseFailed = "ReleaseFailed"
)

// heldTriggerSteadyInterval is how often a trigger that is Ready reads its applier again: that read
// is what finds a `flux resume`, or automated sync switched on in Argo CD, so it is more often than
// other kinds settle.
const heldTriggerSteadyInterval = time.Minute

// applyTriggerSuspendingInterval is how soon a trigger waiting on its Kustomization's suspend is
// read again: a suspended reconcile handles the token in tens of milliseconds.
const applyTriggerSuspendingInterval = 2 * time.Second

// applierCheckTimeout bounds the Configured check: at most two GETs and two access reviews
// against the applier's cluster. Its client has no request timeout, because it also carries the
// watches, and a cluster that accepts the connection and never answers would otherwise hold this
// controller's worker, not only this trigger.
const applierCheckTimeout = 10 * time.Second

// Why the trigger reconciler reads the applier on every reconcile rather than watching it: Flux or
// Argo CD may not be installed in the applier's cluster at all, and an informer on a kind that is
// not served would be a failure to report rather than a check that answers "not found". The
// cadence is its own: heldTriggerSteadyInterval,
// applyTriggerSuspendingInterval while a Kustomization has not handled its suspend yet, or
// applyTriggerSettleInterval while something is expected to clear on its own, such as an applier
// created after its trigger.

// applyTriggerSettleInterval is how soon a trigger that is not Ready, and not stalled, is read again.
const applyTriggerSettleInterval = 15 * time.Second

// ApplyTriggerReconciler reconciles one apply trigger kind: it checks that the applier the trigger
// holds is one GitOps Reverser can drive, and publishes what it applies (status.source) for the
// GitTargets naming the trigger. It also holds the applier (a Kustomization suspended,
// fluxtrigger_hold.go; an Application's automated sync off, argocdtrigger_hold.go), and hands it
// back once the trigger is deleted. It never starts an apply: the
// branch workers decide when, and write status.operation and status.repairPending. It writes
// status.operation only for what no worker follows: an external resume, and an operation nobody
// follows any more.
type ApplyTriggerReconciler struct {
	client.Client

	// Kind is the trigger kind this reconciler serves (v1alpha3.ApplyTriggerKind*).
	Kind     string
	Recorder record.EventRecorder

	// ApplierClients resolves the dynamic client of a trigger's cluster. Nil leaves Configured
	// Unknown (tests, and the CLI).
	ApplierClients applier.Clients
	// APIReader reads a trigger uncached before its Kustomization is suspended, so an operation a
	// branch worker recorded a moment ago is never taken for an external resume. Nil reads through
	// Client.
	APIReader client.Reader
	// Follower says which branch-worker operations this process follows (the apply dispatcher). A
	// FluxTrigger's Kustomization is left resumed for an operation in flight only while it is
	// followed, or for UnfollowedGrace (DefaultUnfollowedGrace when zero) after it was first seen
	// followed by nobody. Nil follows nothing, so only the grace applies.
	Follower        OperationFollower
	UnfollowedGrace time.Duration
	unfollowed      unfollowedOperations
	// newApplier builds the client that checks an applier of a kind; nil is applier.NewClient. A
	// test sets it to answer the access review without a real API server.
	newApplier func(kind string, client dynamic.Interface) applier.Client
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers;argocdtriggers,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers;argocdtriggers,verbs=update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers/finalizers;argocdtriggers/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers/status;argocdtriggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=kubeproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets;gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile publishes one trigger's Configured and Held conditions, its source and the readiness
// trio.
func (r *ApplyTriggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	trigger := configbutleraiv1alpha3.NewApplyTrigger(r.Kind)
	if trigger == nil {
		return ctrl.Result{}, fmt.Errorf("no apply trigger kind %q", r.Kind)
	}
	if err := r.Get(ctx, req.NamespacedName, trigger); err != nil {
		if apierrors.IsNotFound(err) {
			telemetry.ForgetResourceConditions(r.Kind, req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	st := beginStatus(r.Client, r.Recorder, trigger)
	switch t := trigger.(type) {
	case *configbutleraiv1alpha3.FluxTrigger:
		return r.reconcileFlux(ctx, st, t)
	case *configbutleraiv1alpha3.ArgoCDTrigger:
		return r.reconcileArgoCD(ctx, st, t)
	}
	return ctrl.Result{}, fmt.Errorf("no apply trigger kind %q", r.Kind)
}

// reconcileFlux is Reconcile for a FluxTrigger: one being deleted, or that no eligible GitTarget
// references any more, hands its Kustomization back; any other holds it while it is Configured.
func (r *ApplyTriggerReconciler) reconcileFlux(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.FluxTrigger,
) (ctrl.Result, error) {
	if !trigger.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(trigger, configbutleraiv1alpha3.FluxTriggerFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.retire(ctx, st, trigger, nil)
	}
	check := r.configuredVerdict(ctx, trigger)
	publishCheck(st, trigger, check)
	if check.retiring(trigger) {
		return r.retire(ctx, st, trigger, &check)
	}
	held, err := r.holdFlux(ctx, st, trigger, check)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.setValue(ApplyTriggerConditionHeld, held)
	return r.commitTrigger(ctx, st, r.triggerReadiness(trigger, check.configured, &held), &held,
		heldTriggerSteadyInterval)
}

// publishCheck sets Configured, and the source when the check was made.
func publishCheck(st *reconcileStatus, trigger configbutleraiv1alpha3.ApplyTrigger, check triggerCheck) {
	st.setValue(ApplyTriggerConditionConfigured, check.configured)
	if check.checked {
		setTriggerSource(trigger, check.source)
	}
}

// commitTrigger writes the trio from rd and the status, and returns the cadence to read the
// trigger again on: steady, or sooner while a suspend is waited for or something may clear on its
// own.
func (r *ApplyTriggerReconciler) commitTrigger(
	ctx context.Context, st *reconcileStatus, rd *readiness, held *conditionValue, steady time.Duration,
) (ctrl.Result, error) {
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	requeue := steady
	switch {
	case held != nil && held.Reason == ReasonSuspending:
		requeue = applyTriggerSuspendingInterval
	case rd.converging():
		requeue = applyTriggerSettleInterval
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(requeue)}, nil
}

// holdFlux returns a FluxTrigger's Held condition. A Configured trigger holds its Kustomization,
// with its finalizer put in place before the first adoption write, and then publishes its source
// consumers. One that is not Configured holds nothing,
// and one in line behind another holder lets go of a finalizer it carries, handing nothing back.
func (r *ApplyTriggerReconciler) holdFlux(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.FluxTrigger, check triggerCheck,
) (conditionValue, error) {
	configured := check.configured
	ref := trigger.Applier()
	switch {
	case configured.Status == metav1.ConditionTrue:
		if err := r.setFinalizer(ctx, st, trigger, true); err != nil {
			return conditionValue{}, err
		}
		held := r.holdKustomization(ctx, st, trigger)
		if check.checked {
			r.publishConsumers(ctx, trigger)
		}
		return held, nil
	case configured.Reason == applier.ReasonNotReferenced:
		return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
			Message: "nothing to hold: no GitTarget names this trigger"}, nil
	case configured.Reason == applier.ReasonHeldByAnotherTrigger && applier.Holding(trigger):
		// The holder took it, or two adopted at once: spec.suspend is held under the same field
		// manager either way, so there is nothing of this trigger's to hand back.
		if err := r.setFinalizer(ctx, st, trigger, false); err != nil {
			return conditionValue{}, err
		}
	case applier.Holding(trigger):
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
			"Kustomization %s is left suspended under GitOps Reverser's field manager, as this trigger last held "+
				"it: it is held again once the trigger is Configured, and handed back once no eligible GitTarget "+
				"references the trigger or the trigger is deleted", ref)}, nil
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
		Message: "the Kustomization is held once the trigger is Configured"}, nil
}

// triggerCheck is what configuredVerdict found.
type triggerCheck struct {
	// configured is the Configured condition.
	configured conditionValue
	// source is what the applier applies, published only while the binding is satisfied.
	source *applier.Source
	// checked is false when the check could not be made: the published verdict and source stay.
	checked bool
	// refs are the GitTargets naming the trigger, judged; judged is false when they could not be.
	refs   applier.References
	judged bool
}

// retiring reports whether trigger holds its applier and no eligible GitTarget references it any
// more, conclusively: none names it, or each is refused. A reference that cannot be judged yet
// keeps the hold, since a release is not undone: Flux applies at once, and Argo CD syncs.
func (c triggerCheck) retiring(trigger configbutleraiv1alpha3.ApplyTrigger) bool {
	return applier.Holding(trigger) && c.judged && (!c.refs.Named() || c.refs.Refused())
}

// configuredVerdict judges the GitTargets naming the trigger, and, while one may be eligible,
// checks the trigger's applier in its cluster and the binding against what it read. A trigger no
// GitTarget names is NotReferenced, and reads nothing in the applier's cluster. A check that could
// not be made keeps the verdict and the source already published, since a blip reading that cluster
// says nothing about the applier; with nothing published it is Unknown.
func (r *ApplyTriggerReconciler) configuredVerdict(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger,
) triggerCheck {
	cluster := trigger.KubeProviderKey()
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	kept := func(message string) triggerCheck { return keptCheck(trigger, message) }

	targets, err := applier.NamingTargets(ctx, r.Client, trigger)
	if err != nil {
		return kept(fmt.Sprintf("Cannot list the GitTargets naming the trigger: %v", err))
	}
	if len(targets) == 0 {
		return triggerCheck{configured: conditionValue{
			Status: metav1.ConditionUnknown, Reason: applier.ReasonNotReferenced,
			Message: "no GitTarget names this trigger; it reads and writes nothing until one does",
		}, checked: true, judged: true}
	}
	refs, err := applier.JudgeReferences(ctx, r.Client, trigger, targets, nil)
	if err != nil {
		return kept(err.Error())
	}

	decision, err := authz.ApplyTriggerAdmitted(ctx, r.Client, trigger)
	switch {
	case err != nil:
		return kept(fmt.Sprintf("Cannot check KubeProvider %s: %v", cluster, err))
	case !decision.Allowed:
		return falseCheck(decision.Reason, decision.Message, refs)
	case refs.Refused():
		return falseCheck(refs.Reason(), refs.Message(), refs)
	}
	return r.applierVerdict(ctx, trigger, targets, refs)
}

// applierVerdict checks the trigger's applier in its cluster, and judges the GitTargets naming the
// trigger against the source it read: refs is their judgement on admission alone.
func (r *ApplyTriggerReconciler) applierVerdict(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger, targets []*configbutleraiv1alpha3.GitTarget,
	refs applier.References,
) triggerCheck {
	kind, ref, cluster := trigger.ApplierKind(), trigger.Applier(), trigger.KubeProviderKey().String()
	if r.ApplierClients == nil {
		return triggerCheck{configured: conditionValue{
			Status:  metav1.ConditionUnknown,
			Reason:  GitTargetReasonNotChecked,
			Message: fmt.Sprintf("%s %s has not been checked: no cluster client is wired", kind, ref),
		}}
	}
	dynamicClient, err := r.ApplierClients(ctx, cluster)
	if err != nil {
		return keptCheck(trigger, fmt.Sprintf("Cannot reach cluster %q to check %s %s: %v", cluster, kind, ref, err))
	}
	newApplier := r.newApplier
	if newApplier == nil {
		newApplier = applier.NewClient
	}
	verdict := newApplier(kind, dynamicClient).Validate(ctx, ref)
	switch {
	case verdict.Err != nil:
		return keptCheck(trigger, verdict.Message)
	case !verdict.OK:
		return falseCheck(verdict.Reason, verdict.Message, refs)
	}

	refs, err = applier.JudgeReferences(ctx, r.Client, trigger, targets, verdict.Source)
	switch {
	case err != nil:
		return keptCheck(trigger, err.Error())
	case refs.Eligible():
		return r.heldVerdict(ctx, trigger, verdict, refs)
	case refs.Refused():
		return falseCheck(refs.Reason(), refs.Message(), refs)
	}
	return triggerCheck{configured: conditionValue{
		Status: metav1.ConditionUnknown, Reason: applier.ReasonNotBound, Message: refs.Message(),
	}, checked: true, refs: refs, judged: true}
}

// keptCheck is a check that could not be made: the published verdict and source stay.
func keptCheck(trigger configbutleraiv1alpha3.ApplyTrigger, message string) triggerCheck {
	return triggerCheck{configured: keptCondition(*trigger.StatusConditions(), ApplyTriggerConditionConfigured,
		message)}
}

// falseCheck is Configured=False for reason, with the GitTargets naming the trigger as judged.
func falseCheck(reason, message string, refs applier.References) triggerCheck {
	return triggerCheck{configured: conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message},
		checked: true, refs: refs, judged: true}
}

// heldVerdict is Configured for a trigger whose applier passed every other check and an eligible
// GitTarget references: True when this trigger holds it, False (HeldByAnotherTrigger) when another
// trigger in line comes first. Checked last, so a trigger in line for an applier is one that can
// take it over as it is.
func (r *ApplyTriggerReconciler) heldVerdict(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger, verdict applier.Verdict,
	refs applier.References,
) triggerCheck {
	// The triggers in line are judged against the source just read, which they share.
	setTriggerSource(trigger, verdict.Source)
	holder, err := applier.HeldByAnother(ctx, r.Client, trigger)
	switch {
	case err != nil:
		return triggerCheck{configured: keptCondition(*trigger.StatusConditions(), ApplyTriggerConditionConfigured,
			fmt.Sprintf("Cannot check which trigger holds %s %s: %v", trigger.ApplierKind(), trigger.Applier(), err)),
			source: verdict.Source, checked: true, refs: refs, judged: true}
	case holder != nil:
		return triggerCheck{configured: conditionValue{
			Status: metav1.ConditionFalse, Reason: applier.ReasonHeldByAnotherTrigger,
			Message: applier.HeldByAnotherMessage(trigger, holder),
		}, source: verdict.Source, checked: true, refs: refs, judged: true}
	}
	return triggerCheck{
		configured: conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded, Message: verdict.Message},
		source:     verdict.Source, checked: true, refs: refs, judged: true,
	}
}

// setTriggerSource publishes what the applier applies, or clears it when the check read nothing.
func setTriggerSource(trigger configbutleraiv1alpha3.ApplyTrigger, source *applier.Source) {
	switch t := trigger.(type) {
	case *configbutleraiv1alpha3.FluxTrigger:
		t.Status.Source = nil
		if source != nil {
			t.Status.Source = &configbutleraiv1alpha3.FluxTriggerSource{
				GitRepository: meta.NamespacedObjectReference{
					Namespace: source.GitRepository.Namespace, Name: source.GitRepository.Name,
				},
				URL:    source.URL,
				Branch: source.Revision,
			}
		}
	case *configbutleraiv1alpha3.ArgoCDTrigger:
		t.Status.Source = nil
		if source != nil {
			t.Status.Source = &configbutleraiv1alpha3.ArgoCDTriggerSource{
				RepoURL: source.URL, TargetRevision: source.Revision,
			}
		}
	}
}

// triggerSourceSummary names what an applier applies, as the trigger publishes it, for the Ready
// message.
func triggerSourceSummary(trigger configbutleraiv1alpha3.ApplyTrigger) string {
	url, revision, known := trigger.SourceBinding()
	if !known {
		return ""
	}
	return fmt.Sprintf("(revision %q of %s)", revision, url)
}

// triggerReadiness is the trio's accumulator for a trigger, its gates contributed.
func (r *ApplyTriggerReconciler) triggerReadiness(
	trigger configbutleraiv1alpha3.ApplyTrigger, configured conditionValue, held *conditionValue,
) *readiness {
	rd := newReadiness(fmt.Sprintf("%s %s %s can be asked to apply", trigger.ApplierKind(), trigger.Applier(),
		triggerSourceSummary(trigger)), trigger.TriggerKind()+" is not stalled")
	applyTriggerReadinessGates(rd, configured, held, trigger.Operation())
	return rd
}

// applyTriggerReadinessGates contributes Configured, the operation, and Held to the trio, in that
// order.
// An applier that is missing, or a KubeProvider that is, may be on its way (Flux and Argo CD
// objects are often created after the trigger); anything else Configured=False needs a human. An
// operation that has not completed in time clears on its own once it does. A trigger no GitTarget
// names is Ready (NotReferenced): it does all it is asked to, which is nothing, and a Flux health
// check that waits on it must not wait for the GitTarget that will name it.
func applyTriggerReadinessGates(
	rd *readiness, configured conditionValue, held *conditionValue, operation *configbutleraiv1alpha3.ApplyOperation,
) {
	if configured.Reason == applier.ReasonNotReferenced {
		rd.convergesAs(conditionValue{Status: metav1.ConditionTrue, Reason: applier.ReasonNotReferenced,
			Message: "no GitTarget names this trigger, so it does nothing"})
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
func (r *ApplyTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	trigger := configbutleraiv1alpha3.NewApplyTrigger(r.Kind)
	if trigger == nil {
		return fmt.Errorf("no apply trigger kind %q", r.Kind)
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The status is written by two parties: this reconciler (conditions, source) and the branch
		// workers (operation, repairPending). A status-only update is filtered, as everywhere, except
		// one that moves the operation's phase, which Ready reads.
		For(trigger, builder.WithPredicates(applyTriggerSpecOrOperationChanged())).
		// What makes a GitTarget eligible re-runs the check of the trigger it names, and of the one it
		// named before: the GitTarget appearing, going, or changing what it names or writes.
		Watches(&configbutleraiv1alpha3.GitTarget{}, r.gitTargetToTriggers()).
		// A GitBranch and its GitProvider's URL are what its GitTargets write.
		Watches(
			&configbutleraiv1alpha3.GitBranch{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToTriggers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitDestinationToTriggers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// A KubeProvider admits the trigger's namespace, and the namespaces of the GitTargets naming
		// triggers, so a provider created, deleted, or with its spec changed re-runs every check.
		Watches(
			&configbutleraiv1alpha3.KubeProvider{},
			handler.EnqueueRequestsFromMapFunc(r.kubeProviderToTriggers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// An accessFrom selector reads namespace labels.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToTriggers),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		// One trigger holds an applier; the others naming it wait in line. A holder that is gone, or
		// lets go, and a trigger that joins or leaves the line, re-run the others' check: nothing else
		// changes on them to reconcile them.
		Watches(
			trigger,
			handler.EnqueueRequestsFromMapFunc(r.triggersInLine),
			builder.WithPredicates(triggerLineChanged()),
		).
		Named(r.Kind).
		Complete(r)
}

// applyTriggerSpecOrOperationChanged fires on create and delete, on a spec change, on a change of
// the reconcile-request annotation, and on a change of status.operation.phase.
func applyTriggerSpecOrOperationChanged() predicate.Predicate {
	requested := reconcileRequestedOrSpecChanged()
	return predicate.Funcs{
		CreateFunc:  requested.Create,
		DeleteFunc:  requested.Delete,
		GenericFunc: requested.Generic,
		UpdateFunc: func(e event.UpdateEvent) bool {
			if requested.Update(e) {
				return true
			}
			before, ok1 := e.ObjectOld.(configbutleraiv1alpha3.ApplyTrigger)
			after, ok2 := e.ObjectNew.(configbutleraiv1alpha3.ApplyTrigger)
			return !ok1 || !ok2 || operationPhase(before) != operationPhase(after)
		},
	}
}

// operationPhase is a trigger's status.operation.phase, or "" without one.
func operationPhase(trigger configbutleraiv1alpha3.ApplyTrigger) configbutleraiv1alpha3.ApplyOperationPhase {
	if operation := trigger.Operation(); operation != nil {
		return operation.Phase
	}
	return ""
}

// listTriggers lists this reconciler's kind, in namespace or everywhere when it is empty.
func (r *ApplyTriggerReconciler) listTriggers(
	ctx context.Context, namespace string,
) ([]configbutleraiv1alpha3.ApplyTrigger, error) {
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	return applier.ListApplyTriggers(ctx, r.Client, r.Kind, opts...)
}

// triggerLineChanged passes the event of a trigger that is gone, and an update that can move it in
// or out of line for its applier: its finalizer, its Configured condition, or its Held one.
func triggerLineChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(configbutleraiv1alpha3.ApplyTrigger)
			after, ok2 := e.ObjectNew.(configbutleraiv1alpha3.ApplyTrigger)
			return !ok1 || !ok2 || lineSummary(before) != lineSummary(after)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// lineSummary is what of a trigger decides its place in line for its applier.
func lineSummary(trigger configbutleraiv1alpha3.ApplyTrigger) string {
	var summary strings.Builder
	summary.WriteString(strconv.FormatBool(applier.Holding(trigger)))
	for _, conditionType := range []string{ApplyTriggerConditionConfigured, ApplyTriggerConditionHeld} {
		if cond := findCondition(*trigger.StatusConditions(), conditionType); cond != nil {
			summary.WriteString("|" + string(cond.Status) + "/" + cond.Reason)
		}
	}
	return summary.String()
}

// gitTargetToTriggers enqueues the triggers of this kind a GitTarget names, before and after an
// update, when it appears, goes, or changes anything eligibility reads: its spec, or its deletion.
func (r *ApplyTriggerReconciler) gitTargetToTriggers() handler.EventHandler {
	named := func(obj client.Object) []ctrlreconcile.Request {
		target, ok := obj.(*configbutleraiv1alpha3.GitTarget)
		if !ok || target.Spec.ApplyTriggerRef == nil || target.Spec.ApplyTriggerRef.Kind != r.Kind {
			return nil
		}
		return []ctrlreconcile.Request{{NamespacedName: k8stypes.NamespacedName{
			Namespace: target.Namespace, Name: target.Spec.ApplyTriggerRef.Name,
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

// gitDestinationToTriggers maps a GitBranch or GitProvider event to the triggers of this kind named
// by the GitTargets that write through it.
func (r *ApplyTriggerReconciler) gitDestinationToTriggers(
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
		ref := target.Spec.ApplyTriggerRef
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

// triggersInLine maps a trigger that is gone, or that moved in line, to the other triggers of this
// kind naming its applier, so the next in line takes it over.
func (r *ApplyTriggerReconciler) triggersInLine(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	gone, ok := obj.(configbutleraiv1alpha3.ApplyTrigger)
	if !ok || gone.TriggerKind() != r.Kind {
		return nil
	}
	others, err := applier.TriggersOfApplier(ctx, r.Client, gone)
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(others))
	for _, other := range others {
		requests = append(requests, triggerRequest(other))
	}
	return requests
}

// kubeProviderToTriggers maps a KubeProvider event to every trigger of this kind: a provider
// admits the triggers that name it, and the GitTargets that mirror through it, which may name any
// trigger in their namespace.
func (r *ApplyTriggerReconciler) kubeProviderToTriggers(
	ctx context.Context, obj client.Object,
) []ctrlreconcile.Request {
	triggers, err := r.listTriggers(ctx, "")
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(triggers))
	for _, trigger := range triggers {
		requests = append(requests, triggerRequest(trigger))
	}
	return requests
}

// namespaceToTriggers maps a Namespace label change to every trigger of this kind in it.
func (r *ApplyTriggerReconciler) namespaceToTriggers(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	triggers, err := r.listTriggers(ctx, obj.GetName())
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(triggers))
	for _, trigger := range triggers {
		requests = append(requests, triggerRequest(trigger))
	}
	return requests
}

func triggerRequest(trigger configbutleraiv1alpha3.ApplyTrigger) ctrlreconcile.Request {
	return ctrlreconcile.Request{NamespacedName: k8stypes.NamespacedName{
		Namespace: trigger.GetNamespace(), Name: trigger.GetName(),
	}}
}
