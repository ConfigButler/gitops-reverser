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

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
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

func watchRuleStreamsStatus(streams watch.StreamSummary) *configbutleraiv1alpha2.WatchRuleStreamsStatus {
	observed := streams.ObservedTime
	if observed.IsZero() {
		observed = metav1.Now()
	}
	return &configbutleraiv1alpha2.WatchRuleStreamsStatus{
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
	streams := conditionByType(conditions, ConditionTypeStreamsRunning)
	switch {
	case resources != nil && resources.Status == metav1.ConditionFalse:
		setStalled(resources.Reason, resources.Message)
	case streams != nil && streams.Status == metav1.ConditionFalse &&
		(streams.Reason == watch.StreamReasonWatchError || streams.Reason == watch.StreamReasonWatchNotPermitted):
		setStalled(streams.Reason, streams.Message)
	case streams != nil && streams.Status == metav1.ConditionTrue:
		setCondition(ConditionTypeReady, metav1.ConditionTrue, readyReason, readyMessage)
		setCondition(ConditionTypeReconciling, metav1.ConditionFalse, readyReason, "Reconciliation complete")
		setCondition(ConditionTypeStalled, metav1.ConditionFalse, readyReason, notStalledMessage)
	default:
		reason, message := ReasonProgressing, "Waiting for streams to run"
		if streams != nil {
			reason, message = streams.Reason, streams.Message
		}
		setCondition(ConditionTypeReady, metav1.ConditionFalse, reason, message)
		setCondition(ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
		setCondition(ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	}
}
