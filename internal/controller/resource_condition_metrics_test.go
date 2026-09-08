// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// conditionTestNamespace is the namespace every object in this file is declared in. The metric
// carries it as a label, so it is named rather than repeated as a literal at each read.
const conditionTestNamespace = "tenant-acme"

// conditionGaugeValue reads one published condition series.
func conditionGaugeValue(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	kind, name, conditionType, status string,
) (int64, bool) {
	t.Helper()
	return telemetry.CollectInt64Sum(reader, "gitopsreverser_resource_condition", map[string]string{
		"kind":               kind,
		"resource_namespace": conditionTestNamespace,
		"resource_name":      name,
		"type":               conditionType,
		"status":             status,
	})
}

// The regression this metric would otherwise ship with: commit() returns early when the computed
// status is identical to what was read, and a gauge published after that check is missing for every
// object a fresh pod finds already converged — which is most of them, most of the time. A gauge is a
// LEVEL: it has to be republished by a reconcile that changes nothing.
func TestPublishConditionMetrics_PublishesWhenTheStatusPatchIsANoOp(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	defer telemetry.ForgetResourceConditions(conditionKindGitTarget, conditionTestNamespace, "settled")

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "settled", Namespace: conditionTestNamespace, ResourceVersion: "1"},
		Status: configbutleraiv1alpha3.GitTargetStatus{
			Conditions: []metav1.Condition{
				{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded},
				{Type: ConditionTypeReconciling, Status: metav1.ConditionFalse, Reason: ReasonSucceeded},
				{Type: ConditionTypeStalled, Status: metav1.ConditionFalse, Reason: ReasonSucceeded},
			},
		},
	}

	// A nil client is safe precisely because this reconcile writes nothing: if commit() ever got as
	// far as the patch, this test would panic rather than pass for the wrong reason.
	st := beginStatus(nil, nil, target, &target.Status.Conditions)
	require.NoError(t, st.commit(context.Background()))

	value, ok := conditionGaugeValue(t, reader, conditionKindGitTarget, "settled", ConditionTypeReady, "True")
	require.True(t, ok, "a converged object must still publish its level")
	assert.Equal(t, int64(1), value)
}

// An object that carries no such condition yet publishes Unknown, not nothing. "Has not been
// reconciled yet" is a state, and an absent series is how a state becomes indistinguishable from an
// operator that is not running.
func TestPublishConditionMetrics_SynthesizesUnknown(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	defer telemetry.ForgetResourceConditions(conditionKindWatchRule, conditionTestNamespace, "fresh")

	rule := &configbutleraiv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: conditionTestNamespace, ResourceVersion: "1"},
	}
	st := beginStatus(nil, nil, rule, &rule.Status.Conditions)
	require.NoError(t, st.commit(context.Background()))

	for _, conditionType := range []string{ConditionTypeReady, ConditionTypeReconciling, ConditionTypeStalled} {
		value, ok := conditionGaugeValue(t, reader, conditionKindWatchRule, "fresh", conditionType, "Unknown")
		require.True(t, ok, conditionType)
		assert.Equal(t, int64(1), value, conditionType)
	}
}

// The last reconcile of an object that is going must drop the series rather than publish one more
// level for it. This is the deletion-timestamp arm; the definitive delete is the NotFound reconcile,
// which calls telemetry.ForgetResourceConditions directly.
func TestPublishConditionMetrics_ForgetsAnObjectThatIsGoing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	defer telemetry.ForgetResourceConditions(conditionKindGitTarget, conditionTestNamespace, "doomed")

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "doomed", Namespace: conditionTestNamespace, ResourceVersion: "1"},
		Status: configbutleraiv1alpha3.GitTargetStatus{
			Conditions: []metav1.Condition{
				{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded},
			},
		},
	}
	st := beginStatus(nil, nil, target, &target.Status.Conditions)
	require.NoError(t, st.commit(context.Background()))
	_, ok := conditionGaugeValue(t, reader, conditionKindGitTarget, "doomed", ConditionTypeReady, "True")
	require.True(t, ok)

	deleting := metav1.Now()
	target.DeletionTimestamp = &deleting
	st = beginStatus(nil, nil, target, &target.Status.Conditions)
	require.NoError(t, st.commit(context.Background()))

	_, ok = conditionGaugeValue(t, reader, conditionKindGitTarget, "doomed", ConditionTypeReady, "True")
	assert.False(t, ok, "an object with no finalizer left is going now and must stop publishing")
}

// The kind switch is the surface. CommitRequest is data-shaped — one per save — so it is excluded
// structurally rather than by a rule someone has to remember.
func TestConditionMetricKind_CoversTheFiveConfigKindsAndNothingElse(t *testing.T) {
	assert.Equal(t, conditionKindGitTarget, conditionMetricKind(&configbutleraiv1alpha3.GitTarget{}))
	assert.Equal(t, conditionKindWatchRule, conditionMetricKind(&configbutleraiv1alpha3.WatchRule{}))
	assert.Equal(t, conditionKindClusterWatchRule, conditionMetricKind(&configbutleraiv1alpha3.ClusterWatchRule{}))
	assert.Equal(t, conditionKindGitProvider, conditionMetricKind(&configbutleraiv1alpha3.GitProvider{}))
	assert.Equal(t, conditionKindClusterProvider, conditionMetricKind(&configbutleraiv1alpha3.ClusterProvider{}))
	assert.Empty(t, conditionMetricKind(&configbutleraiv1alpha3.CommitRequest{}))
}

// A condition written with an empty reason publishes a visibly wrong placeholder rather than an
// empty label, which Prometheus renders as an absent one.
func TestConditionMetricStates_EmptyReasonBecomesThePlaceholder(t *testing.T) {
	states := conditionMetricStates([]metav1.Condition{
		{Type: ConditionTypeReady, Status: metav1.ConditionFalse},
	})

	require.Len(t, states, 3)
	assert.Equal(t, telemetry.ResourceConditionState{
		Type:   ConditionTypeReady,
		Status: string(metav1.ConditionFalse),
		Reason: reasonUnspecified,
	}, states[0])
}
