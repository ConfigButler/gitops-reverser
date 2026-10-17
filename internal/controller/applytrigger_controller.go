// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
// Argo CD Application) has automated sync off. The trigger's ClusterProvider must admit its
// namespace first.
const ApplyTriggerConditionConfigured = "Configured"

// ReasonOperationUnresolved is Ready=False on a trigger whose operation has not completed in the
// time it was given. It clears when the operation completes.
const ReasonOperationUnresolved = "OperationUnresolved"

// applierCheckTimeout bounds the Configured check: at most two GETs and two access reviews
// against the applier's cluster. Its client has no request timeout, because it also carries the
// watches, and a cluster that accepts the connection and never answers would otherwise hold this
// controller's worker, not only this trigger.
const applierCheckTimeout = 10 * time.Second

// Why the trigger reconciler reads the applier on every reconcile rather than watching it: Flux or
// Argo CD may not be installed in the applier's cluster at all, and an informer on a kind that is
// not served would be a failure to report rather than a check that answers "not found". The
// cadence is its own: RequeueSteadyInterval, or applyTriggerSettleInterval while something is
// expected to clear on its own, such as an applier created after its trigger.

// applyTriggerSettleInterval is how soon a trigger that is not Ready, and not stalled, is read again.
const applyTriggerSettleInterval = 15 * time.Second

// ApplyTriggerReconciler reconciles one apply trigger kind: it checks that the applier the trigger
// holds is one GitOps Reverser can drive, and publishes what it applies (status.source) for the
// GitTargets naming the trigger. It never starts an apply: the branch workers decide when, and
// write status.operation and status.repairOwed themselves.
type ApplyTriggerReconciler struct {
	client.Client

	// Kind is the trigger kind this reconciler serves (v1alpha3.ApplyTriggerKind*).
	Kind     string
	Recorder record.EventRecorder

	// ApplierClients resolves the dynamic client of a trigger's cluster. Nil leaves Configured
	// Unknown (tests, and the CLI).
	ApplierClients applier.SourceClients
	// newApplier builds the client that checks an applier of a kind; nil is applier.NewClient. A
	// test sets it to answer the access review without a real API server.
	newApplier func(kind string, client dynamic.Interface) applier.Client
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers;argocdtriggers,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=fluxtriggers/status;argocdtriggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile publishes one trigger's Configured condition, its source and the readiness trio.
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
	configured, source, checked := r.configuredVerdict(ctx, trigger)
	st.setValue(ApplyTriggerConditionConfigured, configured)
	if checked {
		setTriggerSource(trigger, source)
	}

	applierRef := trigger.Applier()
	rd := newReadiness(
		fmt.Sprintf("%s %s %s can be asked to apply", trigger.ApplierKind(), applierRef, triggerSourceSummary(source)),
		trigger.TriggerKind()+" is not stalled")
	applyTriggerReadinessGates(rd, configured, trigger.Operation())
	st.applyReadiness(rd)

	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	requeue := RequeueSteadyInterval
	if rd.converging() {
		requeue = applyTriggerSettleInterval
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(requeue)}, nil
}

// configuredVerdict checks the trigger's applier in its cluster, and returns the source it read, and
// whether the check was made. A check that could not be made keeps the verdict and the source
// already published, since a blip reading that cluster says nothing about the applier; with nothing
// published it is Unknown.
func (r *ApplyTriggerReconciler) configuredVerdict(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger,
) (conditionValue, *applier.Source, bool) {
	kind, ref, cluster := trigger.ApplierKind(), trigger.Applier(), trigger.ClusterProvider()
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()

	decision, err := authz.ApplyTriggerAdmitted(ctx, r.Client, trigger)
	if err != nil {
		return keptConfigured(trigger, fmt.Sprintf("Cannot check ClusterProvider %q: %v", cluster, err)), nil, false
	}
	if !decision.Allowed {
		return conditionValue{Status: metav1.ConditionFalse, Reason: decision.Reason, Message: decision.Message},
			nil, true
	}
	if r.ApplierClients == nil {
		return conditionValue{
			Status:  metav1.ConditionUnknown,
			Reason:  GitTargetReasonNotChecked,
			Message: fmt.Sprintf("%s %s has not been checked: no cluster client is wired", kind, ref),
		}, nil, false
	}
	dynamicClient, err := r.ApplierClients(ctx, cluster)
	if err != nil {
		return keptConfigured(trigger,
			fmt.Sprintf("Cannot reach cluster %q to check %s %s: %v", cluster, kind, ref, err)), nil, false
	}
	newApplier := r.newApplier
	if newApplier == nil {
		newApplier = applier.NewClient
	}
	verdict := newApplier(kind, dynamicClient).Validate(ctx, ref)
	switch {
	case verdict.OK:
		return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded, Message: verdict.Message},
			verdict.Source, true
	case verdict.Err != nil:
		return keptConfigured(trigger, verdict.Message), nil, false
	default:
		return conditionValue{Status: metav1.ConditionFalse, Reason: verdict.Reason, Message: verdict.Message},
			verdict.Source, true
	}
}

// keptConfigured is the Configured value a check that could not be made publishes.
func keptConfigured(trigger configbutleraiv1alpha3.ApplyTrigger, message string) conditionValue {
	if cond := findCondition(*trigger.StatusConditions(), ApplyTriggerConditionConfigured); cond != nil {
		return conditionValue{Status: cond.Status, Reason: cond.Reason, Message: cond.Message}
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonProgressing, Message: message}
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

// triggerSourceSummary names what an applier applies, for the Ready message.
func triggerSourceSummary(source *applier.Source) string {
	if source == nil {
		return ""
	}
	return fmt.Sprintf("(revision %q of %s)", source.Revision, source.URL)
}

// applyTriggerReadinessGates contributes Configured and the operation to the trio, in that order.
// An applier that is missing, or a ClusterProvider that is, may be on its way (Flux and Argo CD
// objects are often created after the trigger); anything else Configured=False needs a human. An
// operation that has not completed in time clears on its own once it does.
func applyTriggerReadinessGates(
	rd *readiness, configured conditionValue, operation *configbutleraiv1alpha3.ApplyOperation,
) {
	waits := configured.Reason == applier.ReasonApplierNotFound ||
		configured.Reason == authz.ReasonClusterProviderNotFound
	rd.stalledIf(configured.Status == metav1.ConditionFalse && !waits, configured.Reason, configured.Message)
	rd.progressingIf(configured.Status == metav1.ConditionFalse, metav1.ConditionFalse,
		configured.Reason, configured.Message)
	rd.progressingIf(configured.Status == metav1.ConditionUnknown, metav1.ConditionUnknown,
		configured.Reason, configured.Message)
	if operation != nil && operation.Phase == configbutleraiv1alpha3.ApplyOperationUnresolved {
		rd.progressing(metav1.ConditionFalse, ReasonOperationUnresolved, fmt.Sprintf(
			"the %s apply of %s, asked for at %s, has not completed: %s", operation.Reason,
			describeCommit(operation.Commit), operation.RequestedAt.UTC().Format(time.RFC3339), operation.Message))
	}
}

// describeCommit names a commit in a message: its short hash, or what an empty one means.
func describeCommit(commit string) string {
	const short = 8
	switch {
	case commit == "":
		return "the branch, which the remote does not carry"
	case len(commit) > short:
		return commit[:short]
	default:
		return commit
	}
}

// SetupWithManager sets up the controller for its kind with the Manager.
func (r *ApplyTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	trigger := configbutleraiv1alpha3.NewApplyTrigger(r.Kind)
	if trigger == nil {
		return fmt.Errorf("no apply trigger kind %q", r.Kind)
	}
	object, ok := trigger.(client.Object)
	if !ok {
		return fmt.Errorf("apply trigger kind %q is not a client.Object", r.Kind)
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The status is written by two parties: this reconciler (conditions, source) and the branch
		// workers (operation, repairOwed). A status-only update is filtered, as everywhere, except
		// one that moves the operation's phase, which Ready reads.
		For(object, builder.WithPredicates(applyTriggerSpecOrOperationChanged())).
		// The trigger's ClusterProvider admits its namespace, so a provider created, deleted, or with
		// its accessFrom changed re-runs the check.
		Watches(
			&configbutleraiv1alpha3.ClusterProvider{},
			handler.EnqueueRequestsFromMapFunc(r.clusterProviderToTriggers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// An accessFrom selector reads namespace labels.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToTriggers),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
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
	var triggers []configbutleraiv1alpha3.ApplyTrigger
	switch r.Kind {
	case configbutleraiv1alpha3.ApplyTriggerKindFlux:
		var list configbutleraiv1alpha3.FluxTriggerList
		if err := r.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			triggers = append(triggers, &list.Items[i])
		}
	case configbutleraiv1alpha3.ApplyTriggerKindArgoCD:
		var list configbutleraiv1alpha3.ArgoCDTriggerList
		if err := r.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			triggers = append(triggers, &list.Items[i])
		}
	}
	return triggers, nil
}

// clusterProviderToTriggers maps a ClusterProvider event to every trigger of this kind that names it.
func (r *ApplyTriggerReconciler) clusterProviderToTriggers(
	ctx context.Context, obj client.Object,
) []ctrlreconcile.Request {
	triggers, err := r.listTriggers(ctx, "")
	if err != nil {
		logDependencyListError(ctx, err, r.Kind+"s", obj)
		return nil
	}
	var requests []ctrlreconcile.Request
	for _, trigger := range triggers {
		if trigger.ClusterProvider() == obj.GetName() {
			requests = append(requests, triggerRequest(trigger))
		}
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
