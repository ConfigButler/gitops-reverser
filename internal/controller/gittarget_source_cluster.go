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
// is not admitted by its referenced ClusterProvider's spec.accessFrom. It runs on every
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
	// GitTargetConditionGitBranchReady projects the referenced GitBranch's Ready onto the GitTarget,
	// and whether it holds its repository branch, so one `kubectl get gittarget` separates
	// source-side (SourceClusterReachable) from destination-side (GitBranchReady) failure.
	GitTargetConditionGitBranchReady = "GitBranchReady"

	// GitTargetReasonGitBranchNotReady is the GitBranchReady=False reason: the GitBranch exists but
	// does not hold its branch, or reports itself not Ready.
	GitTargetReasonGitBranchNotReady = "GitBranchNotReady"

	// GitTargetConditionClusterProviderReady projects the referenced ClusterProvider's Ready onto
	// the GitTarget, so one `kubectl get gittarget` shows whether the SOURCE cluster's provider is
	// healthy — distinct from SourceClusterReachable (the data plane's runtime reach) and
	// GitBranchReady (the destination). It follows the GitBranchReady contract: only an
	// EXPLICIT Ready=False downgrades the GitTarget; a provider that has not reported readiness yet
	// is Unknown and does not. A MISSING provider never reaches this projection at all — that is the
	// Validated gate's hard denial (GitTargetReasonClusterProviderNotFound), not a readiness axis.
	GitTargetConditionClusterProviderReady = "ClusterProviderReady"
	// GitTargetReasonClusterProviderNotReady is the ClusterProviderReady=False/Unknown reason.
	GitTargetReasonClusterProviderNotReady = "ClusterProviderNotReady"
)

// resolveSourceClusterProvider reads the ClusterProvider this GitTarget mirrors through, ONCE per
// reconcile, for the two consumers that need it: the audit route captured on Declare, and the
// ClusterProviderReady projection. It runs AFTER the Validated gate, which already proved through
// authz.GitTargetAdmitted that this provider exists and admits this namespace — so a failure here
// means the provider vanished (or the apiserver blinked) mid-reconcile, and it is returned as an
// error so the reconcile requeues.
//
// Requeuing rather than guessing a route is the point. spec.attribution.auditRoute may be ANY
// string, so a route inferred from the provider NAME is only correct for a provider that declares
// none: guess it for a provider that declares `shared-route` and this target's streams are declared
// under a partition the audit webhook never writes to. Attribution then resolves nobody — or, if
// some other provider is NAMED `shared-route`, resolves that cluster's facts and writes a wrong
// author into Git.
func (r *GitTargetReconciler) resolveSourceClusterProvider(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (*configbutleraiv1alpha3.ClusterProvider, error) {
	name := target.SourceCluster()
	var cp configbutleraiv1alpha3.ClusterProvider
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: name}, &cp); err != nil {
		return nil, fmt.Errorf("read source ClusterProvider %q: %w", name, err)
	}
	return &cp, nil
}

// clusterProviderReadiness projects the referenced ClusterProvider's Ready condition, mirroring
// gitProviderReadiness. It takes the provider resolveSourceClusterProvider already read rather than
// reading its own, so the readiness reported and the audit route declared always describe the same
// observation of the same object.
//
// Only an EXPLICIT Ready=False downgrades the GitTarget; a provider that has not reported readiness
// yet is Unknown, which does not.
func clusterProviderReadiness(cp *configbutleraiv1alpha3.ClusterProvider) conditionValue {
	name := cp.Name
	notReady := func(status metav1.ConditionStatus, format string, args ...any) conditionValue {
		return conditionValue{
			Status:  status,
			Reason:  GitTargetReasonClusterProviderNotReady,
			Message: fmt.Sprintf(format, args...),
		}
	}
	c := findCondition(cp.Status.Conditions, ConditionTypeReady)
	switch {
	case c == nil:
		return notReady(metav1.ConditionUnknown, "referenced ClusterProvider %q has not reported readiness yet", name)
	case c.Status == metav1.ConditionTrue:
		return conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  ReasonSucceeded,
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

// gitBranchReadiness projects the referenced GitBranch onto the GitTarget. held is whether it holds
// its repository branch now (the claim registry), which decides whether this target can be wired;
// the GitBranch's own Ready carries the reason when it cannot, and its message is repeated here.
// A GitBranch that holds its branch and reports Ready=False (its credential failing, a parent
// branch missing) is not Ready either: the target cannot publish through it.
func gitBranchReadiness(dest gitDestination, held bool) conditionValue {
	notReady := func(format string, args ...any) conditionValue {
		return conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  GitTargetReasonGitBranchNotReady,
			Message: fmt.Sprintf(format, args...),
		}
	}
	if dest.branch == nil {
		return notReady("referenced GitBranch %s not found", dest.key)
	}
	c := findCondition(dest.branch.Status.Conditions, ConditionTypeReady)
	switch {
	case !held && c != nil && c.Status == metav1.ConditionFalse && c.Message != "":
		return notReady("referenced GitBranch %s does not hold its branch: %s", dest.key, c.Message)
	case !held:
		return notReady("referenced GitBranch %s does not hold its branch yet", dest.key)
	case dest.provider == nil:
		return notReady("referenced GitBranch %s names a GitProvider that does not exist", dest.key)
	case c != nil && c.Status == metav1.ConditionFalse:
		return notReady("referenced GitBranch %s is not Ready: %s", dest.key, c.Message)
	}
	return conditionValue{
		Status:  metav1.ConditionTrue,
		Reason:  ReasonSucceeded,
		Message: fmt.Sprintf("referenced GitBranch %s holds its branch", dest.key),
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
