/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

func applyRuleKstatus(
	conditions []metav1.Condition,
	readyMessage string,
	readyReason string,
	notStalledMessage string,
	setCondition func(string, metav1.ConditionStatus, string, string),
	setStalled func(string, string),
) {
	resources := conditionByType(conditions, ConditionTypeResourcesResolved)
	gitTarget := conditionByType(conditions, ConditionTypeGitTargetReady)
	streams := conditionByType(conditions, ConditionTypeStreamsRunning)

	if stalled := stalledRuleCondition(resources, gitTarget, streams); stalled != nil {
		setStalled(stalled.Reason, stalled.Message)
		return
	}
	if gitTarget != nil && gitTarget.Status != metav1.ConditionTrue {
		reason, message := conditionReasonMessage(gitTarget, ReasonProgressing, "Waiting for GitTarget to become ready")
		setRuleProgressing(reason, message, notStalledMessage, setCondition)
		return
	}

	switch {
	case streams != nil && streams.Status == metav1.ConditionTrue:
		setCondition(ConditionTypeReady, metav1.ConditionTrue, readyReason, readyMessage)
		setCondition(ConditionTypeReconciling, metav1.ConditionFalse, readyReason, "Reconciliation complete")
		setCondition(ConditionTypeStalled, metav1.ConditionFalse, readyReason, notStalledMessage)
	default:
		reason, message := ReasonProgressing, "Waiting for streams to run"
		if streams != nil {
			reason, message = streams.Reason, streams.Message
		}
		setRuleProgressing(reason, message, notStalledMessage, setCondition)
	}
}

func stalledRuleCondition(resources, gitTarget, streams *metav1.Condition) *metav1.Condition {
	switch {
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
