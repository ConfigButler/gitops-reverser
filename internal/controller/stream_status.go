// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func streamConditionStatus(streams watch.StreamSummary) metav1.ConditionStatus {
	if streams.StreamsRunning() {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func noResolvedStreamsSummary() watch.StreamSummary {
	return watch.StreamSummary{
		Reason:  watch.StreamReasonNoResolvedTypes,
		Message: "0/0 streams running; no resolved resource types",
	}
}

func watchRuleStreamsStatus(streams watch.StreamSummary) *configbutleraiv1alpha3.WatchRuleStreamsStatus {
	observed := streams.ObservedTime
	if observed.IsZero() {
		observed = metav1.Now()
	}
	return &configbutleraiv1alpha3.WatchRuleStreamsStatus{
		Summary:       streams.Summary(),
		Total:         clampIntToInt32(streams.Total),
		Ready:         clampIntToInt32(streams.Ready),
		Replaying:     clampIntToInt32(streams.Replaying),
		Blocked:       clampIntToInt32(streams.Blocked),
		PendingSample: append([]string(nil), streams.PendingSample...),
		ObservedTime:  &observed,
	}
}

// ruleReadyReason is the reason every rule kind stamps on its Ready/Reconciling/Stalled trio once
// it is healthy. WatchRule and ClusterWatchRule both spell it "Ready", so it is a constant here
// rather than a parameter each caller passes identically.
const ruleReadyReason = "Ready"

func applyRuleKstatus(
	conditions []metav1.Condition,
	readyMessage string,
	notStalledMessage string,
	setCondition func(string, metav1.ConditionStatus, string, string),
	setStalled func(string, string),
) {
	resources := conditionByType(conditions, ConditionTypeResourcesResolved)
	gitTarget := conditionByType(conditions, ConditionTypeGitTargetReady)
	streams := conditionByType(conditions, ConditionTypeStreamsRunning)
	// Absent on ClusterWatchRule, which does not carry this condition; nil simply never gates.
	sourceNS := conditionByType(conditions, ConditionTypeSourceNamespaceAuthorized)

	if stalled := stalledRuleCondition(resources, gitTarget, streams, sourceNS); stalled != nil {
		setStalled(stalled.Reason, stalled.Message)
		return
	}
	// Source authorization is an ADDITIONAL prerequisite of Ready, so Ready=True means the source
	// namespace is authorized AND the GitTarget is ready AND resources resolved AND streams are
	// running — never merely that the gate passed. Unknown yields InProgress here rather than a
	// separate status path, which is what makes the retained-scope and cache-syncing cases
	// representable without inventing a second readiness model.
	if sourceNS != nil && sourceNS.Status != metav1.ConditionTrue {
		reason, message := conditionReasonMessage(
			sourceNS, ReasonProgressing, "Establishing source-namespace authorization")
		setRuleProgressing(reason, message, notStalledMessage, setCondition)
		return
	}
	if gitTarget != nil && gitTarget.Status != metav1.ConditionTrue {
		reason, message := conditionReasonMessage(gitTarget, ReasonProgressing, "Waiting for GitTarget to become ready")
		setRuleProgressing(reason, message, notStalledMessage, setCondition)
		return
	}

	switch {
	case streams != nil && streams.Status == metav1.ConditionTrue:
		setCondition(ConditionTypeReady, metav1.ConditionTrue, ruleReadyReason, readyMessage)
		setCondition(ConditionTypeReconciling, metav1.ConditionFalse, ruleReadyReason, "Reconciliation complete")
		setCondition(ConditionTypeStalled, metav1.ConditionFalse, ruleReadyReason, notStalledMessage)
	default:
		reason, message := ReasonProgressing, "Waiting for streams to run"
		if streams != nil {
			reason, message = streams.Reason, streams.Message
		}
		setRuleProgressing(reason, message, notStalledMessage, setCondition)
	}
}

func stalledRuleCondition(resources, gitTarget, streams, sourceNS *metav1.Condition) *metav1.Condition {
	switch {
	// A source-authorization REFUSAL outranks every other cause: the rule is not running and no
	// amount of waiting changes that, so it must not be reported as merely progressing behind an
	// unresolved type or an unready target. Only False is terminal here — Unknown is the
	// deliberately non-terminal "cannot say yet", handled by the caller as progressing.
	case sourceNS != nil && sourceNS.Status == metav1.ConditionFalse:
		return sourceNS
	case resources != nil && resources.Status == metav1.ConditionFalse:
		return resources
	case gitTarget != nil && gitTarget.Status == metav1.ConditionFalse && gitTargetReadyReasonIsStalled(gitTarget.Reason):
		return gitTarget
	case streams != nil && streams.Status == metav1.ConditionFalse && streamReasonIsStalled(streams.Reason):
		return streams
	default:
		return nil
	}
}

func setRuleProgressing(
	reason string,
	message string,
	notStalledMessage string,
	setCondition func(string, metav1.ConditionStatus, string, string),
) {
	setCondition(ConditionTypeReady, metav1.ConditionFalse, reason, message)
	setCondition(ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	setCondition(ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
}

func conditionReasonMessage(condition *metav1.Condition, defaultReason, defaultMessage string) (string, string) {
	if condition == nil {
		return defaultReason, defaultMessage
	}
	reason := condition.Reason
	if reason == "" {
		reason = defaultReason
	}
	message := condition.Message
	if message == "" {
		message = defaultMessage
	}
	return reason, message
}

func streamReasonIsStalled(reason string) bool {
	return reason == watch.StreamReasonWatchError || reason == watch.StreamReasonWatchNotPermitted
}

func gitTargetReadyReasonIsStalled(reason string) bool {
	switch reason {
	case GitTargetReasonProviderNotFound,
		GitTargetReasonBranchNotAllowed,
		GitTargetReasonTargetConflict,
		GitTargetReasonUnsupportedContent,
		GitTargetReasonIgnoreShadowsManagedPath,
		GitTargetReasonWriteBoundaryRefused,
		GitTargetReasonRenderDoesNotMatchLive,
		GitTargetReadyReasonValidationFailed,
		GitTargetReadyReasonEncryptionNotConfigured,
		GitTargetReadyReasonWorkerUnavailable,
		WatchRuleReasonGitTargetNotFound,
		WatchRuleReasonGitProviderNotFound,
		WatchRuleReasonGitDestinationInvalid,
		watch.StreamReasonWatchError,
		watch.StreamReasonWatchNotPermitted:
		return true
	default:
		return false
	}
}
