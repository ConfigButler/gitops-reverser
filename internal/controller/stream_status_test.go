// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestRuleReadiness_GitTargetReadyStalledBlocksRule(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionTypeResourcesResolved, Status: metav1.ConditionTrue, Reason: "Resolved", Message: "resolved"},
		{
			Type:    ConditionTypeGitTargetReady,
			Status:  metav1.ConditionFalse,
			Reason:  GitTargetReasonUnsupportedContent,
			Message: "Git path refused at kustomization.yaml: uses patches",
		},
		{
			Type:    ConditionTypeStreamsRunning,
			Status:  metav1.ConditionTrue,
			Reason:  watch.StreamReasonAllStreamsReady,
			Message: "1/1 streams running",
		},
	}
	trio := ruleReadiness(conditions, "WatchRule", "rule ready").trio()

	assert.Equal(t, metav1.ConditionFalse, trio.Ready.Status)
	assert.Equal(t, metav1.ConditionFalse, trio.Reconciling.Status)
	assert.Equal(t, metav1.ConditionTrue, trio.Stalled.Status)
	assert.Equal(t, GitTargetReasonUnsupportedContent, trio.Stalled.Reason)
}

// TestCommitRule_LostWriteBeatsTheConvergingLoop covers the path a rule takes while it is still
// coming up, which is where a lost write hurts most: a CONVERGING rule already polls on the fast
// stream-settle loop, so returning that cadence directly looks close enough to correct and is not.
// The settle interval is for watching streams converge, not for retrying a write the API server
// rejected, and a rule stuck on the older answer is exactly what a spec waiting on its Ready reason
// reads.
//
// Both rule kinds carry their own copy of commitRule, so both are checked here: fixing one and
// leaving the other is the shape of mistake this table exists to catch.
func TestCommitRule_LostWriteBeatsTheConvergingLoop(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	conflict := interceptor.Funcs{
		SubResourcePatch: func(
			context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
		) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: "configbutler.ai", Resource: "watchrules"}, "rule",
				errors.New("the object has been modified"))
		},
	}

	// Each phase gets a FRESH object: commit() sends nothing when the status is identical to what
	// was read, so a rule carrying the conditions the previous phase already wrote would take the
	// no-op path and never reach the patch this test is about.
	tests := []struct {
		name    string
		commit  func(context.Context, *reconcileStatus, *readiness) (ctrl.Result, error)
		newRule func() (client.Object, *[]metav1.Condition)
	}{
		{
			name:   "WatchRule",
			commit: (&WatchRuleReconciler{}).commitRule,
			newRule: func() (client.Object, *[]metav1.Condition) {
				rule := &configbutleraiv1alpha3.WatchRule{
					ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: "tenant-acme", ResourceVersion: "1"},
				}
				return rule, &rule.Status.Conditions
			},
		},
		{
			name:   "ClusterWatchRule",
			commit: (&ClusterWatchRuleReconciler{}).commitRule,
			newRule: func() (client.Object, *[]metav1.Condition) {
				rule := &configbutleraiv1alpha3.ClusterWatchRule{
					ObjectMeta: metav1.ObjectMeta{Name: "rule", ResourceVersion: "1"},
				}
				return rule, &rule.Status.Conditions
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			converging := func() *readiness {
				rd := newRuleReadiness(tc.name, "")
				rd.progressing(metav1.ConditionFalse, ReasonProgressing, "streams coming up")
				return rd
			}

			rule, conditions := tc.newRule()
			landed := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rule).
				WithStatusSubresource(rule).Build()
			result, err := tc.commit(context.Background(), beginStatus(landed, nil, rule, conditions), converging())
			require.NoError(t, err)
			assert.Equal(t, RequeueStreamSettleInterval, result.RequeueAfter,
				"a converging rule that published its status keeps the stream-settle loop")

			rule, conditions = tc.newRule()
			lost := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rule).
				WithInterceptorFuncs(conflict).Build()
			result, err = tc.commit(context.Background(), beginStatus(lost, nil, rule, conditions), converging())
			require.NoError(t, err)
			assert.Equal(t, RequeueWriteLostInterval, result.RequeueAfter,
				"a converging rule whose status never landed must come back promptly, not on the settle loop")
		})
	}
}
