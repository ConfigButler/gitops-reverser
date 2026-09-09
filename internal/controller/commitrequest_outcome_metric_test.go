// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const commitRequestsMetric = "gitopsreverser_commit_requests_total"

// outcomeCount reads one bounded outcome's counter value, and reports whether the series exists.
func outcomeCount(t *testing.T, reader *sdkmetric.ManualReader, outcome string) (int64, bool) {
	t.Helper()
	return telemetry.CollectInt64Sum(reader, commitRequestsMetric, map[string]string{"outcome": outcome})
}

// outcomeCountForTarget reads one outcome's counter for one GitTarget identity. The empty pair is
// the documented fallback for a request whose target never resolved.
func outcomeCountForTarget(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	outcome, namespace, name string,
) (int64, bool) {
	t.Helper()
	return telemetry.CollectInt64Sum(reader, commitRequestsMetric, map[string]string{
		"outcome":             outcome,
		"gittarget_namespace": namespace,
		"gittarget_name":      name,
	})
}

// The reason the label was added: two targets' saves must be tellable apart, so an operator can
// alert on one tenant's target rather than on the fleet.
func TestCommitRequestMetric_TwoTargetsAreDistinguishable(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	for _, target := range []string{"team-a-config", "team-b-config"} {
		cr := newCommitRequest("save-" + target)
		cr.Spec.GitTargetRef = meta.LocalObjectReference{Name: target}
		c := newCommitRequestClient(t, nil, cr)
		f := &fakeFinalizer{
			result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"},
			resolved: true,
		}
		r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}
		reconcileCommitRequest(t, r, "save-"+target)
	}

	for _, target := range []string{"team-a-config", "team-b-config"} {
		value, ok := outcomeCountForTarget(t, reader, crOutcomeCommitted, "default", target)
		require.True(t, ok, "target %s published no series", target)
		assert.Equal(t, int64(1), value, "each target counts its own save, not the other's")
	}
}

// Cardinality is bounded by configuration rather than by saves: repeated saves against one target
// reuse one series. That is the property that makes the label affordable.
func TestCommitRequestMetric_RepeatedSavesReuseOneSeries(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	for i := range 3 {
		name := fmt.Sprintf("save-repeat-%d", i)
		c := newCommitRequestClient(t, nil, newCommitRequest(name))
		f := &fakeFinalizer{
			result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"},
			resolved: true,
		}
		r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}
		reconcileCommitRequest(t, r, name)
	}

	value, ok := outcomeCountForTarget(t, reader, crOutcomeCommitted, "default", "team-a-config")
	require.True(t, ok)
	assert.Equal(t, int64(3), value, "three saves, one series")
}

// The bound on the label. spec.gitTargetRef is whatever the client wrote, so a request naming a
// target that does not exist must NOT mint a series under that name: anyone with create rights on
// commitrequests could otherwise grow the metric without limit. The requested reference stays on
// the conditions and in the logs, which is where an unbounded string belongs.
func TestCommitRequestMetric_UnresolvableTargetTakesTheFallback(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	cr := newCommitRequest("save-bogus")
	cr.Spec.GitTargetRef = meta.LocalObjectReference{Name: "no-such-target"}
	// Older than the resolve timeout, so the poll gives up on this pass rather than requeueing.
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * commitRequestResolveTimeout))
	c := newCommitRequestClient(t, nil, cr)
	// A service error is what EventRouter.ServiceCommitRequest returns when the GitTarget Get fails.
	f := &fakeFinalizer{err: errors.New("get GitTarget default/no-such-target: not found")}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-bogus")

	value, ok := outcomeCountForTarget(t, reader, crOutcomeFailed, "", "")
	require.True(t, ok, "an unresolved target must still be counted, under the fallback identity")
	assert.Equal(t, int64(1), value)

	_, minted := telemetry.CollectInt64Sum(reader, commitRequestsMetric, map[string]string{
		"gittarget_name": "no-such-target",
	})
	assert.False(t, minted, "a name that resolved to nothing must never become a label value")
}

// The mapping is derived from FinalizeResult rather than from the condition reasons, so pin it
// directly. The default arm matters most: an unmapped outcome must count as failed, because a bug
// that increments nothing is a bug nobody sees.
func TestCommitRequestOutcome_MapsEveryFinalizeResult(t *testing.T) {
	tests := []struct {
		name   string
		result git.FinalizeResult
		err    error
		want   string
	}{
		{"committed", git.FinalizeResult{Outcome: git.FinalizeCommitted}, nil, crOutcomeCommitted},
		{"no open window", git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow}, nil, crOutcomeNoWindow},
		{"window mismatch", git.FinalizeResult{Outcome: git.FinalizeWindowMismatch}, nil, crOutcomeWindowMismatch},
		{"already present", git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent}, nil, crOutcomeAlreadyPresent},
		{"finalize error", git.FinalizeResult{}, errors.New("boom"), crOutcomeFailed},
		{"empty outcome, no error", git.FinalizeResult{}, nil, crOutcomeFailed},
		{"unknown outcome", git.FinalizeResult{Outcome: "SomethingNew"}, nil, crOutcomeFailed},
		{
			"an error outranks a set outcome",
			git.FinalizeResult{Outcome: git.FinalizeCommitted},
			errors.New("boom"),
			crOutcomeFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, commitRequestOutcome(tc.result, tc.err))
		})
	}
}

// Every terminal outcome must reach the counter through a real reconcile, not only through the
// mapping function. A value with no raise site is a label nobody can alert on.
func TestCommitRequestMetric_EveryOutcomeIsReachableThroughReconcile(t *testing.T) {
	tests := []struct {
		name   string
		result git.FinalizeResult
		err    error
		want   string
	}{
		{
			"committed",
			git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"},
			nil,
			crOutcomeCommitted,
		},
		{"no window", git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}, nil, crOutcomeNoWindow},
		{
			"window mismatch",
			git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"},
			nil,
			crOutcomeWindowMismatch,
		},
		{
			"already present",
			git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"},
			nil,
			crOutcomeAlreadyPresent,
		},
		{"failed", git.FinalizeResult{}, errors.New("attach failed"), crOutcomeFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			c := newCommitRequestClient(t, nil, newCommitRequest("save-outcome"))
			f := &fakeFinalizer{result: tc.result, err: tc.err, resolved: true}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

			reconcileCommitRequest(t, r, "save-outcome")

			value, ok := outcomeCount(t, reader, tc.want)
			require.True(t, ok, "outcome %q published no series", tc.want)
			assert.Equal(t, int64(1), value)
		})
	}
}

// The trap the recording site exists to avoid. applyFinalizeResultToStatus is called once per
// attempt inside writeTerminalStatus's conflict-retry loop, so recording there would over-report
// exactly the contended request. Two conflicts, one increment.
func TestCommitRequestMetric_ConflictRetriesIncrementOnce(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	conflicts := 2
	fns := interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(
					schema.GroupResource{Group: "configbutler.ai", Resource: "commitrequests"},
					obj.GetName(), errors.New("simulated"))
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
	c := newCommitRequestClient(t, &fns, withInProgress(newCommitRequest("save-conflict")))
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-conflict")

	value, ok := outcomeCount(t, reader, crOutcomeCommitted)
	require.True(t, ok)
	assert.Equal(t, int64(1), value, "two conflicts inside one invocation must still be one increment")
}

// The documented limit, asserted as behavior rather than left to be discovered. A status write that
// fails permanently is not retried into a terminal state, so the next reconcile re-decides and
// increments again. This is why the instrument promises one increment per terminal DECISION
// ATTEMPT and not one per CommitRequest; deduplicating would need a durable marker on the object.
func TestCommitRequestMetric_FailedStatusWriteThenRedeliveryCountsTwice(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	fns := interceptor.Funcs{
		SubResourceUpdate: func(
			_ context.Context,
			_ client.Client,
			_ string,
			_ client.Object,
			_ ...client.SubResourceUpdateOption,
		) error {
			return apierrors.NewInternalError(errors.New("status write is down"))
		},
	}
	c := newCommitRequestClient(t, &fns, withInProgress(newCommitRequest("save-lost")))
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-lost")
	reconcileCommitRequest(t, r, "save-lost")

	value, ok := outcomeCount(t, reader, crOutcomeCommitted)
	require.True(t, ok)
	assert.Equal(t, int64(2), value,
		"a terminal status that never persisted is re-decided on redelivery; the counter says so")
}

// refusePrunedGitTargetRef reaches a terminal state without passing through writeTerminalStatus.
// It is rare and migration-only, which is exactly the kind of terminal state that must not be
// missing from the counter.
func TestCommitRequestMetric_PrunedGitTargetRefCountsFailed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	pruned := &configv1alpha3.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "save-pruned", Namespace: "default", UID: types.UID("uid-save-pruned")},
		Spec: configv1alpha3.CommitRequestSpec{
			GitTargetRef: meta.LocalObjectReference{Name: ""},
			Message:      "save: pruned",
		},
	}
	c := newCommitRequestClient(t, nil, pruned)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-pruned")

	got := fetchCommitRequest(t, c, "save-pruned")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonGitTargetRefPruned)

	value, ok := outcomeCountForTarget(t, reader, crOutcomeFailed, "", "")
	require.True(t, ok, "the pruned-reference path must not be invisible to the counter")
	assert.Equal(t, int64(1), value, "a spec naming no GitTarget has nothing to resolve")
	require.Empty(t, f.calls, "a request naming no GitTarget must never reach the finalizer")
}

// A recreated CommitRequest is a new incarnation of the same save, and each one is its own
// decision. Counting per incarnation is what makes a rate over the counter meaningful.
func TestCommitRequestMetric_RecreatedRequestCountsPerIncarnation(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	c := newCommitRequestClient(t, nil, newCommitRequest("save-again"))
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-again")

	// Delete and recreate under a fresh UID, as a second save from the same editor would.
	first := fetchCommitRequest(t, c, "save-again")
	require.NoError(t, c.Delete(context.Background(), &first))
	recreated := newCommitRequest("save-again")
	recreated.UID = types.UID("uid-save-again-2")
	require.NoError(t, c.Create(context.Background(), recreated))

	reconcileCommitRequest(t, r, "save-again")

	value, ok := outcomeCount(t, reader, crOutcomeWindowMismatch)
	require.True(t, ok)
	assert.Equal(t, int64(2), value)
}
