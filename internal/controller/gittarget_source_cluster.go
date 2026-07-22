// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
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

// auditRouteFor resolves the audit route this GitTarget's attribution facts are keyed under, from
// the referenced ClusterProvider's spec.attribution.auditRoute. It is read here, on the control
// plane, so the watch data plane can capture it on Declare and never needs a Kubernetes client of
// its own to attribute an event.
//
// An unreadable provider falls back to the provider NAME, which is exactly what AuditRoute()
// defaults to. So a transient read failure resolves the same route a provider that sets nothing
// would, and never a route that silently matches no facts.
func (r *GitTargetReconciler) auditRouteFor(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) string {
	name := target.SourceCluster()
	var cp configbutleraiv1alpha3.ClusterProvider
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: name}, &cp); err != nil {
		return name
	}
	return cp.AuditRoute()
}

// clusterProviderReadiness reads the referenced ClusterProvider's Ready condition and projects it,
// mirroring gitProviderReadiness. It returns Unknown — which does NOT downgrade Ready — when the
// provider cannot be observed (not found, or no Ready condition yet), so a not-yet-installed
// "default" provider never blocks a local GitTarget; only an explicit Ready=False downgrades.
func (r *GitTargetReconciler) clusterProviderReadiness(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) conditionValue {
	name := target.SourceCluster()
	notReady := func(status metav1.ConditionStatus, format string, args ...any) conditionValue {
		return conditionValue{
			Status:  status,
			Reason:  GitTargetReasonClusterProviderNotReady,
			Message: fmt.Sprintf(format, args...),
		}
	}

	var cp configbutleraiv1alpha3.ClusterProvider
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: name}, &cp); err != nil {
		return notReady(metav1.ConditionUnknown,
			"referenced ClusterProvider %q readiness not observed: %v", name, err)
	}
	c := findCondition(cp.Status.Conditions, ConditionTypeReady)
	switch {
	case c == nil:
		return notReady(metav1.ConditionUnknown, "referenced ClusterProvider %q has not reported readiness yet", name)
	case c.Status == metav1.ConditionTrue:
		return conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  GitTargetReasonClusterProviderReady,
			Message: fmt.Sprintf("referenced ClusterProvider %q is Ready", name),
		}
	case c.Status == metav1.ConditionFalse:
		if c.Message != "" {
			return notReady(metav1.ConditionFalse, "referenced ClusterProvider %q is not Ready: %s", name, c.Message)
		}
		return notReady(metav1.ConditionFalse, "referenced ClusterProvider %q is not Ready", name)
	default:
		// Ready=Unknown is the provider saying it does not know yet, which is exactly the case the
		// contract above refuses to downgrade on. Collapsing it into False would hold a GitTarget
		// down on a provider mid-reconcile.
		return notReady(metav1.ConditionUnknown, "referenced ClusterProvider %q readiness is unknown", name)
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
) conditionValue {
	key := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	notReady := func(status metav1.ConditionStatus, format string, args ...any) conditionValue {
		return conditionValue{
			Status:  status,
			Reason:  GitTargetReasonGitProviderNotReady,
			Message: fmt.Sprintf(format, args...),
		}
	}

	var gp configbutleraiv1alpha3.GitProvider
	if err := r.Get(ctx, key, &gp); err != nil {
		return notReady(metav1.ConditionUnknown, "referenced GitProvider %s readiness not observed: %v", key, err)
	}
	c := findCondition(gp.Status.Conditions, ConditionTypeReady)
	switch {
	case c == nil:
		return notReady(metav1.ConditionUnknown, "referenced GitProvider %s has not reported readiness yet", key)
	case c.Status == metav1.ConditionTrue:
		return conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  GitTargetReasonGitProviderReady,
			Message: fmt.Sprintf("referenced GitProvider %s is Ready", key),
		}
	case c.Message != "":
		return notReady(metav1.ConditionFalse, "referenced GitProvider %s is not Ready: %s", key, c.Message)
	default:
		return notReady(metav1.ConditionFalse, "referenced GitProvider %s is not Ready", key)
	}
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
