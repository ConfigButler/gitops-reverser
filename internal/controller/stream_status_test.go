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
// Both rule kinds go through it, which is now one shared function rather than a copy each. The
// table stays because the two kinds still reach it over different objects — one namespaced, one
// cluster-scoped, each with its own status subresource — and it is the whole path from a verdict to
// a requeue that is under test, not the arithmetic in the middle.
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
		newRule func() statusObject
	}{
		{
			name: "WatchRule",
			newRule: func() statusObject {
				return &configbutleraiv1alpha3.WatchRule{
					ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: "tenant-acme", ResourceVersion: "1"},
				}
			},
		},
		{
			name: "ClusterWatchRule",
			newRule: func() statusObject {
				return &configbutleraiv1alpha3.ClusterWatchRule{
					ObjectMeta: metav1.ObjectMeta{Name: "rule", ResourceVersion: "1"},
				}
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

			rule := tc.newRule()
			landed := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rule).
				WithStatusSubresource(rule).Build()
			result, err := commitRule(context.Background(), beginStatus(landed, nil, rule), converging())
			require.NoError(t, err)
			assert.Equal(t, RequeueStreamSettleInterval, result.RequeueAfter,
				"a converging rule that published its status keeps the stream-settle loop")

			rule = tc.newRule()
			lost := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rule).
				WithInterceptorFuncs(conflict).Build()
			result, err = commitRule(context.Background(), beginStatus(lost, nil, rule), converging())
			require.NoError(t, err)
			assert.Equal(t, RequeueWriteLostInterval, result.RequeueAfter,
				"a converging rule whose status never landed must come back promptly, not on the settle loop")
		})
	}
}
