// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	reverserTypes "github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// WatchRule status condition reasons.
const (
	WatchRuleReasonValidating            = "Validating"
	WatchRuleReasonGitProviderNotFound   = "GitRepoConfigNotFound"
	WatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	WatchRuleReasonAccessDenied          = "AccessDenied"
	WatchRuleReasonGitTargetNotFound     = "GitTargetNotFound"
	WatchRuleReasonGitDestinationInvalid = "GitDestinationInvalid"
	WatchRuleReasonReady                 = ReasonSucceeded
	WatchRuleReasonResourcesResolved     = "Resolved"
	WatchRuleReasonUnresolvedResources   = "UnresolvedResources"
)

// WatchRuleReconciler reconciles a WatchRule object.
type WatchRuleReconciler struct {
	client.Client

	Scheme       *runtime.Scheme
	RuleStore    *rulestore.RuleStore
	WatchManager WatchManagerInterface
	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("WatchRuleReconciler")

	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the WatchRule instance
	var watchRule configbutleraiv1alpha3.WatchRule
	if err := r.Get(ctx, req.NamespacedName, &watchRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("WatchRule not found, was likely deleted", "namespacedName", req.NamespacedName)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.Delete(req.NamespacedName)
			log.Info("WatchRule deleted, removed from store", "name", req.Name, "namespace", req.Namespace)

			// The rule is gone, so the GitTarget it named cannot be read off it. Mark them all;
			// each one's pass is a cheap diff against a plan that has not moved.
			if r.WatchManager != nil {
				r.WatchManager.TriggerAllRuleChange()
			}

			// Stop publishing its conditions for the same reason the store entry goes: a series
			// that outlives its object reports Ready=False forever and never clears.
			telemetry.ForgetResourceConditions(conditionKindWatchRule, req.Namespace, req.Name)

			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch WatchRule", "namespacedName", req.NamespacedName)
		return ctrl.Result{}, err
	}

	log.Info("Starting WatchRule validation",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"target", watchRule.Spec.GitTargetRef,
		"generation", watchRule.Generation,
		"resourceVersion", watchRule.ResourceVersion)
	st := beginStatus(r.Client, r.Recorder, &watchRule)

	// Seed the axis conditions as not-yet-evaluated. There is deliberately no placeholder write of
	// the Ready/Reconciling/Stalled trio here: every path below ends in exactly one applyReadiness,
	// and a placeholder trio would be a second writer of the thing that must have only one.
	st.set(
		ConditionTypeStreamsRunning,
		metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady,
		"Blocked by validation; streams not evaluated",
	)
	st.set(
		ConditionTypeGitTargetReady,
		metav1.ConditionUnknown,
		ReasonProgressing,
		"Blocked by validation; GitTarget not evaluated",
	)
	st.set(
		ConditionTypeSourceNamespaceAuthorized,
		metav1.ConditionUnknown,
		ReasonProgressing,
		"Blocked by validation; source namespace not evaluated",
	)

	// Route by configuration surface (Target is required now)
	if watchRule.Spec.GitTargetRef.Name == "" {
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified",
		)
		return r.stallRule(ctx, st, WatchRuleReasonGitDestinationInvalid, "Target.name must be specified")
	}
	return r.reconcileWatchRuleViaTarget(ctx, st, &watchRule)
}

// reconcileWatchRuleViaTarget validates and stores a WatchRule that references a GitTarget.
// watchRuleGitTarget names the GitTarget a WatchRule writes through. gitTargetRef is a
// meta.LocalObjectReference, so the GitTarget is always in the rule's own namespace.
//
// It carries no UID, and that is correct rather than an omission: a rule-derived reference has
// none to carry, and the watch-plane owner resolves the trigger against the UID the GitTarget
// controller captured. See resolveGitTargetUID.
func watchRuleGitTarget(rule *configbutleraiv1alpha3.WatchRule) reverserTypes.ResourceReference {
	return reverserTypes.NewResourceReference(rule.Spec.GitTargetRef.Name, rule.Namespace)
}

func (r *WatchRuleReconciler) reconcileWatchRuleViaTarget(
	ctx context.Context,
	st *reconcileStatus,
	watchRule *configbutleraiv1alpha3.WatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileWatchRuleViaTarget")

	// Determine target namespace (same as WatchRule's namespace)
	targetNS := watchRule.Namespace

	// Fetch GitTarget
	var target configbutleraiv1alpha3.GitTarget
	targetKey := types.NamespacedName{Name: watchRule.Spec.GitTargetRef.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", watchRule.Spec.GitTargetRef.Name,
			"gitTargetNamespace", targetNS)
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitTargetNotFound,
			fmt.Sprintf("Referenced GitTarget '%s/%s' not found: %v", targetNS, watchRule.Spec.GitTargetRef.Name, err),
		)
		return r.stallRule(ctx, st, WatchRuleReasonGitTargetNotFound, "Referenced GitTarget not found")
	}
	ready := gitTargetReadyCondition(target)
	st.set(ConditionTypeGitTargetReady, ready.Status, ready.Reason, ready.Message)

	// Resolve the GitProvider named by the target. A meta.LocalObjectReference is a
	// name-only reference to a GitProvider in the GitTarget's own namespace.
	providerName := target.Spec.GitProviderRef.Name
	providerNS := target.Namespace // GitProvider is namespace-local to the GitTarget

	var provider configbutleraiv1alpha3.GitProvider
	providerKey := types.NamespacedName{Name: providerName, Namespace: providerNS}
	if err := r.Get(ctx, providerKey, &provider); err != nil {
		log.Error(err, "Failed to resolve GitProvider from GitTarget",
			"gitProviderName", providerName, "gitProviderNamespace", providerNS)
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitProviderNotFound,
			fmt.Sprintf("GitProvider '%s/%s' (from GitTarget) not found: %v", providerNS, providerName, err),
		)
		return r.stallRule(ctx, st, WatchRuleReasonGitProviderNotFound, "Referenced GitProvider not found")
	}

	// Source-namespace gate AND compilation, in that order and in one call — see
	// gateSourceNamespace. There is deliberately no AddOrUpdateWatchRule here: routing every
	// compilation through watch.CompileWatchRule is what stops the startup bootstrap from being a
	// second, ungated path into the store.
	if handled, result, err := r.gateSourceNamespace(ctx, st, watchRule, target, provider, log); handled {
		return result, err
	}

	// Submit the change and return. The pass runs on the watch-plane owner, after this target has
	// been quiet for the settle window, so the stream summary read below describes the state from
	// BEFORE it — see the status contract in docs/design/watch-manager-ownership.md.
	if r.WatchManager != nil {
		r.WatchManager.TriggerRuleChange(watchRuleGitTarget(watchRule))
		r.setResourceResolutionCondition(ctx, st, watchRule)
		r.setStreamsReadyCondition(st, watchRule, r.WatchManager.StreamSummaryForWatchRule(*watchRule))
	} else {
		r.setStreamsReadyCondition(st, watchRule, noResolvedStreamsSummary())
	}

	log.Info("WatchRule reconciliation via GitTarget successful", "name", watchRule.Name)

	msg := fmt.Sprintf(
		"WatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		targetNS,
		watchRule.Spec.GitTargetRef.Name,
	)
	return commitRule(ctx, st, ruleReadiness(watchRule.Status.Conditions, "WatchRule", msg))
}

// stallRule publishes a terminal WatchRule outcome and ends the reconcile.
func (r *WatchRuleReconciler) stallRule(
	ctx context.Context,
	st *reconcileStatus,
	reason, message string,
) (ctrl.Result, error) {
	rd := newRuleReadiness("WatchRule", "")
	rd.stalled(reason, message)
	return commitRule(ctx, st, rd)
}

func (r *WatchRuleReconciler) setResourceResolutionCondition(
	ctx context.Context,
	st *reconcileStatus,
	watchRule *configbutleraiv1alpha3.WatchRule,
) {
	resolved, message := r.WatchManager.ResolveWatchRuleResources(ctx, *watchRule)
	status := metav1.ConditionFalse
	reason := WatchRuleReasonUnresolvedResources
	if resolved {
		status = metav1.ConditionTrue
		reason = WatchRuleReasonResourcesResolved
	}
	st.set(ConditionTypeResourcesResolved, status, reason, message)
}

func (r *WatchRuleReconciler) setStreamsReadyCondition(
	st *reconcileStatus,
	watchRule *configbutleraiv1alpha3.WatchRule,
	streams watch.StreamSummary,
) {
	watchRule.Status.Streams = watchRuleStreamsStatus(streams)
	st.set(ConditionTypeStreamsRunning, streamConditionStatus(streams), streams.Reason, streams.Message)
}

// SetupWithManager sets up the controller with the Manager.
func (r *WatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		// A For() predicate is not an optimisation here, it closes a self-triggering edge: a status
		// write bumps resourceVersion and fires an Update watch event that EnqueueRequestForObject
		// turns straight back into a queued request, un-rate-limited. reconcileStatus.commit()
		// already suppresses no-op writes, so the loop has no fuel; this makes it structural, and
		// matches what GitProvider and ClusterProvider already do.
		For(&configbutleraiv1alpha3.WatchRule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// This rule MIRRORS the GitTarget's readiness into its own GitTargetReady, so the watch has
		// to see the status-only update that carries a heal — GenerationChangedPredicate would drop
		// exactly that. gitTargetReadyProjectionChanged fires on a spec change or on a move in the
		// mirrored verdict, and on nothing else.
		Watches(
			&configbutleraiv1alpha3.GitTarget{},
			handler.EnqueueRequestsFromMapFunc(r.gitTargetToWatchRules),
			builder.WithPredicates(gitTargetReadyProjectionChanged()),
		).
		// Nothing of the GitProvider's STATUS is mirrored here, so GenerationChangedPredicate is
		// right on this edge: it reacts to a freshly applied or spec-changed provider while ignoring
		// the status-only updates its controller writes itself — without it every GitProvider
		// heartbeat would re-list and re-enqueue all WatchRules.
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToWatchRules),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to a ClusterProvider's allowAnySourceNamespace (or accessFrom)
		// changing. The GitTarget->WatchRules edge above still cannot be relied on to carry this: a
		// ClusterProvider change reaches the GitTarget as a status update that need not move the
		// GitTarget's readiness at all, and only a readiness move re-enqueues the rules. Without
		// this mapper, flipping the delegation flag would leave every affected WatchRule
		// un-reconciled until its periodic requeue — so a REVOCATION would take minutes.
		Watches(
			&configbutleraiv1alpha3.ClusterProvider{},
			handler.EnqueueRequestsFromMapFunc(r.clusterProviderToWatchRules),
			builder.WithPredicates(clusterProviderReadyOrSpecChanged()),
		).
		Named("watchrule")

	if r.WatchManager != nil {
		// React to a stream of this rule's GitTarget reaching or leaving Streaming. Without it a
		// rule whose streams came up two seconds ago keeps publishing StreamsRunning=False until
		// its 10s settle requeue, because nothing tells it otherwise.
		if events := r.WatchManager.StreamStateEvents(); events != nil {
			b = b.WatchesRawSource(source.Channel(
				events,
				handler.EnqueueRequestsFromMapFunc(r.gitTargetToWatchRules),
			))
		}
	}

	return b.Complete(r)
}

// clusterProviderToWatchRules maps a ClusterProvider change to every WatchRule whose GitTarget
// mirrors through that provider, so a delegation grant or revocation converges on the event rather
// than on the periodic reconcile.
func (r *WatchRuleReconciler) clusterProviderToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	// A WatchRule's gitTargetRef is a meta.LocalObjectReference, so candidates always live in their
	// GitTarget's own namespace — collect the affected (namespace, target name) pairs.
	affected := make(map[types.NamespacedName]struct{}, len(targets.Items))
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.SourceCluster() != obj.GetName() {
			continue
		}
		affected[types.NamespacedName{Name: t.Name, Namespace: t.Namespace}] = struct{}{}
	}
	if len(affected) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		key := types.NamespacedName{Name: rule.Spec.GitTargetRef.Name, Namespace: rule.Namespace}
		if _, ok := affected[key]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}

// gitTargetToWatchRules maps a GitTarget event to every WatchRule in the
// GitTarget's namespace that references it. WatchRule.spec.gitTargetRef is a
// meta.LocalObjectReference, so candidates only live in the same namespace as the
// GitTarget.
func (r *WatchRuleReconciler) gitTargetToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.GitTargetRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}

// gitProviderToWatchRules maps a GitProvider event to every WatchRule (in the
// GitProvider's namespace) whose referenced GitTarget points at this provider.
// Mirrors the equivalent helper on ClusterWatchRuleReconciler so that an
// arriving provider doesn't have to wait for a separate GitTarget event to
// reach the rule.
func (r *WatchRuleReconciler) gitProviderToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	matchingTargets := make(map[string]struct{})
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.Spec.GitProviderRef.Name == obj.GetName() {
			matchingTargets[t.Name] = struct{}{}
		}
	}
	if len(matchingTargets) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		if _, ok := matchingTargets[rule.Spec.GitTargetRef.Name]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}
