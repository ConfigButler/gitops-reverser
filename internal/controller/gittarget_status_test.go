// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// TestGitTargetReadinessGates walks the gate table and asserts what a kstatus CONSUMER would read,
// not what the conditions happen to say. Every row goes through the real kstatus library.
func TestGitTargetReadinessGates(t *testing.T) {
	running := watch.StreamSummary{
		Total: 3, Ready: 3, Reason: watch.StreamReasonAllStreamsReady, Message: "3/3 streams running",
	}
	accepted := watch.GitPathAcceptanceStatus{Accepted: true}
	matches := watch.RenderFidelityStatus{State: git.RenderFidelityTrue}
	healthy := conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded, Message: "healthy"}

	tests := []struct {
		name            string
		streams         watch.StreamSummary
		gitPath         watch.GitPathAcceptanceStatus
		render          watch.RenderFidelityStatus
		provider        conditionValue
		clusterProvider conditionValue
		sourceReach     conditionValue
		want            kstatus.Status
		wantReadyReason string
	}{
		{
			name: "everything healthy", streams: running, gitPath: accepted, render: matches,
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.CurrentStatus, wantReadyReason: ReasonSucceeded,
		},
		{
			// F2. No WatchRule has claimed this target yet — step 3 of the documented setup flow.
			// Nothing is pending, so nothing may report as in progress.
			name:    "no resolved types is converged, not perpetually reconciling",
			streams: watch.StreamSummary{Reason: watch.StreamReasonNoResolvedTypes, Message: "0/0 streams running"},
			gitPath: accepted, render: matches,
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.CurrentStatus, wantReadyReason: ReasonSucceeded,
		},
		{
			name: "stream replaying", gitPath: accepted, render: matches,
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Replaying: 1, Reason: watch.StreamReasonReplaying,
				Message: "1/2 streams running; 1 replaying",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.InProgressStatus, wantReadyReason: watch.StreamReasonReplaying,
		},
		{
			name: "stream blocked", gitPath: accepted, render: matches,
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Blocked: 1, Reason: watch.StreamReasonWatchError,
				Message: "1/2 streams running; 1 blocked",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.FailedStatus, wantReadyReason: watch.StreamReasonWatchError,
		},
		{
			name: "Git path refused", streams: running, render: matches,
			gitPath: watch.GitPathAcceptanceStatus{
				Accepted: false,
				Reason:   GitTargetReasonUnsupportedContent,
				Message:  "Git path refused at kustomization.yaml: uses patches",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.FailedStatus, wantReadyReason: GitTargetReasonUnsupportedContent,
		},
		{
			// A write-boundary refusal is not "this folder holds content we cannot manage"; it is
			// "this edit had nowhere safe to land". It must carry its own reason through to Stalled.
			name: "write boundary refused", streams: running, render: matches,
			gitPath: watch.GitPathAcceptanceStatus{
				Accepted: false,
				Reason:   GitTargetReasonWriteBoundaryRefused,
				Message:  "Git path refused at base/deployment.yaml: write-fan-in must be 1",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.FailedStatus, wantReadyReason: GitTargetReasonWriteBoundaryRefused,
		},
		{
			name: "render diverged is terminal", streams: running, gitPath: accepted,
			render: watch.RenderFidelityStatus{
				State: git.RenderFidelityFalse, Reason: GitTargetReasonRenderDoesNotMatchLive, Message: "${REGION}",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.FailedStatus, wantReadyReason: GitTargetReasonRenderDoesNotMatchLive,
		},
		{
			name: "render rechecking is progress", streams: running, gitPath: accepted,
			render: watch.RenderFidelityStatus{
				State: git.RenderFidelityUnknown, Reason: GitTargetReasonRenderRechecking, Message: "waiting",
			},
			provider: healthy, clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.InProgressStatus, wantReadyReason: GitTargetReasonRenderRechecking,
		},
		{
			name: "unready GitProvider holds the target below Ready", streams: running,
			gitPath: accepted, render: matches,
			provider: conditionValue{
				Status: metav1.ConditionFalse, Reason: GitTargetReasonGitProviderNotReady, Message: "no repo",
			},
			clusterProvider: healthy, sourceReach: healthy,
			want: kstatus.InProgressStatus, wantReadyReason: GitTargetReasonGitProviderNotReady,
		},
		{
			// THE F1 REGRESSION. A refused Git path is terminal, and an unready provider is
			// transient. The provider gate used to run last and stamp Reconciling=True /
			// Stalled=False over the refusal, flipping the object from kstatus Failed to
			// InProgress — so `kubectl wait` and every CI gate built on kstatus waited out its
			// timeout on a target that was never going to converge.
			name: "refused path AND unready provider stays Failed", streams: running, render: matches,
			gitPath: watch.GitPathAcceptanceStatus{
				Accepted: false,
				Reason:   GitTargetReasonUnsupportedContent,
				Message:  "Git path refused at kustomization.yaml: uses patches",
			},
			provider: conditionValue{
				Status: metav1.ConditionFalse, Reason: GitTargetReasonGitProviderNotReady, Message: "no repo",
			},
			clusterProvider: healthy,
			sourceReach: conditionValue{
				Status: metav1.ConditionUnknown, Reason: "AwaitingDiscovery", Message: "not discovered yet",
			},
			want: kstatus.FailedStatus, wantReadyReason: GitTargetReasonUnsupportedContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &configbutleraiv1alpha3.GitTarget{}
			st := beginStatus(nil, nil, target, &target.Status.Conditions)
			observed := dataPlaneObservation{
				streams: tt.streams,
				axes: gitTargetAxes{
					Streams: streamsAxis(tt.streams),
					GitPath: gitPathAxis(tt.gitPath),
					Render:  renderAxis(tt.render),
				},
			}
			st.setValue(GitTargetConditionStreamsRunning, observed.axes.Streams)
			st.setValue(GitTargetConditionGitPathAccepted, observed.axes.GitPath)
			st.setValue(GitTargetConditionRenderMatchesLive, observed.axes.Render)

			rd := newGitTargetReadiness()
			gitTargetReadinessGates(rd, observed, tt.provider, tt.clusterProvider, tt.sourceReach)
			st.applyReadiness(rd)

			ready := findCondition(target.Status.Conditions, GitTargetConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tt.wantReadyReason, ready.Reason)
			assert.NotEmpty(t, ready.Message)

			computed, err := kstatus.Compute(gitTargetStatusObject(unstructuredConditions(target.Status.Conditions)))
			require.NoError(t, err)
			assert.Equal(t, tt.want, computed.Status,
				"a kstatus consumer must read %s here", tt.want)
		})
	}
}

// unstructuredConditions renders a real condition set the way kstatus.Compute consumes it, so the
// conformance assertions above run against reconciler OUTPUT rather than a hand-built fixture.
func unstructuredConditions(conditions []metav1.Condition) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(conditions))
	for _, c := range conditions {
		out = append(out, conditionMap(c.Type, string(c.Status), c.Reason, c.Message))
	}
	return out
}

// TestGitTargetStall_PublishesTerminalTrio covers the control-plane gates that return before the
// data plane is ever evaluated.
func TestGitTargetStall_PublishesTerminalTrio(t *testing.T) {
	target := &configbutleraiv1alpha3.GitTarget{}
	st := beginStatus(nil, nil, target, &target.Status.Conditions)

	rd := newGitTargetReadiness()
	rd.stalled(GitTargetReadyReasonValidationFailed, "Validated gate failed: ProviderNotFound")
	st.applyReadiness(rd)

	computed, err := kstatus.Compute(gitTargetStatusObject(unstructuredConditions(target.Status.Conditions)))
	require.NoError(t, err)
	assert.Equal(t, kstatus.FailedStatus, computed.Status)
}

// TestGitTargetRetentionStatus_AbsentAndZeroMeanDifferentThings is the whole point of the field
// being a pointer. Absent is "no resync has reported yet — the target has not replayed, or predates
// this field"; zero is "a resync ran and found nothing to retain", which is the converged signal.
// Collapsing them would leave status unable to say a mirror is converged.
func TestGitTargetRetentionStatus_AbsentAndZeroMeanDifferentThings(t *testing.T) {
	assert.Nil(t, gitTargetRetentionStatus(watch.RetentionSummary{}),
		"a target that has never reported must not publish a count of zero")

	converged := gitTargetRetentionStatus(watch.RetentionSummary{
		Reported: true, Mode: configbutleraiv1alpha3.PruneAlways,
		ObservedTime: time.Date(2026, 7, 21, 13, 20, 0, 0, time.UTC),
	})
	require.NotNil(t, converged, "a reported zero is a report")
	assert.Zero(t, converged.RetainedDocuments)
	assert.Equal(t, configbutleraiv1alpha3.PruneAlways, converged.Mode)
}

// TestGitTargetRetentionStatus_ReportsTheEffectiveMode covers the legacy GitTarget: it stores no
// spec.prune at all, so status is the only place the mode keeping its documents is visible.
func TestGitTargetRetentionStatus_ReportsTheEffectiveMode(t *testing.T) {
	observed := time.Date(2026, 7, 21, 13, 20, 0, 0, time.UTC)

	projected := gitTargetRetentionStatus(watch.RetentionSummary{
		Reported: true, Mode: configbutleraiv1alpha3.PruneOnEvent, RetainedDocuments: 3, ObservedTime: observed,
	})

	require.NotNil(t, projected)
	assert.Equal(t, int32(3), projected.RetainedDocuments)
	assert.Equal(t, configbutleraiv1alpha3.PruneOnEvent, projected.Mode)
	assert.Equal(t, observed, projected.ObservedTime.Time)
}

// TestStatusCommit_LostRaceIsRecordedSoTheCallerComesBack is the regression guard for Failure A.
//
// A status write that loses an optimistic-lock race leaves the object holding the WINNER's older
// answer. Discarding this reconcile's observation is correct — it is stale by then — but the old
// code also returned success, so the caller chose its CONVERGED requeue and nothing re-enqueued
// the object: every For() here carries a GenerationChangedPredicate, which exists to filter the
// status-only updates controllers write themselves.
//
// A 61-scope GitTarget produced ~66 reconciles in four seconds; the one that computed
// RenderMatchesLive=True lost the race, vanished, and left every WatchRule on that target reading
// "Rechecking" for five minutes.
func TestStatusCommit_LostRaceIsRecordedSoTheCallerComesBack(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1"},
	}
	conflicting := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "configbutler.ai", Resource: "gittargets"}, "acme",
					errors.New("the object has been modified"))
			},
		}).Build()

	st := beginStatus(conflicting, nil, target, &target.Status.Conditions)
	st.set(GitTargetConditionReady, metav1.ConditionTrue, ReasonSucceeded, "converged")

	require.NoError(t, st.commit(context.Background()),
		"a lost race is expected and must not be reported as a reconcile failure")
	assert.True(t, st.writeLost(),
		"the caller must be able to tell that its status never landed, or it will wait out its "+
			"converged requeue holding the loser's answer")
}

// TestStall_LostWriteComesBackPromptly is the regression guard for the flake this fix closes.
//
// The stall path — every control-plane gate that fails before the data plane is evaluated — used to
// return its gate's cadence directly and was the ONLY status path that never asked writeLost(). A
// GitTarget that stalled on "provider not validated yet", then lost the race publishing the
// recovery, therefore kept the ValidationFailed reason for the full steady interval, and anything
// waiting on it (a WatchRule mirroring Ready, a spec polling for Progressing) read the stale reason
// until then.
//
// The assertion is on the interval, not merely on "shorter than steady": the recovery has to beat
// the waits that watch this object, and borrowing a ten-second settle cadence put it in a dead heat
// with a ten-second wait.
func TestStall_LostWriteComesBackPromptly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1"},
	}
	conflicting := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "configbutler.ai", Resource: "gittargets"}, "acme",
					errors.New("the object has been modified"))
			},
		}).Build()

	reconciler := &GitTargetReconciler{Client: conflicting}
	st := beginStatus(conflicting, nil, target, &target.Status.Conditions)

	result, err := reconciler.stall(context.Background(), st, blockedGate{
		reason:  GitTargetReadyReasonValidationFailed,
		message: "Validated gate failed",
		blocked: "Blocked by Validated=False",
	})

	require.NoError(t, err)
	assert.Equal(t, RequeueWriteLostInterval, result.RequeueAfter,
		"a stalled reconcile whose status never landed must come back promptly, not on the gate cadence")
}

// TestStall_KeepsItsGateCadenceWhenTheWriteLands is the other half: the prompt requeue is for a LOST
// write only. A stall that published what it computed has nothing to retry, and must not spin.
func TestStall_KeepsItsGateCadenceWhenTheWriteLands(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).WithStatusSubresource(target).Build()

	reconciler := &GitTargetReconciler{Client: c}
	st := beginStatus(c, nil, target, &target.Status.Conditions)

	result, err := reconciler.stall(context.Background(), st, blockedGate{
		reason:  GitTargetReadyReasonValidationFailed,
		message: "Validated gate failed",
		blocked: "Blocked by Validated=False",
	})

	require.NoError(t, err)
	assert.Equal(t, RequeueSteadyInterval, result.RequeueAfter)
}

// TestStatusCommit_SuccessfulWriteIsNotFlaggedAsLost keeps the flag meaningful: an ordinary write
// must leave it clear, or every reconcile would take the shortened requeue.
func TestStatusCommit_SuccessfulWriteIsNotFlaggedAsLost(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).
		WithStatusSubresource(target).Build()

	st := beginStatus(c, nil, target, &target.Status.Conditions)
	st.set(GitTargetConditionReady, metav1.ConditionTrue, ReasonSucceeded, "converged")

	require.NoError(t, st.commit(context.Background()))
	assert.False(t, st.writeLost())
}
