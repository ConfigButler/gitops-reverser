// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// The gitopsreverser_resource_condition recording site. The instrument's contract — its geometry,
// its label keys, and why identity labels are allowed on this one family — is documented at
// internal/telemetry/resource_condition.go; what lives here is which objects and which conditions
// go on it.

// The kinds on the condition surface. They are the five objects a HUMAN declares, which is what
// bounds the series count and is the whole basis on which §7.1 of the metrics plan allows their
// names as labels.
//
// CommitRequest is deliberately absent. It is created once per save, so it is data-shaped rather
// than config-shaped: a gauge per object would churn series continuously — created, Ready,
// finalized, deleted — and churn is the one cost Prometheus handles worst. Its state is answerable
// from git_commits_total{message_source="commit_request"} and from the object's own conditions, and
// if a fleet view is ever wanted the right shape is a counter of terminal outcomes, because those
// are events rather than state. Its reconciler does not use this status helper either, so the
// exclusion holds at both ends.
const (
	conditionKindGitTarget        = "GitTarget"
	conditionKindWatchRule        = "WatchRule"
	conditionKindClusterWatchRule = "ClusterWatchRule"
	conditionKindGitProvider      = "GitProvider"
	conditionKindClusterProvider  = "ClusterProvider"
)

// conditionMetricKind names the kind an object publishes under, or "" for one that is not on the
// surface. The type switch is the surface: a kind that is not listed cannot reach the metric, so
// the exclusion above is structural rather than a rule someone has to remember.
//
// Which is why the kind does NOT come off statusObject, the accessor interface the status session
// takes. That interface is about writing a status, and anything can be given the two methods it
// asks for; hanging the kind on it would turn this list into an implication — implement the
// accessors over in api/v1alpha3 and you are on the metric — and CommitRequest would arrive here by
// a route nobody reviewing that file would see.
func conditionMetricKind(obj client.Object) string {
	switch obj.(type) {
	case *configbutleraiv1alpha3.GitTarget:
		return conditionKindGitTarget
	case *configbutleraiv1alpha3.WatchRule:
		return conditionKindWatchRule
	case *configbutleraiv1alpha3.ClusterWatchRule:
		return conditionKindClusterWatchRule
	case *configbutleraiv1alpha3.GitProvider:
		return conditionKindGitProvider
	case *configbutleraiv1alpha3.ClusterProvider:
		return conditionKindClusterProvider
	default:
		return ""
	}
}

// conditionMetricStates renders the kstatus trio for publication, synthesizing Unknown for a
// condition the object does not carry yet.
//
// The trio and nothing else: Ready is the summary every automation already reads, Reconciling
// separates "working on it" from "given up", and Stalled is the kstatus signal that nothing will
// retry without a human. The per-kind axis conditions (GitPathAccepted, StreamsRunning,
// SourceNamespaceAuthorized, …) stay off the metric — each of them already resolves into Ready, and
// the one that failed is named by the `reason` label rather than by a series of its own.
//
// A synthesized Unknown is a published series, not an omitted one, because "this object has not
// been reconciled yet" is a state: an absent series is indistinguishable from an operator that is
// not running, and those two need opposite responses.
func conditionMetricStates(conditions []metav1.Condition) []telemetry.ResourceConditionState {
	types := []string{ConditionTypeReady, ConditionTypeReconciling, ConditionTypeStalled}

	states := make([]telemetry.ResourceConditionState, 0, len(types))
	for _, conditionType := range types {
		state := telemetry.ResourceConditionState{
			Type:   conditionType,
			Status: string(metav1.ConditionUnknown),
			Reason: reasonUnspecified,
		}
		if found := findCondition(conditions, conditionType); found != nil {
			state.Status = string(found.Status)
			state.Reason = found.Reason
			if state.Reason == "" {
				state.Reason = reasonUnspecified
			}
		}
		states = append(states, state)
	}
	return states
}

// publishConditionMetrics publishes this reconcile's conditions for the object under status.
//
// The deletion arm is Flux's IsDelete adapted to controllers that take no finalizers of their own:
// an object with a deletion timestamp and no finalizer left is going now, and the last thing this
// helper should do is publish a level for it. The definitive delete is still the NotFound reconcile
// that follows — see the call sites of telemetry.ForgetResourceConditions — because an object
// deleted while this process was down produces no reconcile here at all, and a fresh process
// publishes nothing about it either way.
func (s *reconcileStatus) publishConditionMetrics() {
	kind := conditionMetricKind(s.object)
	if kind == "" {
		return
	}
	namespace, name := s.object.GetNamespace(), s.object.GetName()

	if !s.object.GetDeletionTimestamp().IsZero() && len(s.object.GetFinalizers()) == 0 {
		telemetry.ForgetResourceConditions(kind, namespace, name)
		return
	}
	telemetry.RecordResourceConditions(kind, namespace, name, conditionMetricStates(*s.conditions))
}
