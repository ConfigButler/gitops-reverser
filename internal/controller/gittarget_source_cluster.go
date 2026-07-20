// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// GitTargetReasonNamespaceNotAuthorized is the Validated=False reason when a GitTarget's namespace
// is not admitted by its referenced ClusterProvider's spec.allowedNamespaces. It runs on every
// reconcile, so a policy tightened AFTER a GitTarget was created stops that target's watches too.
const GitTargetReasonNamespaceNotAuthorized = authz.ReasonNamespaceNotAuthorized

// GitTargetReasonClusterProviderNotFound is the Validated=False reason when a GitTarget's
// referenced ClusterProvider does not exist. This is a HARD GATE: a GitTarget may mirror a source
// cluster ONLY through an existing ClusterProvider, "default" included. The operator never creates
// one, so a target whose provider was never declared is held NotReady and its data plane stopped
// rather than mirroring on an implicit local identity.
const GitTargetReasonClusterProviderNotFound = authz.ReasonClusterProviderNotFound

// checkSourceAuthorization is the GitTarget reconciler's view of the shared source-cluster gate:
// the referenced ClusterProvider must exist, and it must admit the GitTarget's namespace. The
// decision itself lives in internal/authz because the ClusterWatchRule reconciler and the watch
// manager's bootstrap must reach the SAME verdict — see authz.GitTargetAdmitted.
//
// This caller runs it inside the Validated gate and returns BEFORE DeclareForGitTarget, so an
// unauthorized GitTarget never starts a watch and never writes to Git. It returns authorized=false
// with a legible reason on denial; a non-NotFound read error is returned as err so the reconcile
// requeues rather than tearing down a running data plane on a transient apiserver failure.
func (r *GitTargetReconciler) checkSourceAuthorization(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (bool, string, string, error) {
	decision, err := authz.GitTargetAdmitted(ctx, r.Client, target)
	if err != nil {
		return false, "", "", err
	}
	if !decision.Allowed {
		return false, decision.Reason, decision.Message, nil
	}
	return true, "", "", nil
}

const (
	// GitTargetConditionSourceClusterReachable is the RUNTIME reachability of the source
	// cluster a GitTarget mirrors from: True (reason LocalCluster) when kubeConfig is omitted,
	// Unknown before the data plane's first discovery, False after a real failed attempt. It is
	// distinct from Validated: a missing/malformed kubeconfig is Validated=False (an input the
	// controller reads without a network dial); an otherwise-valid kubeconfig whose API server
	// cannot be contacted is SourceClusterReachable=False.
	GitTargetConditionSourceClusterReachable = "SourceClusterReachable"
	// GitTargetConditionGitProviderReady projects the referenced GitProvider's Ready onto the
	// GitTarget, so one `kubectl get gittarget` separates source-side (SourceClusterReachable)
	// from destination-side (GitProviderReady) failure.
	GitTargetConditionGitProviderReady = "GitProviderReady"

	// GitTargetReasonGitProviderNotReady is the GitProviderReady=False reason.
	GitTargetReasonGitProviderNotReady = "GitProviderNotReady"
	// GitTargetReasonGitProviderReady is the GitProviderReady=True reason.
	GitTargetReasonGitProviderReady = "GitProviderReady"

	// GitTargetConditionClusterProviderReady projects the referenced ClusterProvider's Ready onto
	// the GitTarget, so one `kubectl get gittarget` shows whether the SOURCE cluster's provider is
	// healthy — distinct from SourceClusterReachable (the data plane's runtime reach) and
	// GitProviderReady (the destination). It follows the GitProviderReady contract: only an
	// EXPLICIT Ready=False downgrades the GitTarget; a not-found or not-yet-reported provider is
	// Unknown and does not (so a single-cluster install that has not yet installed the "default"
	// provider is not held down).
	GitTargetConditionClusterProviderReady = "ClusterProviderReady"
	// GitTargetReasonClusterProviderNotReady is the ClusterProviderReady=False/Unknown reason.
	GitTargetReasonClusterProviderNotReady = "ClusterProviderNotReady"
	// GitTargetReasonClusterProviderReady is the ClusterProviderReady=True reason.
	GitTargetReasonClusterProviderReady = "ClusterProviderReady"
)

// clusterProviderReadiness reads the referenced ClusterProvider's Ready condition and projects it,
// mirroring gitProviderReadiness. It returns Unknown — which does NOT downgrade Ready — when the
// provider cannot be observed (not found, or no Ready condition yet), so a not-yet-installed
// "default" provider never blocks a local GitTarget; only an explicit Ready=False downgrades.
func (r *GitTargetReconciler) clusterProviderReadiness(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (metav1.ConditionStatus, string, string) {
	name := target.SourceCluster()
	var cp configbutleraiv1alpha3.ClusterProvider
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: name}, &cp); err != nil {
		return metav1.ConditionUnknown, GitTargetReasonClusterProviderNotReady,
			fmt.Sprintf("referenced ClusterProvider %q readiness not observed: %v", name, err)
	}
	c := findCondition(cp.Status.Conditions, ConditionTypeReady)
	switch {
	case c == nil:
		return metav1.ConditionUnknown, GitTargetReasonClusterProviderNotReady,
			fmt.Sprintf("referenced ClusterProvider %q has not reported readiness yet", name)
	case c.Status == metav1.ConditionTrue:
		return metav1.ConditionTrue, GitTargetReasonClusterProviderReady,
			fmt.Sprintf("referenced ClusterProvider %q is Ready", name)
	case c.Status == metav1.ConditionFalse:
		msg := fmt.Sprintf("referenced ClusterProvider %q is not Ready", name)
		if c.Message != "" {
			msg = fmt.Sprintf("referenced ClusterProvider %q is not Ready: %s", name, c.Message)
		}
		return metav1.ConditionFalse, GitTargetReasonClusterProviderNotReady, msg
	default:
		// Ready=Unknown is the provider saying it does not know yet, which is exactly the case the
		// contract above refuses to downgrade on. Collapsing it into False would hold a GitTarget
		// down on a provider mid-reconcile.
		return metav1.ConditionUnknown, GitTargetReasonClusterProviderNotReady,
			fmt.Sprintf("referenced ClusterProvider %q readiness is unknown", name)
	}
}

// describeKubeConfigKey renders the resolved-key hint for a "key not found" message.
func describeKubeConfigKey(specKey string) string {
	if specKey == "" {
		return "value or value.yaml"
	}
	return specKey
}

// gitProviderReadiness reads the referenced GitProvider's Ready condition and projects it. It
// runs only after the Validated gate has confirmed the provider exists. It returns Unknown —
// which does NOT downgrade Ready — when the provider's readiness cannot be observed (a transient
// read error, or a provider that has not reported a Ready condition yet), so a not-yet-reconciled
// provider never blocks its GitTarget; only an EXPLICIT Ready=False downgrades.
func (r *GitTargetReconciler) gitProviderReadiness(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (metav1.ConditionStatus, string, string) {
	var gp configbutleraiv1alpha3.GitProvider
	key := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, key, &gp); err != nil {
		return metav1.ConditionUnknown, GitTargetReasonGitProviderNotReady,
			fmt.Sprintf("referenced GitProvider %s readiness not observed: %v", key, err)
	}
	c := findCondition(gp.Status.Conditions, ConditionTypeReady)
	switch {
	case c == nil:
		return metav1.ConditionUnknown, GitTargetReasonGitProviderNotReady,
			fmt.Sprintf("referenced GitProvider %s has not reported readiness yet", key)
	case c.Status == metav1.ConditionTrue:
		return metav1.ConditionTrue, GitTargetReasonGitProviderReady,
			fmt.Sprintf("referenced GitProvider %s is Ready", key)
	default:
		msg := fmt.Sprintf("referenced GitProvider %s is not Ready", key)
		if c.Message != "" {
			msg = fmt.Sprintf("referenced GitProvider %s is not Ready: %s", key, c.Message)
		}
		return metav1.ConditionFalse, GitTargetReasonGitProviderNotReady, msg
	}
}

// findCondition returns the named condition, or nil.
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// projectSourceAndProvider sets the SourceClusterReachable and GitProviderReady conditions and
// folds them into the aggregate. It only ever DOWNGRADES Ready — a source-side or
// destination-side problem holds the target below Ready, but a healthy pair never overrides a
// data-plane stall (e.g. a still-replaying stream). Precedence: destination first
// (GitProviderReady, a stall the operator must fix), then source reachability (a transient the
// data plane retries — False is progressing, Unknown holds Ready at Unknown).
func (r *GitTargetReconciler) projectSourceAndProvider(
	target *configbutleraiv1alpha3.GitTarget,
	sourceReach watch.SourceClusterReachableStatus,
	providerStatus metav1.ConditionStatus,
	providerReason, providerMessage string,
	clusterProviderStatus metav1.ConditionStatus,
	clusterProviderReason, clusterProviderMessage string,
) {
	reachStatus := conditionStatusFromString(sourceReach.State)
	r.setCondition(
		target,
		GitTargetConditionSourceClusterReachable,
		reachStatus,
		sourceReach.Reason,
		sourceReach.Message,
	)
	r.setCondition(target, GitTargetConditionGitProviderReady, providerStatus, providerReason, providerMessage)
	r.setCondition(target, GitTargetConditionClusterProviderReady, clusterProviderStatus,
		clusterProviderReason, clusterProviderMessage)

	switch {
	case providerStatus == metav1.ConditionFalse:
		// Destination-side: the provider's own periodic check is failing. Wait for it to
		// recover (progressing), which the Watches(&GitProvider{}) trigger promptly re-runs.
		r.downgradeReady(target, metav1.ConditionFalse, providerReason, providerMessage)
	case clusterProviderStatus == metav1.ConditionFalse:
		// Source-config side: the ClusterProvider explicitly reports itself not Ready (e.g. its
		// kubeconfig stopped validating). Progressing — the Watches(&ClusterProvider{}) trigger
		// re-runs this as the provider recovers.
		r.downgradeReady(target, metav1.ConditionFalse, clusterProviderReason, clusterProviderMessage)
	case reachStatus == metav1.ConditionFalse:
		// Source-side: an otherwise-valid kubeconfig whose API server cannot be contacted. A
		// transient the data plane retries, so this is progressing, not stalled.
		r.downgradeReady(target, metav1.ConditionFalse, sourceReach.Reason, sourceReach.Message)
	case reachStatus == metav1.ConditionUnknown:
		// An unconfirmed source is not yet Ready; hold Ready at Unknown until first discovery.
		r.downgradeReady(target, metav1.ConditionUnknown, sourceReach.Reason, sourceReach.Message)
	}
}

// downgradeReady lowers the aggregate below Ready without ever raising it: it is called only on a
// source/provider problem, and rewrites Ready plus the kstatus Reconciling/Stalled pair to match.
// Every such problem here is expected to clear on its own (a provider recovers, a source becomes
// reachable), so it is always PROGRESSING (Reconciling=True, Stalled=False) — a re-check via the
// Watches triggers converges it. A blocking, needs-a-human problem is handled by the setStalled*
// path in Reconcile instead, never here.
func (r *GitTargetReconciler) downgradeReady(
	target *configbutleraiv1alpha3.GitTarget,
	readyStatus metav1.ConditionStatus,
	reason, message string,
) {
	r.setCondition(target, GitTargetConditionReady, readyStatus, reason, message)
	r.setCondition(target, GitTargetConditionReconciling, metav1.ConditionTrue, reason, message)
	r.setCondition(target, GitTargetConditionStalled, metav1.ConditionFalse, ReasonProgressing,
		"Reconciliation is making progress")
}

// conditionStatusFromString maps the watch layer's "True"/"False"/"Unknown" onto the API type.
func conditionStatusFromString(state string) metav1.ConditionStatus {
	switch state {
	case "True":
		return metav1.ConditionTrue
	case "False":
		return metav1.ConditionFalse
	default:
		return metav1.ConditionUnknown
	}
}
