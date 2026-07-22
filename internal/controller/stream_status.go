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
	return &configbutleraiv1alpha3.WatchRuleStreamsStatus{
		Summary:       streams.Summary(),
		Total:         clampIntToInt32(streams.Total),
		Ready:         clampIntToInt32(streams.Ready),
		Replaying:     clampIntToInt32(streams.Replaying),
		Blocked:       clampIntToInt32(streams.Blocked),
		PendingSample: append([]string(nil), streams.PendingSample...),
	}
}

// newRuleReadiness starts the accumulator that owns a rule kind's kstatus trio.
func newRuleReadiness(kind, readyMessage string) *readiness {
	return newReadiness(readyMessage, kind+" is not stalled")
}

// ruleReadiness derives a rule's trio from the axis conditions the reconcile already published.
//
// Both rule kinds share it because they share the axes: resources resolved, the referenced
// GitTarget's health, whether streams are running, and — WatchRule only — whether the effective
// source namespace is authorized. ClusterWatchRule simply carries no SourceNamespaceAuthorized
// condition, so that gate is absent rather than special-cased.
//
// The order of the contributions below IS the precedence; see readiness.
func ruleReadiness(conditions []metav1.Condition, kind, readyMessage string) *readiness {
	rd := newRuleReadiness(kind, readyMessage)

	resources := findCondition(conditions, ConditionTypeResourcesResolved)
	gitTarget := findCondition(conditions, ConditionTypeGitTargetReady)
	streams := findCondition(conditions, ConditionTypeStreamsRunning)
	// Absent on ClusterWatchRule, which does not carry this condition; nil simply never gates.
	sourceNS := findCondition(conditions, ConditionTypeSourceNamespaceAuthorized)

	// A source-authorization REFUSAL outranks every other cause: the rule is not running and no
	// amount of waiting changes that, so it must not be reported as merely progressing behind an
	// unresolved type or an unready target. Only False is terminal here — Unknown is the
	// deliberately non-terminal "cannot say yet", handled among the progressing gates below.
	contributeStalled(rd, sourceNS)
	contributeStalled(rd, resources)
	if gitTarget != nil && gitTargetReadyReasonIsStalled(gitTarget.Reason) {
		contributeStalled(rd, gitTarget)
	}
	if streams != nil && streamReasonIsStalled(streams.Reason) {
		contributeStalled(rd, streams)
	}

	// Source authorization is an ADDITIONAL prerequisite of Ready, so Ready=True means the source
	// namespace is authorized AND the GitTarget is ready AND resources resolved AND streams are
	// running — never merely that the gate passed. Unknown yields InProgress here rather than a
	// separate status path, which is what makes the retained-scope and cache-syncing cases
	// representable without inventing a second readiness model.
	contributeProgressing(rd, sourceNS, ReasonProgressing, "Establishing source-namespace authorization")
	contributeProgressing(rd, gitTarget, ReasonProgressing, "Waiting for GitTarget to become ready")
	contributeProgressing(rd, streams, ReasonProgressing, "Waiting for streams to run")
	return rd
}

// contributeStalled contributes a present-and-False condition as a terminal gate.
func contributeStalled(rd *readiness, condition *metav1.Condition) {
	if condition != nil && condition.Status == metav1.ConditionFalse {
		rd.stalled(condition.Reason, condition.Message)
	}
}

// contributeProgressing contributes any condition that is not yet True as a transient gate, falling
// back to the given reason and message when the condition itself carries neither.
func contributeProgressing(rd *readiness, condition *metav1.Condition, reason, message string) {
	if condition == nil || condition.Status == metav1.ConditionTrue {
		return
	}
	if condition.Reason != "" {
		reason = condition.Reason
	}
	if condition.Message != "" {
		message = condition.Message
	}
	rd.progressing(metav1.ConditionFalse, reason, message)
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
