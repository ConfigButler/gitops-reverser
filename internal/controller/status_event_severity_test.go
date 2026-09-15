// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Severity is read off Stalled, not off "Ready is not True".
//
// The two are different questions, and conflating them is what made the severity stop carrying
// information: a stream still replaying after a restart, or a rule waiting on a GitTarget that is
// still coming up, announced itself as a Warning. On a healthy cluster essentially every Warning
// was then the system working, so a real block arrived looking exactly like the routine ones.
func TestRecordReadyTransition_SeverityFollowsStalled(t *testing.T) {
	for name, tc := range map[string]struct {
		contribute func(*readiness)
		want       string
	}{
		// kstatus InProgress. It clears on its own; nobody needs to be paged.
		"a progressing gate is Normal": {
			contribute: func(rd *readiness) {
				rd.progressing(metav1.ConditionFalse, ReasonProgressing, "Waiting for streams to run")
			},
			want: corev1.EventTypeNormal,
		},
		// Unknown is the other honest way to not be ready yet, and it is equally not a failure.
		"an unestablished gate is Normal": {
			contribute: func(rd *readiness) {
				rd.progressing(metav1.ConditionUnknown, ReasonProgressing, "Nothing observed yet")
			},
			want: corev1.EventTypeNormal,
		},
		// kstatus Failed. Waiting changes nothing, which is exactly what a Warning should mean.
		"a stalled gate is a Warning": {
			contribute: func(rd *readiness) {
				rd.stalled(GitTargetReasonBranchNotAllowed, "branch is not allowed")
			},
			want: corev1.EventTypeWarning,
		},
		"reaching Ready is Normal": {
			contribute: func(*readiness) {},
			want:       corev1.EventTypeNormal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(4)
			st := newSeverityTestStatus(t, recorder)

			rd := newReadiness("converged", "GitTarget is not stalled")
			tc.contribute(rd)
			st.applyReadiness(rd)
			require.NoError(t, st.commit(context.Background()))

			assert.Equal(t, tc.want, eventTypeOf(t, recorder))
		})
	}
}

// The conservative arm. Nothing in this package publishes Ready without the trio — every
// beginStatus caller goes through applyReadiness — so this pins the behaviour for a future one
// that does not: it stays loud rather than being silently downgraded to Normal.
func TestRecordReadyTransition_ReadyWithoutTheTrioStaysAWarning(t *testing.T) {
	recorder := record.NewFakeRecorder(4)
	st := newSeverityTestStatus(t, recorder)

	st.set(ConditionTypeReady, metav1.ConditionFalse, ReasonProgressing, "no trio written")
	require.NoError(t, st.commit(context.Background()))

	assert.Equal(t, corev1.EventTypeWarning, eventTypeOf(t, recorder))
}

func newSeverityTestStatus(t *testing.T, recorder record.EventRecorder) *reconcileStatus {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "acme", Namespace: "tenant-acme", ResourceVersion: "1", Generation: 1,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(target).WithStatusSubresource(target).Build()
	return beginStatus(c, recorder, target)
}

// eventTypeOf reads the leading word of the recorded event, which is where FakeRecorder puts the
// type ("Normal Succeeded converged").
func eventTypeOf(t *testing.T, recorder *record.FakeRecorder) string {
	t.Helper()

	select {
	case got := <-recorder.Events:
		require.NotEmpty(t, got)
		return strings.Fields(got)[0]
	default:
		t.Fatal("expected exactly one Event for the persisted Ready transition")
		return ""
	}
}
