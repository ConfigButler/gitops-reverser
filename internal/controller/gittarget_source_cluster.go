// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

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
)

// validateKubeConfig is the legibility gate for spec.kubeConfig (extends Validated). It reads
// and parses the kubeconfig Secret from the config plane and applies the exec/TLS safety
// policy, but deliberately does NOT dial the cluster — reachability is a runtime observation
// the data plane records on SourceClusterReachable. It returns ok=false with a typed
// KubeConfig* reason and a legible message when an input is wrong; a non-NotFound read error is
// returned as err so the reconcile requeues rather than falsely reporting a bad input. An
// omitted kubeConfig is trivially valid — the local cluster.
func (r *GitTargetReconciler) validateKubeConfig(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (bool, string, string, error) {
	if target.Spec.KubeConfig == nil || target.Spec.KubeConfig.SecretRef == nil {
		return true, "", "", nil
	}
	ref := target.Spec.KubeConfig.SecretRef
	secretKey := k8stypes.NamespacedName{Namespace: target.Namespace, Name: ref.Name}

	var secret corev1.Secret
	if getErr := r.Get(ctx, secretKey, &secret); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, kubeconfig.ReasonSecretNotFound, fmt.Sprintf(
				"spec.kubeConfig.secretRef names Secret %s, which does not exist in this namespace",
				secretKey), nil
		}
		return false, "", "", fmt.Errorf("read kubeconfig Secret %s: %w", secretKey, getErr)
	}

	raw, usedKey, present := kubeconfig.ResolveKey(secret.Data, ref.Key)
	if !present {
		return false, kubeconfig.ReasonKeyNotFound, fmt.Sprintf(
			"kubeconfig Secret %s has no kubeconfig under key %q (set spec.kubeConfig.secretRef.key)",
			secretKey, describeKubeConfigKey(ref.Key)), nil
	}
	if rej := kubeconfig.Check(raw, r.KubeConfigSafety); rej != nil {
		// A RejectionError is a validation VERDICT, not a reconcile error: it is surfaced as
		// the Validated=False reason/message, and reconcile requeues normally (err == nil).
		//nolint:nilerr
		return false, rej.Reason, fmt.Sprintf("kubeconfig Secret %s key %q: %s", secretKey, usedKey, rej.Message), nil
	}
	return true, "", "", nil
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

	switch {
	case providerStatus == metav1.ConditionFalse:
		// Destination-side: the provider's own periodic check is failing. Wait for it to
		// recover (progressing), which the Watches(&GitProvider{}) trigger promptly re-runs.
		r.downgradeReady(target, metav1.ConditionFalse, providerReason, providerMessage, true)
	case reachStatus == metav1.ConditionFalse:
		// Source-side: an otherwise-valid kubeconfig whose API server cannot be contacted. A
		// transient the data plane retries, so this is progressing, not stalled.
		r.downgradeReady(target, metav1.ConditionFalse, sourceReach.Reason, sourceReach.Message, true)
	case reachStatus == metav1.ConditionUnknown:
		// An unconfirmed source is not yet Ready; hold Ready at Unknown until first discovery.
		r.downgradeReady(target, metav1.ConditionUnknown, sourceReach.Reason, sourceReach.Message, true)
	}
}

// downgradeReady lowers the aggregate below Ready without ever raising it: it is called only on
// a source/provider problem, and rewrites Ready plus the kstatus Reconciling/Stalled pair to
// match. progressing=true means the condition is expected to clear on its own (Reconciling),
// false means it needs action (Stalled).
func (r *GitTargetReconciler) downgradeReady(
	target *configbutleraiv1alpha3.GitTarget,
	readyStatus metav1.ConditionStatus,
	reason, message string,
	progressing bool,
) {
	r.setCondition(target, GitTargetConditionReady, readyStatus, reason, message)
	if progressing {
		r.setCondition(target, GitTargetConditionReconciling, metav1.ConditionTrue, reason, message)
		r.setCondition(target, GitTargetConditionStalled, metav1.ConditionFalse, ReasonProgressing,
			"Reconciliation is making progress")
		return
	}
	r.setCondition(target, GitTargetConditionReconciling, metav1.ConditionFalse, reason, "Reconciliation is stalled")
	r.setCondition(target, GitTargetConditionStalled, metav1.ConditionTrue, reason, message)
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
