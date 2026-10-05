// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func invalidObjectSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "team", Operator: metav1.LabelSelectorOpIn},
	}}
}

// assertResourcesRefused pins a rule refused for its resources: ResourcesResolved and
// StreamsRunning are False under the reason, and the rule is Stalled under it.
func assertResourcesRefused(t *testing.T, conditions []metav1.Condition, reason string) {
	t.Helper()
	for _, want := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{ConditionTypeResourcesResolved, metav1.ConditionFalse},
		{ConditionTypeReady, metav1.ConditionFalse},
		{ConditionTypeStalled, metav1.ConditionTrue},
	} {
		cond := apimeta.FindStatusCondition(conditions, want.conditionType)
		require.NotNil(t, cond, "condition %s must be published", want.conditionType)
		assert.Equal(t, want.status, cond.Status, "condition %s status", want.conditionType)
		assert.Equal(t, reason, cond.Reason, "condition %s reason", want.conditionType)
	}
}

func TestReconcile_WatchRuleWithAnInvalidObjectSelectorIsRefused(t *testing.T) {
	ctx := context.Background()
	rule := wrsnWatchRule("")
	rule.Spec.Rules[0].ObjectSelector = invalidObjectSelector()
	f := newWRSNFixture(t, []client.Object{
		wrsnGitTarget(), wrsnGitProvider(), wrsnClusterProvider(false), rule,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wrsnTenantNS}},
	})

	_, err := f.reconcile(ctx)

	require.NoError(t, err, "an invalid selector is terminal, not an error to retry")
	assert.Empty(t, f.compiledNames(), "a refused rule must leave no compiled rule behind")
	assert.Positive(t, f.wm.replans, "the watch manager must be replanned so no stream survives")
	conditions := f.reloadRule(ctx, t).Status.Conditions
	assertResourcesRefused(t, conditions, watch.ReasonInvalidObjectSelector)
	streams := apimeta.FindStatusCondition(conditions, ConditionTypeStreamsRunning)
	require.NotNil(t, streams)
	assert.Equal(t, metav1.ConditionFalse, streams.Status)
}

func TestReconcile_WatchRuleWithAConflictingObjectSelectorIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newWRSNFixture(t, wrsnBaseObjects(false, ""))
	f.wm.collectionOverlap = "WatchRule tenant-acme/repo-config-rule is refused: it selects secrets"

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	conditions := f.reloadRule(ctx, t).Status.Conditions
	assertResourcesRefused(t, conditions, watch.ReasonCollectionOverlap)
	resolved := apimeta.FindStatusCondition(conditions, ConditionTypeResourcesResolved)
	assert.Equal(t, f.wm.collectionOverlap, resolved.Message)
}

func TestReconcile_ClusterWatchRuleWithAnInvalidObjectSelectorIsRefused(t *testing.T) {
	ctx := context.Background()
	rule := cwaClusterWatchRule()
	rule.Spec.Rules[0].ObjectSelector = invalidObjectSelector()
	f := newCWAFixture(t, []client.Object{
		rule, cwaGitTarget(), cwaGitProvider(), cwaClusterProvider(cwaAdmitting()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assert.Empty(t, f.compiledNames())
	assert.Positive(t, f.wm.replans)
	assertResourcesRefused(t, f.reloadRule(ctx, t).Status.Conditions, watch.ReasonInvalidObjectSelector)
}

func TestReconcile_ClusterWatchRuleWithAConflictingObjectSelectorIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newCWAFixture(t, []client.Object{
		cwaClusterWatchRule(), cwaGitTarget(), cwaGitProvider(), cwaClusterProvider(cwaAdmitting()),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cwaTargetNS}},
	}, interceptor.Funcs{})
	f.wm.collectionOverlap = "ClusterWatchRule mirror-everything is refused"

	_, err := f.reconcile(ctx)

	require.NoError(t, err)
	assertResourcesRefused(t, f.reloadRule(ctx, t).Status.Conditions, watch.ReasonCollectionOverlap)
}
