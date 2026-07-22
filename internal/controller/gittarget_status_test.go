// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"

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
	})
	require.NotNil(t, converged, "a reported zero is a report")
	assert.Zero(t, converged.RetainedDocuments)
	assert.Equal(t, configbutleraiv1alpha3.PruneAlways, converged.Mode)
	require.NotNil(t, converged.ObservedTime, "a reading with no timestamp cannot be judged stale")
	assert.False(t, converged.ObservedTime.IsZero())
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
