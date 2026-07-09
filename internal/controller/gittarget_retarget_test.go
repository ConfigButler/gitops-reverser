// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func targetAt(branch, path string) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov"},
			Branch:      branch,
			Path:        path,
		},
	}
}

// materializedAtMainApps stamps the destination the GitTarget's content currently lives at.
// Every fixture here materialized at main:apps first; the spec is what moves.
func materializedAtMainApps(target *configbutleraiv1alpha3.GitTarget) *configbutleraiv1alpha3.GitTarget {
	target.Status.ObservedDestination = &configbutleraiv1alpha3.GitTargetDestination{Branch: "main", Path: "apps"}
	return target
}

func TestDestinationMoved(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		target *configbutleraiv1alpha3.GitTarget
		want   bool
	}{
		"never materialized": {
			// Absent observedDestination is not a move: the target has not arrived anywhere.
			target: targetAt("main", "apps"),
			want:   false,
		},
		"settled": {
			target: materializedAtMainApps(targetAt("main", "apps")),
			want:   false,
		},
		"path changed": {
			target: materializedAtMainApps(targetAt("main", "clusters/acme")),
			want:   true,
		},
		"branch changed": {
			target: materializedAtMainApps(targetAt("live", "apps")),
			want:   true,
		},
		"trailing slash is not a move": {
			// spec.path normalizes a trailing slash away, so "apps/" and "apps" name one folder.
			target: materializedAtMainApps(targetAt("main", "apps/")),
			want:   false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, destinationMoved(tc.target))
		})
	}
}

// The teardown is generation-scoped, not a plain "is Retargeting true?", so that a second
// destination change arriving mid-retarget tears down again instead of quietly continuing
// to build the first one.
func TestRetargetAlreadyTornDown(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("live", "apps"))
	assert.False(t, retargetAlreadyTornDown(target), "no Retargeting condition yet")

	r := &GitTargetReconciler{}
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionTrue,
		GitTargetReasonDestinationChanged, "moving")
	assert.True(t, retargetAlreadyTornDown(target))

	// A second spec change bumps the generation, which makes the recorded teardown stale.
	target.Generation = 2
	assert.False(t, retargetAlreadyTornDown(target),
		"a destination change arriving mid-retarget must tear down again")

	// A False condition is never a completed teardown, whatever its generation.
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
		GitTargetReasonDestinationSettled, "settled")
	assert.False(t, retargetAlreadyTornDown(target))
}

func TestBeginRetarget_MarksRetargetingAndClearsPushTime(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	now := metav1.Now()
	target.Status.LastPushTime = &now

	r := &GitTargetReconciler{}
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, GitTargetReasonDestinationChanged, c.Reason)
	assert.Contains(t, c.Message, "main:apps", "the abandoned folder is named")
	assert.Contains(t, c.Message, "main:clusters/acme", "and so is the new one")
	assert.Contains(t, c.Message, "left in place",
		"the operator must be told the old folder is not deleted")

	assert.Nil(t, target.Status.LastPushTime, "the push time belonged to the abandoned folder")
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path,
		"observedDestination still names the folder being abandoned until the move settles")
}

func TestSettleDestination_FirstMaterialization(t *testing.T) {
	t.Parallel()

	target := targetAt("main", "apps")
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	require.NotNil(t, target.Status.ObservedDestination)
	assert.Equal(t, "main", target.Status.ObservedDestination.Branch)
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, GitTargetReasonDestinationSettled, c.Reason)
	assert.NotContains(t, c.Message, "abandoned", "a first materialization abandons nothing")
}

func TestSettleDestination_CompletedRetargetNamesTheAbandonedFolder(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("live", "clusters/acme"))
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	assert.Equal(t, "live", target.Status.ObservedDestination.Branch)
	assert.Equal(t, "clusters/acme", target.Status.ObservedDestination.Path)

	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Contains(t, c.Message, "main:apps was abandoned")
	assert.Contains(t, c.Message, "remove it by hand")
}

// The path is normalized on the way into status, so a settled target never looks moved to
// the next reconcile just because the user wrote a trailing slash.
func TestSettleDestination_NormalizesThePath(t *testing.T) {
	t.Parallel()

	target := targetAt("main", "apps/")
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)
	assert.False(t, destinationMoved(target))
}

// A control-plane gate that blocks the reconcile has not put the old materialization back,
// so an in-flight retarget keeps reporting True.
func TestMarkRetargetingUnknown_KeepsAnInFlightRetarget(t *testing.T) {
	t.Parallel()

	r := &GitTargetReconciler{}

	inFlight := materializedAtMainApps(targetAt("live", "apps"))
	require.NoError(t, r.beginRetarget(t.Context(), inFlight, logr.Discard()))
	r.markRetargetingUnknown(inFlight, "Blocked by Validated=False")
	assert.Equal(t, metav1.ConditionTrue,
		conditionByType(inFlight.Status.Conditions, GitTargetConditionRetargeting).Status)

	settled := materializedAtMainApps(targetAt("main", "apps"))
	r.markRetargetingUnknown(settled, "Blocked by Validated=False")
	c := conditionByType(settled.Status.Conditions, GitTargetConditionRetargeting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, GitTargetReasonNotChecked, c.Reason)
}

func TestDestinationString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "main:apps",
		destinationString(configbutleraiv1alpha3.GitTargetDestination{Branch: "main", Path: "apps"}))
}

// The abandoned-folder message is the only place in status where an operator learns which
// folder is now unmanaged Git content they may want to remove. A later reconcile must not
// overwrite it with a steady-state message.
func TestSettleDestination_AbandonedMessageSurvivesLaterReconciles(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	r := &GitTargetReconciler{}

	r.settleDestination(target, logr.Discard())
	first := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting).Message
	require.Contains(t, first, "main:apps was abandoned")

	// Two more steady-state reconciles, exactly as the 5-minute requeue produces.
	r.settleDestination(target, logr.Discard())
	r.settleDestination(target, logr.Discard())

	after := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting).Message
	assert.Equal(t, first, after,
		"a periodic reconcile must not erase the name of the folder the retarget abandoned")
	assert.Equal(t, "clusters/acme", target.Status.ObservedDestination.Path)
}

func TestSettleDestination_ReSettlesAfterANewRetarget(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	r := &GitTargetReconciler{}
	r.settleDestination(target, logr.Discard())

	// A second move: teardown, then settle. The message must name the freshly abandoned
	// folder, not the one from the first move.
	target.Generation = 2
	target.Spec.Path = "clusters/beta"
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))
	r.settleDestination(target, logr.Discard())

	msg := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting).Message
	assert.Contains(t, msg, "main:clusters/acme was abandoned")
	assert.NotContains(t, msg, "main:apps")
	assert.Equal(t, "clusters/beta", target.Status.ObservedDestination.Path)
}

func TestDestinationAlreadySettled(t *testing.T) {
	t.Parallel()

	target := targetAt("main", "apps")
	assert.False(t, destinationAlreadySettled(target))

	r := &GitTargetReconciler{}
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionTrue,
		GitTargetReasonDestinationChanged, "moving")
	assert.False(t, destinationAlreadySettled(target))

	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
		GitTargetReasonDestinationSettled, "settled")
	assert.True(t, destinationAlreadySettled(target))
}

// recordedEvent is one Kubernetes event the reconciler emitted.
type recordedEvent struct{ reason, message string }

type fakeRecorder struct{ events []recordedEvent }

func (f *fakeRecorder) Event(_ runtime.Object, _, reason, message string) {
	f.events = append(f.events, recordedEvent{reason, message})
}

func (f *fakeRecorder) Eventf(_ runtime.Object, _, reason, messageFmt string, args ...any) {
	f.events = append(f.events, recordedEvent{reason, fmt.Sprintf(messageFmt, args...)})
}

func (f *fakeRecorder) AnnotatedEventf(
	_ runtime.Object, _ map[string]string, _, reason, messageFmt string, args ...any,
) {
	f.events = append(f.events, recordedEvent{reason, fmt.Sprintf(messageFmt, args...)})
}

func (f *fakeRecorder) reasons() []string {
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.reason)
	}
	return out
}

// status.retargetingTo makes a move self-describing, and is cleared once it settles.
func TestRetargetingTo_TracksTheMoveAndClearsOnSettle(t *testing.T) {
	t.Parallel()

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	r := &GitTargetReconciler{}

	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))
	require.NotNil(t, target.Status.RetargetingTo)
	assert.Equal(t, "clusters/acme", target.Status.RetargetingTo.Path)

	r.settleDestination(target, logr.Discard())
	assert.Nil(t, target.Status.RetargetingTo, "the move is over")
}

// A destination change arriving mid-move leaves the first move's partially-built folder
// behind. observedDestination still names the ORIGINAL folder, so without this event nothing
// would ever name the intermediate one.
func TestBeginRetarget_NamesTheFolderASupersededMoveAbandoned(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	r := &GitTargetReconciler{Recorder: recorder}

	// main:apps -> main:intermediate, still building.
	target := materializedAtMainApps(targetAt("main", "intermediate"))
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))
	assert.Empty(t, recorder.events, "the first move abandons only observedDestination, named at settle")

	// A second change before the first settles.
	target.Generation = 2
	target.Spec.Path = "final"
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))

	require.Equal(t, []string{GitTargetEventRetargetSuperseded}, recorder.reasons())
	assert.Contains(t, recorder.events[0].message, "main:intermediate")
	assert.Contains(t, recorder.events[0].message, "main:final")
	assert.Equal(t, "final", target.Status.RetargetingTo.Path)

	// The original folder is still the one observedDestination names, and settle reports it.
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)
	r.settleDestination(target, logr.Discard())
	assert.Contains(t,
		conditionByType(target.Status.Conditions, GitTargetConditionRetargeting).Message,
		"main:apps was abandoned")
	require.Equal(t,
		[]string{GitTargetEventRetargetSuperseded, GitTargetEventRetargeted}, recorder.reasons())
}

// Reverting a destination mid-move never passes through beginRetarget — spec agrees with
// observedDestination again — but the folder the abandoned move began building is still there.
func TestSettleDestination_NamesTheFolderARevertedMoveAbandoned(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	r := &GitTargetReconciler{Recorder: recorder}

	target := materializedAtMainApps(targetAt("main", "elsewhere"))
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))
	require.Empty(t, recorder.events)

	// The operator changes their mind and puts the path back.
	target.Generation = 2
	target.Spec.Path = "apps"
	require.False(t, destinationMoved(target), "spec agrees with observedDestination again")

	r.settleDestination(target, logr.Discard())

	require.Equal(t, []string{GitTargetEventRetargetSuperseded}, recorder.reasons())
	assert.Contains(t, recorder.events[0].message, "main:elsewhere")
	assert.Nil(t, target.Status.RetargetingTo)
	assert.Equal(t, "apps", target.Status.ObservedDestination.Path)
}

// A move that settles where it said it was going supersedes nothing.
func TestSettleDestination_CompletedMoveEmitsOnlyRetargeted(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	r := &GitTargetReconciler{Recorder: recorder}

	target := materializedAtMainApps(targetAt("main", "clusters/acme"))
	require.NoError(t, r.beginRetarget(t.Context(), target, logr.Discard()))
	r.settleDestination(target, logr.Discard())

	assert.Equal(t, []string{GitTargetEventRetargeted}, recorder.reasons())
}
